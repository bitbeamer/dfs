package mount

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

type writeTransaction struct {
	path        string
	stagingPath string
	refs        int
	dirty       bool
	created     bool
	inode       uint64
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

	stagingDirectory := filepath.Join(f.root, ".dfs", "staging")
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
	if !exists {
		placeholder, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
		if err != nil {
			_ = os.Remove(stagingPath)
			return nil, err
		}
		if err := placeholder.Close(); err != nil {
			_ = os.Remove(stagingPath)
			return nil, err
		}
		if stagedInfo, err := os.Stat(stagingPath); err == nil {
			if attr := attrFromInfo(stagedInfo); attr != nil {
				inode = attr.Ino
			}
		}
	}

	return &writeTransaction{
		path: path, stagingPath: stagingPath, dirty: !exists || flags&syscall.O_TRUNC != 0,
		created: !exists, inode: inode,
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
		delete(f.writes, transaction.path)
		if transaction.dirty {
			err = f.publishTransaction(transaction)
		} else {
			err = os.Remove(transaction.stagingPath)
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
	if final && transaction.dirty {
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
	if transaction.inode != 0 {
		attr.Ino = transaction.inode
	}
	f.attrsMu.Lock()
	f.attrs[transaction.path] = visibleState{attr: *attr, signature: signature}
	f.attrsMu.Unlock()
	return nil
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
	if code := s.File.Flush(); code != fuse.OK {
		return code
	}
	return s.finish()
}

func (s *stagedFile) Release() {
	s.File.Release()
	s.finish()
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
	defer f.attrsMu.Unlock()
	visible, ok := f.attrs[path]
	if !ok {
		return
	}
	if locked {
		target, err := os.Readlink(f.full(path))
		if err != nil || !strings.Contains(target, "--"+visible.signature) {
			delete(f.attrs, path)
			return
		}
	}
	attr.Ino = visible.attr.Ino
	attr.Size = visible.attr.Size
	attr.Blocks = visible.attr.Blocks
	attr.Mtime = visible.attr.Mtime
	attr.Mtimensec = visible.attr.Mtimensec
	attr.Ctime = visible.attr.Ctime
	attr.Ctimensec = visible.attr.Ctimensec
	attr.Mode = visible.attr.Mode
	attr.Owner = visible.attr.Owner
}

func (f *FileSystem) renameVisible(oldPath, newPath string) {
	f.attrsMu.Lock()
	defer f.attrsMu.Unlock()
	if visible, ok := f.attrs[oldPath]; ok {
		f.attrs[newPath] = visible
		delete(f.attrs, oldPath)
	}
}

func (f *FileSystem) removeVisible(path string) {
	f.attrsMu.Lock()
	delete(f.attrs, path)
	f.attrsMu.Unlock()
}

func (f *FileSystem) captureVisible(path string) {
	info, err := os.Stat(f.full(path))
	if err != nil {
		return
	}
	attr := attrFromInfo(info)
	if attr == nil {
		return
	}
	f.attrsMu.Lock()
	visible := f.attrs[path]
	visible.attr = *attr
	f.attrs[path] = visible
	f.attrsMu.Unlock()
}
