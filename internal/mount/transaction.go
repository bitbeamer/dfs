package mount

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/store"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

type writeTransaction struct {
	path               string
	stagingPath        string
	recordPath         string
	refs               int
	dirty              bool
	created            bool
	unlinked           bool
	destinationExisted bool
	failure            fuse.Status
	inode              uint64
}

type visibleState struct {
	attr      fuse.Attr
	signature string
}

type stagedFile struct {
	nodefs.File
	filesystem  *FileSystem
	transaction *writeTransaction
	path        string
	finishOnce  sync.Once
	finishCode  fuse.Status
}

func attrFromInfo(info os.FileInfo) *fuse.Attr {
	stat := fuse.ToStatT(info)
	if stat == nil {
		return nil
	}
	attr := &fuse.Attr{}
	attr.FromStat(stat)
	return attr
}

func (f *FileSystem) openStaged(name string, flags uint32, mode os.FileMode, create bool) (nodefs.File, error) {
	path := clean(name)
	destination := f.full(path)
	if annexSymlink(destination) {
		if _, err := os.Stat(destination); err != nil {
			if err := f.hydrate(path); err != nil {
				return nil, err
			}
		}
	}

	f.writesMu.Lock()
	defer f.writesMu.Unlock()

	transaction := f.writes[path]
	if transaction == nil {
		var err error
		transaction, err = f.createTransaction(path, flags, mode, create)
		if err != nil {
			return nil, err
		}
		f.writes[path] = transaction
	} else if flags&syscall.O_TRUNC != 0 {
		if err := os.Truncate(transaction.stagingPath, 0); err != nil {
			return nil, err
		}
		transaction.dirty = true
	}

	accessMode := int(flags & syscall.O_ACCMODE)
	handle, err := os.OpenFile(transaction.stagingPath, accessMode, 0)
	if err != nil {
		if transaction.refs == 0 {
			delete(f.writes, path)
			_ = os.Remove(transaction.stagingPath)
			_ = os.Remove(transaction.recordPath)
			if transaction.created {
				_ = os.Remove(f.full(transaction.path))
			}
		}
		return nil, err
	}
	transaction.refs++
	if f.notifier != nil {
		f.notifier.BeginWrite()
	}
	f.repo.Touch(path)
	f.logger.Debug("staged write opened", "path", path, "staging_path", transaction.stagingPath, "flags", flags)
	return &stagedFile{
		File: nodefs.NewLoopbackFile(handle), filesystem: f,
		transaction: transaction, path: path, finishCode: fuse.OK,
	}, nil
}

func (f *FileSystem) createTransaction(path string, flags uint32, mode os.FileMode, create bool) (*writeTransaction, error) {
	destination := f.full(path)
	info, statErr := os.Stat(destination)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	}
	if !exists && !create {
		return nil, os.ErrNotExist
	}
	if exists && create && flags&syscall.O_EXCL != 0 {
		return nil, os.ErrExist
	}
	f.attrsMu.Lock()
	previousVisible, hasPreviousVisible := f.attrs[path]
	f.attrsMu.Unlock()
	if mode == 0 {
		if hasPreviousVisible {
			mode = os.FileMode(previousVisible.attr.Mode).Perm()
		} else if exists {
			// git-annex deliberately freezes object files read-only. The
			// mounted file remains writable, so do not copy that internal
			// owner-write restriction into the staging file.
			mode = info.Mode().Perm() | 0o200
		} else {
			mode = 0o644
		}
	}

	stagingDirectory := filepath.Join(f.root, filepath.FromSlash(config.Directory), "staging")
	if err := os.MkdirAll(stagingDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	staging, err := os.CreateTemp(stagingDirectory, "write-*")
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}
	stagingPath := staging.Name()
	cleanup := func() {
		_ = staging.Close()
		_ = os.Remove(stagingPath)
	}
	if err := staging.Chmod(mode.Perm()); err != nil {
		cleanup()
		return nil, err
	}
	if exists && flags&syscall.O_TRUNC == 0 {
		source, err := os.Open(destination)
		if err != nil {
			cleanup()
			return nil, err
		}
		_, copyErr := io.Copy(staging, source)
		closeErr := source.Close()
		if copyErr != nil {
			cleanup()
			return nil, copyErr
		}
		if closeErr != nil {
			cleanup()
			return nil, closeErr
		}
		if err := os.Chtimes(stagingPath, info.ModTime(), info.ModTime()); err != nil {
			cleanup()
			return nil, err
		}
	}
	if err := staging.Close(); err != nil {
		_ = os.Remove(stagingPath)
		return nil, err
	}

	inode := uint64(0)
	if hasPreviousVisible {
		inode = previousVisible.attr.Ino
	}
	if inode == 0 && exists {
		if lstat, err := os.Lstat(destination); err == nil {
			if attr := attrFromInfo(lstat); attr != nil {
				inode = attr.Ino
			}
		}
	}
	recordPath, err := persistTransactionRecord(f.root, transactionRecord{
		Path: path, Staging: filepath.Base(stagingPath), DestinationExisted: exists,
	})
	if err != nil {
		_ = os.Remove(stagingPath)
		return nil, fmt.Errorf("record write transaction: %w", err)
	}
	if !exists {
		placeholder, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
		if err != nil {
			_ = os.Remove(stagingPath)
			_ = os.Remove(recordPath)
			return nil, err
		}
		if err := placeholder.Close(); err != nil {
			_ = os.Remove(stagingPath)
			_ = os.Remove(destination)
			_ = os.Remove(recordPath)
			return nil, err
		}
		if stagedInfo, err := os.Stat(stagingPath); err == nil {
			if attr := attrFromInfo(stagedInfo); attr != nil {
				inode = attr.Ino
			}
		}
	}

	return &writeTransaction{
		path: path, stagingPath: stagingPath, recordPath: recordPath, dirty: !exists || flags&syscall.O_TRUNC != 0,
		created: !exists, destinationExisted: exists, inode: inode,
	}, nil
}

func (f *FileSystem) openStagedRead(path string) (nodefs.File, bool, error) {
	f.writesMu.Lock()
	defer f.writesMu.Unlock()
	transaction := f.writes[path]
	if transaction == nil {
		return nil, false, nil
	}
	handle, err := os.Open(transaction.stagingPath)
	if err != nil {
		return nil, true, err
	}
	return nodefs.NewLoopbackFile(handle), true, nil
}

func (f *FileSystem) markDirty(transaction *writeTransaction) {
	f.writesMu.Lock()
	transaction.dirty = true
	f.writesMu.Unlock()
}

func (f *FileSystem) finishWrite(transaction *writeTransaction, path string) fuse.Status {
	f.writesMu.Lock()
	transaction.refs--
	final := transaction.refs == 0
	var err error
	if final {
		if f.writes[transaction.path] == transaction {
			delete(f.writes, transaction.path)
		}
		if transaction.failure != fuse.OK {
			err = fmt.Errorf("write transaction failed before publication: %s", transaction.failure)
		} else if transaction.unlinked || !transaction.dirty {
			err = errors.Join(os.Remove(transaction.stagingPath), os.Remove(transaction.recordPath))
		} else {
			err = f.publishTransaction(transaction)
		}
	}
	f.writesMu.Unlock()

	if f.notifier != nil {
		f.notifier.EndWrite()
	}
	if err != nil {
		f.logger.Error("staged write publish failed", "path", path, "staging_path", transaction.stagingPath, "error", err)
		return status(err)
	}
	if final && transaction.dirty && !transaction.unlinked {
		f.logger.Info("write transaction committed", "path", path)
		f.changed("completed write", "path", path)
	}
	return fuse.OK
}

func (f *FileSystem) publishTransaction(transaction *writeTransaction) error {
	staging, err := os.Open(transaction.stagingPath)
	if err != nil {
		return err
	}
	if err := staging.Sync(); err != nil {
		_ = staging.Close()
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, staging); err != nil {
		_ = staging.Close()
		return err
	}
	info, err := staging.Stat()
	if err != nil {
		_ = staging.Close()
		return err
	}
	attr := attrFromInfo(info)
	if attr == nil {
		_ = staging.Close()
		return fmt.Errorf("read attributes for staged file %s", transaction.path)
	}
	if err := staging.Close(); err != nil {
		return err
	}
	signature := fmt.Sprintf("%x", hash.Sum(nil))
	destination := f.full(transaction.path)
	if err := os.Rename(transaction.stagingPath, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("persist published transaction: %w", err)
	}
	if err := os.Remove(transaction.recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove completed transaction record: %w", err)
	}
	if err := syncDirectory(filepath.Dir(transaction.recordPath)); err != nil {
		return fmt.Errorf("persist completed transaction record removal: %w", err)
	}
	if transaction.inode != 0 {
		attr.Ino = transaction.inode
	}
	return f.saveVisible(transaction.path, attr, signature)
}

func (s *stagedFile) finish() fuse.Status {
	s.finishOnce.Do(func() {
		s.finishCode = s.filesystem.finishWrite(s.transaction, s.path)
	})
	return s.finishCode
}

func (s *stagedFile) Write(data []byte, off int64) (uint32, fuse.Status) {
	written, code := s.File.Write(data, off)
	if code == fuse.OK && written > 0 {
		s.filesystem.markDirty(s.transaction)
	}
	return written, code
}

func (s *stagedFile) Truncate(size uint64) fuse.Status {
	code := s.File.Truncate(size)
	if code == fuse.OK {
		s.filesystem.markDirty(s.transaction)
	}
	return code
}

func (s *stagedFile) Chmod(mode uint32) fuse.Status {
	code := s.File.Chmod(mode)
	if code == fuse.OK {
		s.filesystem.markDirty(s.transaction)
	}
	return code
}

func (s *stagedFile) Chown(uid, gid uint32) fuse.Status {
	code := s.File.Chown(uid, gid)
	if code == fuse.OK {
		s.filesystem.markDirty(s.transaction)
	}
	return code
}

func (s *stagedFile) Utimens(atime, mtime *time.Time) fuse.Status {
	code := s.File.Utimens(atime, mtime)
	if code == fuse.OK {
		s.filesystem.markDirty(s.transaction)
	}
	return code
}

func (s *stagedFile) Allocate(off, size uint64, mode uint32) fuse.Status {
	code := s.File.Allocate(off, size, mode)
	if code == fuse.OK {
		s.filesystem.markDirty(s.transaction)
	}
	return code
}

func (s *stagedFile) GetAttr(out *fuse.Attr) fuse.Status {
	code := s.File.GetAttr(out)
	if code == fuse.OK && s.transaction.inode != 0 {
		out.Ino = s.transaction.inode
	}
	return code
}

func (s *stagedFile) Flush() fuse.Status {
	code := s.File.Flush()
	if code != fuse.OK {
		s.filesystem.markFailure(s.transaction, code)
	}
	return code
}

func (s *stagedFile) Release() {
	s.File.Release()
	s.finish()
}

func (s *stagedFile) Fsync(flags int) fuse.Status {
	if code := s.File.Fsync(flags); code != fuse.OK {
		s.filesystem.markFailure(s.transaction, code)
		return code
	}
	err := s.filesystem.checkpointTransaction(s.transaction)
	code := status(err)
	if code != fuse.OK {
		s.filesystem.markFailure(s.transaction, code)
	}
	return code
}

func (f *FileSystem) markFailure(transaction *writeTransaction, code fuse.Status) {
	f.writesMu.Lock()
	if transaction.failure == fuse.OK {
		transaction.failure = code
	}
	f.writesMu.Unlock()
}

func (f *FileSystem) checkpointTransaction(transaction *writeTransaction) error {
	f.writesMu.Lock()
	defer f.writesMu.Unlock()
	if !transaction.dirty || transaction.unlinked {
		return nil
	}
	source, err := os.Open(transaction.stagingPath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	attr := attrFromInfo(info)
	if attr == nil {
		return fmt.Errorf("read fsync checkpoint attributes for %s", transaction.path)
	}
	checkpoint, err := os.CreateTemp(filepath.Dir(transaction.stagingPath), "checkpoint-*")
	if err != nil {
		return err
	}
	checkpointPath := checkpoint.Name()
	cleanup := func() {
		_ = checkpoint.Close()
		_ = os.Remove(checkpointPath)
	}
	if err := checkpoint.Chmod(info.Mode().Perm()); err != nil {
		cleanup()
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(checkpoint, hash), source); err != nil {
		cleanup()
		return err
	}
	accessTime := time.Unix(int64(attr.Atime), int64(attr.Atimensec))
	if err := os.Chtimes(checkpointPath, accessTime, info.ModTime()); err != nil {
		cleanup()
		return err
	}
	if err := checkpoint.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := checkpoint.Close(); err != nil {
		_ = os.Remove(checkpointPath)
		return err
	}
	destination := f.full(transaction.path)
	if err := os.Rename(checkpointPath, destination); err != nil {
		_ = os.Remove(checkpointPath)
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("persist fsync checkpoint: %w", err)
	}
	if transaction.inode != 0 {
		attr.Ino = transaction.inode
	}
	if err := f.saveVisible(transaction.path, attr, fmt.Sprintf("%x", hash.Sum(nil))); err != nil {
		return err
	}
	transaction.destinationExisted = true
	recordPath, err := persistTransactionRecord(f.root, transactionRecord{
		Path: transaction.path, Staging: filepath.Base(transaction.stagingPath), DestinationExisted: true,
	})
	if err != nil {
		return fmt.Errorf("record fsync checkpoint: %w", err)
	}
	transaction.recordPath = recordPath
	return nil
}

func (f *FileSystem) unlinkWrite(path string) {
	f.writesMu.Lock()
	defer f.writesMu.Unlock()
	if transaction := f.writes[path]; transaction != nil {
		transaction.unlinked = true
		delete(f.writes, path)
	}
}

func (f *FileSystem) renameWrite(oldPath, newPath string) error {
	f.writesMu.Lock()
	defer f.writesMu.Unlock()
	sources := make(map[string]*writeTransaction)
	for path, transaction := range f.writes {
		if path == oldPath || strings.HasPrefix(path, oldPath+"/") {
			sources[path] = transaction
		}
	}
	for path, transaction := range f.writes {
		if path != newPath && !strings.HasPrefix(path, newPath+"/") {
			continue
		}
		if _, moving := sources[path]; moving {
			continue
		}
		transaction.unlinked = true
		delete(f.writes, path)
	}
	if len(sources) == 0 {
		return nil
	}
	for path := range sources {
		delete(f.writes, path)
	}
	for old, source := range sources {
		destination := newPath + strings.TrimPrefix(old, oldPath)
		source.path = destination
		recordPath, err := persistTransactionRecord(f.root, transactionRecord{
			Path: destination, Staging: filepath.Base(source.stagingPath), DestinationExisted: source.destinationExisted,
		})
		if err != nil {
			for _, transaction := range sources {
				transaction.unlinked = true
			}
			return fmt.Errorf("record renamed write transaction: %w", err)
		}
		source.recordPath = recordPath
		f.writes[destination] = source
	}
	return nil
}

func (f *FileSystem) stagedAttr(path string) (*fuse.Attr, bool) {
	f.writesMu.Lock()
	transaction := f.writes[path]
	if transaction == nil {
		f.writesMu.Unlock()
		return nil, false
	}
	info, err := os.Stat(transaction.stagingPath)
	inode := transaction.inode
	f.writesMu.Unlock()
	if err != nil {
		return nil, false
	}
	attr := attrFromInfo(info)
	if attr == nil {
		return nil, false
	}
	if inode != 0 {
		attr.Ino = inode
	}
	return attr, true
}

func (f *FileSystem) applyVisibleAttr(path string, attr *fuse.Attr, locked bool) {
	f.attrsMu.Lock()
	visible, ok := f.attrs[path]
	f.attrsMu.Unlock()
	if !ok && f.repo.Store != nil {
		metadata, found, err := f.repo.Store.FileMetadata(path)
		if err == nil && found {
			visible = visibleState{signature: metadata.Signature}
			visible.attr.Mode = metadata.Mode
			visible.attr.Uid, visible.attr.Gid = metadata.UID, metadata.GID
			visible.attr.Atime, visible.attr.Atimensec = joinTime(metadata.AtimeNS)
			visible.attr.Mtime, visible.attr.Mtimensec = joinTime(metadata.MtimeNS)
			visible.attr.Ctime, visible.attr.Ctimensec = joinTime(metadata.CtimeNS)
			f.attrsMu.Lock()
			f.attrs[path] = visible
			f.attrsMu.Unlock()
			ok = true
		}
	}
	if !ok {
		return
	}
	if locked {
		target, err := os.Readlink(f.full(path))
		if err != nil || !strings.Contains(target, "--"+visible.signature) {
			f.attrsMu.Lock()
			delete(f.attrs, path)
			f.attrsMu.Unlock()
			return
		}
	}
	if visible.attr.Ino != 0 {
		attr.Ino = visible.attr.Ino
	}
	if visible.attr.Size != 0 || attr.Size == 0 {
		attr.Size = visible.attr.Size
		attr.Blocks = visible.attr.Blocks
	}
	attr.Mtime = visible.attr.Mtime
	attr.Mtimensec = visible.attr.Mtimensec
	attr.Ctime = visible.attr.Ctime
	attr.Ctimensec = visible.attr.Ctimensec
	attr.Mode = visible.attr.Mode
	attr.Owner = visible.attr.Owner
}

func (f *FileSystem) renameVisible(oldPath, newPath string) {
	if oldPath == newPath {
		return
	}
	f.attrsMu.Lock()
	for path := range f.attrs {
		if path == newPath || strings.HasPrefix(path, newPath+"/") {
			delete(f.attrs, path)
		}
	}
	moved := make(map[string]visibleState)
	for path, visible := range f.attrs {
		if path == oldPath || strings.HasPrefix(path, oldPath+"/") {
			moved[newPath+strings.TrimPrefix(path, oldPath)] = visible
			delete(f.attrs, path)
		}
	}
	for path, visible := range moved {
		f.attrs[path] = visible
	}
	f.attrsMu.Unlock()
	f.annexInodesMu.Lock()
	for candidate := range f.annexInodes {
		if candidate == newPath || strings.HasPrefix(candidate, newPath+"/") {
			delete(f.annexInodes, candidate)
		}
	}
	movedInodes := make(map[string]uint64)
	for candidate, inode := range f.annexInodes {
		if candidate == oldPath || strings.HasPrefix(candidate, oldPath+"/") {
			movedInodes[newPath+strings.TrimPrefix(candidate, oldPath)] = inode
			delete(f.annexInodes, candidate)
		}
	}
	for candidate, inode := range movedInodes {
		f.annexInodes[candidate] = inode
	}
	f.annexInodesMu.Unlock()
}

func (f *FileSystem) removeVisible(path string) {
	f.attrsMu.Lock()
	for candidate := range f.attrs {
		if candidate == path || strings.HasPrefix(candidate, path+"/") {
			delete(f.attrs, candidate)
		}
	}
	f.attrsMu.Unlock()
	f.annexInodesMu.Lock()
	for candidate := range f.annexInodes {
		if candidate == path || strings.HasPrefix(candidate, path+"/") {
			delete(f.annexInodes, candidate)
		}
	}
	f.annexInodesMu.Unlock()
}

func (f *FileSystem) captureVisible(path string) error {
	target := f.full(path)
	if transaction := f.writeAt(path); transaction != nil {
		target = transaction.stagingPath
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	attr := attrFromInfo(info)
	if attr == nil {
		return fmt.Errorf("read visible attributes for %s", path)
	}
	return f.saveVisible(path, attr, f.visibleSignature(path))
}

func (f *FileSystem) writeAt(path string) *writeTransaction {
	f.writesMu.Lock()
	defer f.writesMu.Unlock()
	return f.writes[path]
}

func (f *FileSystem) visibleSignature(path string) string {
	f.attrsMu.Lock()
	defer f.attrsMu.Unlock()
	return f.attrs[path].signature
}

func (f *FileSystem) saveVisible(path string, attr *fuse.Attr, signature string) error {
	f.attrsMu.Lock()
	f.attrs[path] = visibleState{attr: *attr, signature: signature}
	f.attrsMu.Unlock()
	if f.repo.Store == nil {
		return nil
	}
	return f.repo.Store.SaveFileMetadata(path, store.FileMetadata{
		Mode: attr.Mode, UID: attr.Uid, GID: attr.Gid,
		AtimeNS: combineTime(attr.Atime, attr.Atimensec), MtimeNS: combineTime(attr.Mtime, attr.Mtimensec),
		CtimeNS: combineTime(attr.Ctime, attr.Ctimensec), Signature: signature,
	})
}

func combineTime(seconds uint64, nanoseconds uint32) int64 {
	return int64(seconds)*int64(time.Second) + int64(nanoseconds)
}

func joinTime(nanoseconds int64) (uint64, uint32) {
	return uint64(nanoseconds / int64(time.Second)), uint32(nanoseconds % int64(time.Second))
}
