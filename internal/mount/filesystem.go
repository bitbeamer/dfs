package mount

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/core"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
)

type changeNotifier interface {
	Notify(reason string)
	BeginWrite()
	EndWrite()
}

type contentInvalidator interface {
	InvalidateContent(path string)
}

// FileSystem is only a protocol adapter. All namespace, metadata, content,
// mutation, and private-state decisions belong to core.API.
type FileSystem struct {
	pathfs.FileSystem
	core             core.API
	lifetime         context.Context
	notifier         changeNotifier
	logger           *slog.Logger
	writesMu         sync.Mutex
	writes           map[string]*writeSession
	cacheInvalidator contentInvalidator
}

type writeSession struct {
	mu          sync.Mutex
	transaction core.WriteTransaction
	path        string
	attributes  core.Attributes
	refs        int
	dirty       bool
	created     bool
	removed     bool
	failure     error
}

type adapterFile struct {
	nodefs.File
	filesystem *FileSystem
	session    *writeSession
	writable   bool
	release    sync.Once
}

type readFile struct {
	nodefs.File
	handle     core.ReadHandle
	filesystem *FileSystem
	path       string
	closed     atomic.Bool
}

type versionedReadFile struct {
	*readFile
	mu sync.Mutex
}

func NewFileSystem(api core.API, notifier changeNotifier, logger *slog.Logger) *FileSystem {
	return NewFileSystemWithContext(context.Background(), api, notifier, logger)
}

func NewFileSystemWithContext(ctx context.Context, api core.API, notifier changeNotifier, logger *slog.Logger) *FileSystem {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &FileSystem{FileSystem: pathfs.NewDefaultFileSystem(), core: api, lifetime: ctx,
		notifier: notifier, logger: logger, writes: make(map[string]*writeSession)}
}

func status(err error) fuse.Status {
	if err == nil {
		return fuse.OK
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fuse.ToStatus(errno)
	}
	var coreError *core.Error
	if errors.As(err, &coreError) {
		switch coreError.Code {
		case core.CodeCanceled:
			if errors.Is(err, context.DeadlineExceeded) {
				return fuse.ToStatus(syscall.ETIMEDOUT)
			}
			return fuse.EINTR
		case core.CodeNotFound:
			return fuse.ENOENT
		case core.CodeAlreadyExists:
			return fuse.ToStatus(syscall.EEXIST)
		case core.CodePermission:
			return fuse.EACCES
		case core.CodeInvalid:
			return fuse.EINVAL
		case core.CodeConflict:
			return fuse.EBUSY
		case core.CodeNoSpace:
			return fuse.ToStatus(syscall.ENOSPC)
		case core.CodeNotSupported:
			return fuse.ENOSYS
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fuse.ToStatus(syscall.ETIMEDOUT)
	}
	if errors.Is(err, context.Canceled) {
		return fuse.EINTR
	}
	return fuse.EIO
}

func (f *FileSystem) changed(reason string, attributes ...any) {
	f.logger.Info("filesystem changed", append([]any{"operation", reason}, attributes...)...)
	if f.notifier != nil {
		f.notifier.Notify(reason)
	}
}

func (f *FileSystem) withMutation(operation func() error) fuse.Status {
	if f.notifier != nil {
		f.notifier.BeginWrite()
		defer f.notifier.EndWrite()
	}
	return status(operation())
}

func fuseAttr(attributes core.Attributes) *fuse.Attr {
	return &fuse.Attr{Mode: attributes.Mode, Owner: fuse.Owner{Uid: attributes.UID, Gid: attributes.GID},
		Size: uint64(max(attributes.Size, 0)), Ino: attributes.Inode, Blocks: uint64(max(attributes.Blocks, 0)),
		Atime: uint64(attributes.Accessed.Unix()), Atimensec: uint32(attributes.Accessed.Nanosecond()),
		Mtime: uint64(attributes.Modified.Unix()), Mtimensec: uint32(attributes.Modified.Nanosecond()),
		Ctime: uint64(attributes.Changed.Unix()), Ctimensec: uint32(attributes.Changed.Nanosecond())}
}

func dirMode(kind core.Kind, mode uint32) uint32 {
	if mode != 0 {
		return mode
	}
	switch kind {
	case core.KindDirectory:
		return syscall.S_IFDIR
	case core.KindSymlink:
		return syscall.S_IFLNK
	default:
		return syscall.S_IFREG
	}
}

func (f *FileSystem) session(path string) *writeSession {
	f.writesMu.Lock()
	defer f.writesMu.Unlock()
	return f.writes[path]
}

func (f *FileSystem) GetAttr(name string, _ *fuse.Context) (*fuse.Attr, fuse.Status) {
	if hiddenPath(name) {
		return nil, fuse.ENOENT
	}
	path, err := logicalPath(name)
	if err != nil {
		return nil, status(err)
	}
	if session := f.session(path); session != nil {
		session.mu.Lock()
		defer session.mu.Unlock()
		if !session.removed {
			return fuseAttr(session.attributes), fuse.OK
		}
	}
	entry, err := f.core.Lookup(f.lifetime, path)
	if err != nil {
		return nil, status(err)
	}
	return fuseAttr(entry.Attributes), fuse.OK
}

func logicalPath(name string) (string, error) {
	name = filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if name == "." {
		return "", nil
	}
	name = filepath.ToSlash(name)
	if filepath.IsAbs(filepath.FromSlash(name)) {
		name = name[1:]
	}
	return name, nil
}

func hiddenPath(name string) bool {
	name = strings.Trim(filepath.ToSlash(name), "/")
	for name != "" {
		component := name
		if index := strings.IndexByte(name, '/'); index >= 0 {
			component, name = name[:index], name[index+1:]
		} else {
			name = ""
		}
		if component == ".git" {
			return true
		}
	}
	return false
}

func (f *FileSystem) OpenDir(name string, _ *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	if hiddenPath(name) {
		return nil, fuse.ENOENT
	}
	path, err := logicalPath(name)
	if err != nil {
		return nil, status(err)
	}
	page, err := f.core.ReadDirectory(f.lifetime, path, core.PageRequest{})
	if err != nil {
		return nil, status(err)
	}
	f.writesMu.Lock()
	if len(f.writes) == 0 {
		f.writesMu.Unlock()
		result := make([]fuse.DirEntry, len(page.Entries))
		for index, entry := range page.Entries {
			result[index] = fuse.DirEntry{Name: entry.Name, Mode: dirMode(entry.Kind, entry.Mode), Ino: entry.Inode}
		}
		return result, fuse.OK
	}
	byName := make(map[string]fuse.DirEntry, len(page.Entries))
	for _, entry := range page.Entries {
		byName[entry.Name] = fuse.DirEntry{Name: entry.Name, Mode: dirMode(entry.Kind, entry.Mode), Ino: entry.Inode}
	}
	for candidate, session := range f.writes {
		if filepath.ToSlash(filepath.Dir(candidate)) != emptyAsDot(path) {
			continue
		}
		session.mu.Lock()
		if !session.removed {
			name := filepath.Base(candidate)
			byName[name] = fuse.DirEntry{Name: name, Mode: dirMode(session.attributes.Kind, session.attributes.Mode), Ino: session.attributes.Inode}
		}
		session.mu.Unlock()
	}
	f.writesMu.Unlock()
	result := make([]fuse.DirEntry, 0, len(byName))
	for _, entry := range byName {
		result = append(result, entry)
	}
	sortDirEntries(result)
	return result, fuse.OK
}

func emptyAsDot(path string) string {
	if path == "" {
		return "."
	}
	return path
}

func sortDirEntries(entries []fuse.DirEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].Name < entries[j-1].Name; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

func (f *FileSystem) Open(name string, flags uint32, _ *fuse.Context) (nodefs.File, fuse.Status) {
	if hiddenPath(name) {
		return nil, fuse.ENOENT
	}
	path, err := logicalPath(name)
	if err != nil {
		return nil, status(err)
	}
	writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	if writable || f.session(path) != nil {
		return f.openWrite(path, flags, false)
	}
	handle, err := f.core.OpenRead(f.lifetime, path)
	if err != nil {
		return nil, status(err)
	}
	file := &readFile{File: nodefs.NewDefaultFile(), handle: handle, filesystem: f, path: path}
	if handle.DirectIO() {
		return &nodefs.WithFlags{File: &versionedReadFile{readFile: file}, FuseFlags: fuse.FOPEN_DIRECT_IO}, fuse.OK
	}
	return file, fuse.OK
}

func (f *FileSystem) Create(name string, flags, mode uint32, _ *fuse.Context) (nodefs.File, fuse.Status) {
	path, err := logicalPath(name)
	if err != nil {
		return nil, status(err)
	}
	file, code := f.openWrite(path, flags, true, mode)
	if code == fuse.OK {
		f.logger.Info("file created", "path", path)
	}
	return file, code
}

func (f *FileSystem) openWrite(path string, flags uint32, create bool, modes ...uint32) (nodefs.File, fuse.Status) {
	f.writesMu.Lock()
	if session := f.writes[path]; session != nil {
		session.mu.Lock()
		session.refs++
		writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
		if writable && flags&syscall.O_TRUNC != 0 {
			size := int64(0)
			err := session.transaction.Truncate(0)
			if err == nil {
				session.attributes.Size = size
				session.dirty = true
			}
			session.mu.Unlock()
			f.writesMu.Unlock()
			return &adapterFile{File: nodefs.NewDefaultFile(), filesystem: f, session: session, writable: writable}, status(err)
		}
		session.mu.Unlock()
		f.writesMu.Unlock()
		return &adapterFile{File: nodefs.NewDefaultFile(), filesystem: f, session: session, writable: writable}, fuse.OK
	}
	mode := uint32(0o644)
	if len(modes) > 0 {
		mode = modes[0]
	}
	request := core.WriteRequest{Path: path, Mode: mode, Create: create, Exclusive: create && flags&syscall.O_EXCL != 0, Truncate: flags&syscall.O_TRUNC != 0}
	transaction, err := f.core.BeginWrite(f.lifetime, request)
	if err != nil {
		f.writesMu.Unlock()
		return nil, status(err)
	}
	attributes := core.Attributes{Kind: core.KindFile, Mode: syscall.S_IFREG | mode&0o7777, Modified: time.Now(), Changed: time.Now(), Accessed: time.Now()}
	if !create {
		if entry, lookupErr := f.core.Lookup(f.lifetime, path); lookupErr == nil {
			attributes = entry.Attributes
		}
	}
	if request.Truncate {
		attributes.Size = 0
	}
	session := &writeSession{transaction: transaction, path: path, attributes: attributes, refs: 1, dirty: create || request.Truncate, created: create}
	f.writes[path] = session
	f.writesMu.Unlock()
	if f.notifier != nil {
		f.notifier.BeginWrite()
	}
	return &adapterFile{File: nodefs.NewDefaultFile(), filesystem: f, session: session, writable: true}, fuse.OK
}

func (r *readFile) Read(destination []byte, offset int64) (fuse.ReadResult, fuse.Status) {
	n, err := r.handle.ReadAt(destination, offset)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return nil, status(err)
	}
	return fuse.ReadResultData(destination[:n]), fuse.OK
}

func (r *readFile) GetAttr(out *fuse.Attr) fuse.Status {
	attributes := core.Attributes{Kind: core.KindFile, Mode: syscall.S_IFREG | 0o644, Size: r.handle.Size()}
	if entry, err := r.filesystem.core.Lookup(r.filesystem.lifetime, r.path); err == nil {
		attributes = entry.Attributes
	} else if !core.IsErrorCode(err, core.CodeNotFound) {
		return status(err)
	}
	attributes.Size = r.handle.Size()
	*out = *fuseAttr(attributes)
	return fuse.OK
}
func (r *readFile) Release() {
	if r.closed.CompareAndSwap(false, true) {
		_ = r.handle.Close()
	}
}

func (r *versionedReadFile) Read(destination []byte, offset int64) (fuse.ReadResult, fuse.Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readFile.Read(destination, offset)
}

func (r *versionedReadFile) GetAttr(out *fuse.Attr) fuse.Status {
	version, err := r.filesystem.core.ContentVersion(r.filesystem.lifetime, r.path)
	if err != nil && !core.IsErrorCode(err, core.CodeNotFound) {
		return status(err)
	}
	changed := false
	r.mu.Lock()
	if err == nil && version != r.handle.Version() {
		replacement, openErr := r.filesystem.core.OpenRead(r.filesystem.lifetime, r.path)
		if openErr != nil {
			r.mu.Unlock()
			return status(openErr)
		}
		previous := r.handle
		r.handle = replacement
		_ = previous.Close()
		changed = true
	}
	r.mu.Unlock()
	if changed && r.filesystem.cacheInvalidator != nil {
		r.filesystem.cacheInvalidator.InvalidateContent(r.path)
	}
	return r.readFile.GetAttr(out)
}

func (r *versionedReadFile) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readFile.Release()
}

func fuseToCoreLock(lock *fuse.FileLock) core.FileLock {
	kind := core.LockUnlocked
	switch lock.Typ {
	case syscall.F_RDLCK:
		kind = core.LockRead
	case syscall.F_WRLCK:
		kind = core.LockWrite
	}
	return core.FileLock{Start: lock.Start, End: lock.End, Kind: kind, PID: lock.Pid}
}

func coreToFuseLock(lock core.FileLock, output *fuse.FileLock) {
	typeValue := uint32(syscall.F_UNLCK)
	if lock.Kind == core.LockRead {
		typeValue = syscall.F_RDLCK
	} else if lock.Kind == core.LockWrite {
		typeValue = syscall.F_WRLCK
	}
	*output = fuse.FileLock{Start: lock.Start, End: lock.End, Typ: typeValue, Pid: lock.PID}
}

func getFileLock(api core.API, ctx context.Context, path string, owner uint64, lock, output *fuse.FileLock) fuse.Status {
	result, err := api.GetLock(ctx, path, owner, fuseToCoreLock(lock))
	if err == nil {
		coreToFuseLock(result, output)
	}
	return status(err)
}

func setFileLock(api core.API, ctx context.Context, path string, owner uint64, lock *fuse.FileLock, wait bool) fuse.Status {
	err := api.SetLock(ctx, path, owner, fuseToCoreLock(lock), wait)
	if core.IsErrorCode(err, core.CodeConflict) {
		return fuse.ToStatus(syscall.EAGAIN)
	}
	return status(err)
}

func (r *readFile) GetLk(owner uint64, lock *fuse.FileLock, _ uint32, output *fuse.FileLock) fuse.Status {
	return getFileLock(r.filesystem.core, r.filesystem.lifetime, r.path, owner, lock, output)
}

func (r *readFile) SetLk(owner uint64, lock *fuse.FileLock, _ uint32) fuse.Status {
	return setFileLock(r.filesystem.core, r.filesystem.lifetime, r.path, owner, lock, false)
}

func (r *readFile) SetLkw(owner uint64, lock *fuse.FileLock, _ uint32) fuse.Status {
	return setFileLock(r.filesystem.core, r.filesystem.lifetime, r.path, owner, lock, true)
}

func (a *adapterFile) Read(destination []byte, offset int64) (fuse.ReadResult, fuse.Status) {
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	n, err := a.session.transaction.ReadAt(destination, offset)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return nil, status(err)
	}
	return fuse.ReadResultData(destination[:n]), fuse.OK
}

func (a *adapterFile) GetLk(owner uint64, lock *fuse.FileLock, _ uint32, output *fuse.FileLock) fuse.Status {
	return getFileLock(a.filesystem.core, a.filesystem.lifetime, a.session.path, owner, lock, output)
}

func (a *adapterFile) SetLk(owner uint64, lock *fuse.FileLock, _ uint32) fuse.Status {
	return setFileLock(a.filesystem.core, a.filesystem.lifetime, a.session.path, owner, lock, false)
}

func (a *adapterFile) SetLkw(owner uint64, lock *fuse.FileLock, _ uint32) fuse.Status {
	return setFileLock(a.filesystem.core, a.filesystem.lifetime, a.session.path, owner, lock, true)
}

func (a *adapterFile) Write(data []byte, offset int64) (uint32, fuse.Status) {
	if !a.writable {
		return 0, fuse.EBADF
	}
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	n, err := a.session.transaction.WriteAt(data, offset)
	if n > 0 {
		a.session.dirty = true
		if end := offset + int64(n); end > a.session.attributes.Size {
			a.session.attributes.Size = end
			a.session.attributes.Blocks = (end + 511) / 512
		}
		a.session.attributes.Modified, a.session.attributes.Changed = time.Now(), time.Now()
	}
	if err != nil && a.session.failure == nil {
		a.session.failure = err
	}
	return uint32(n), status(err)
}

func (a *adapterFile) Truncate(size uint64) fuse.Status {
	value := int64(size)
	return a.changeAttributes(core.AttributeChanges{Size: &value})
}

func (a *adapterFile) Chmod(mode uint32) fuse.Status {
	return a.changeAttributes(core.AttributeChanges{Mode: &mode})
}

func (a *adapterFile) Chown(uid, gid uint32) fuse.Status {
	changes := core.AttributeChanges{}
	if uid != ^uint32(0) {
		changes.UID = &uid
	}
	if gid != ^uint32(0) {
		changes.GID = &gid
	}
	return a.changeAttributes(changes)
}

func (a *adapterFile) Utimens(accessed, modified *time.Time) fuse.Status {
	return a.changeAttributes(core.AttributeChanges{Accessed: accessed, Modified: modified})
}

func (a *adapterFile) changeAttributes(changes core.AttributeChanges) fuse.Status {
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	err := a.session.transaction.SetAttributes(changes)
	if err == nil {
		a.session.dirty = true
		applyAttributeChanges(&a.session.attributes, changes)
	} else if a.session.failure == nil {
		a.session.failure = err
	}
	return status(err)
}

func applyAttributeChanges(attributes *core.Attributes, changes core.AttributeChanges) {
	if changes.Mode != nil {
		attributes.Mode = attributes.Mode&syscall.S_IFMT | *changes.Mode&0o7777
	}
	if changes.UID != nil {
		attributes.UID = *changes.UID
	}
	if changes.GID != nil {
		attributes.GID = *changes.GID
	}
	if changes.Size != nil {
		attributes.Size = *changes.Size
		attributes.Blocks = (*changes.Size + 511) / 512
	}
	if changes.Accessed != nil {
		attributes.Accessed = *changes.Accessed
	}
	if changes.Modified != nil {
		attributes.Modified = *changes.Modified
	}
	attributes.Changed = time.Now()
}

func (a *adapterFile) Allocate(offset, size uint64, mode uint32) fuse.Status {
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	err := a.session.transaction.Allocate(int64(offset), int64(size), mode)
	if err == nil {
		a.session.dirty = true
		if end := int64(offset + size); end > a.session.attributes.Size {
			a.session.attributes.Size, a.session.attributes.Blocks = end, (end+511)/512
		}
	} else if a.session.failure == nil {
		a.session.failure = err
	}
	return status(err)
}

func (a *adapterFile) GetAttr(out *fuse.Attr) fuse.Status {
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	*out = *fuseAttr(a.session.attributes)
	return fuse.OK
}

func (a *adapterFile) Flush() fuse.Status {
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	return status(a.session.failure)
}

func (a *adapterFile) Fsync(int) fuse.Status {
	a.session.mu.Lock()
	defer a.session.mu.Unlock()
	if a.session.failure != nil || !a.session.dirty {
		return status(a.session.failure)
	}
	if err := a.session.transaction.Commit(a.filesystem.lifetime); err != nil {
		a.session.failure = err
		return status(err)
	}
	a.filesystem.changed("fsync", "path", a.session.path)
	replacement, err := a.filesystem.core.BeginWrite(a.filesystem.lifetime, core.WriteRequest{Path: a.session.path})
	if err != nil {
		a.session.failure = err
		return status(err)
	}
	a.session.transaction = replacement
	a.session.created, a.session.dirty = false, false
	return fuse.OK
}

func (a *adapterFile) Release() { a.release.Do(func() { a.filesystem.releaseWrite(a.session) }) }

func (f *FileSystem) releaseWrite(session *writeSession) {
	f.writesMu.Lock()
	session.mu.Lock()
	session.refs--
	final := session.refs == 0
	if !final {
		session.mu.Unlock()
		f.writesMu.Unlock()
		return
	}
	var err error
	if session.removed || session.failure != nil || !session.dirty {
		err = session.transaction.Abort(context.Background())
	} else {
		commitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = session.transaction.Commit(commitCtx)
		cancel()
	}
	path, changed := session.path, err == nil && session.dirty && !session.removed
	if f.writes[session.path] == session {
		delete(f.writes, session.path)
	}
	session.mu.Unlock()
	f.writesMu.Unlock()
	if f.notifier != nil {
		f.notifier.EndWrite()
	}
	if err != nil {
		f.logger.Error("write transaction failed", "path", path, "error", err)
	} else if changed {
		f.logger.Info("write transaction committed", "path", path)
		f.changed("completed write", "path", path)
	}
}

func (f *FileSystem) Truncate(name string, size uint64, _ *fuse.Context) fuse.Status {
	path, err := logicalPath(name)
	if err != nil {
		return status(err)
	}
	if session := f.session(path); session != nil {
		session.mu.Lock()
		value := int64(size)
		err = session.transaction.SetAttributes(core.AttributeChanges{Size: &value})
		if err == nil {
			session.dirty = true
			applyAttributeChanges(&session.attributes, core.AttributeChanges{Size: &value})
		}
		session.mu.Unlock()
		return status(err)
	}
	return f.withMutation(func() error {
		transaction, err := f.core.BeginWrite(f.lifetime, core.WriteRequest{Path: path})
		if err != nil {
			return err
		}
		if err = transaction.Truncate(int64(size)); err != nil {
			_ = transaction.Abort(context.Background())
			return err
		}
		if err = transaction.Commit(f.lifetime); err == nil {
			f.changed("truncate", "path", path)
		}
		return err
	})
}

func (f *FileSystem) Mkdir(name string, mode uint32, _ *fuse.Context) fuse.Status {
	path, err := logicalPath(name)
	if err != nil {
		return status(err)
	}
	code := f.withMutation(func() error { return f.core.MakeDirectory(f.lifetime, path, mode, "") })
	if code == fuse.OK {
		f.changed("mkdir", "path", path)
	}
	return code
}

func (f *FileSystem) Mknod(name string, mode, device uint32, _ *fuse.Context) fuse.Status {
	path, err := logicalPath(name)
	if err != nil {
		return status(err)
	}
	code := f.withMutation(func() error { return f.core.MakeNode(f.lifetime, path, mode, device, "") })
	if code == fuse.OK {
		f.changed("mknod", "path", path)
	}
	return code
}

func (f *FileSystem) Rename(oldName, newName string, _ *fuse.Context) fuse.Status {
	oldPath, err := logicalPath(oldName)
	if err != nil {
		return status(err)
	}
	newPath, err := logicalPath(newName)
	if err != nil {
		return status(err)
	}
	if oldPath == newPath {
		return fuse.OK
	}
	code := f.withMutation(func() error {
		f.writesMu.Lock()
		defer f.writesMu.Unlock()
		var moved []*writeSession
		for path, session := range f.writes {
			if path == oldPath || stringsHasPathPrefix(path, oldPath) {
				moved = append(moved, session)
			}
		}
		for _, session := range moved {
			target := newPath + session.path[len(oldPath):]
			if existing := f.writes[target]; existing != nil && existing != session {
				return syscall.EBUSY
			}
		}
		renameErr := f.core.Rename(f.lifetime, oldPath, newPath, "")
		if renameErr != nil && !(core.IsErrorCode(renameErr, core.CodeNotFound) && len(moved) > 0) {
			return renameErr
		}
		for _, session := range moved {
			session.mu.Lock()
			oldSessionPath := session.path
			newSessionPath := newPath + oldSessionPath[len(oldPath):]
			if err := session.transaction.Rename(newSessionPath); err != nil {
				session.mu.Unlock()
				return err
			}
			delete(f.writes, oldSessionPath)
			session.path = newSessionPath
			f.writes[newSessionPath] = session
			session.mu.Unlock()
		}
		return nil
	})
	if code == fuse.OK {
		f.changed("rename", "old_path", oldPath, "new_path", newPath)
	}
	return code
}

func stringsHasPathPrefix(path, prefix string) bool {
	return len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix)] == '/'
}

func (f *FileSystem) remove(name string, directory bool) fuse.Status {
	path, err := logicalPath(name)
	if err != nil {
		return status(err)
	}
	code := f.withMutation(func() error {
		f.writesMu.Lock()
		defer f.writesMu.Unlock()
		if directory {
			for candidate := range f.writes {
				if stringsHasPathPrefix(candidate, path) {
					return syscall.ENOTEMPTY
				}
			}
		}
		if session := f.writes[path]; session != nil {
			session.mu.Lock()
			session.removed = true
			session.mu.Unlock()
			delete(f.writes, path)
			err := f.core.Remove(f.lifetime, path, directory, "")
			if core.IsErrorCode(err, core.CodeNotFound) && session.created {
				return nil
			}
			return err
		}
		return f.core.Remove(f.lifetime, path, directory, "")
	})
	if code == fuse.OK {
		f.changed("remove", "path", path)
	}
	return code
}

func (f *FileSystem) Unlink(name string, _ *fuse.Context) fuse.Status { return f.remove(name, false) }
func (f *FileSystem) Rmdir(name string, _ *fuse.Context) fuse.Status  { return f.remove(name, true) }

func (f *FileSystem) Link(oldName, newName string, _ *fuse.Context) fuse.Status {
	oldPath, err := logicalPath(oldName)
	if err != nil {
		return status(err)
	}
	newPath, err := logicalPath(newName)
	if err != nil {
		return status(err)
	}
	code := f.withMutation(func() error { return f.core.Link(f.lifetime, oldPath, newPath, "") })
	if code == fuse.OK {
		f.changed("link", "old_path", oldPath, "new_path", newPath)
	}
	return code
}

func (f *FileSystem) Symlink(value, linkName string, _ *fuse.Context) fuse.Status {
	path, err := logicalPath(linkName)
	if err != nil {
		return status(err)
	}
	code := f.withMutation(func() error { return f.core.Symlink(f.lifetime, value, path, "") })
	if code == fuse.OK {
		f.changed("symlink", "path", path)
	}
	return code
}

func (f *FileSystem) Readlink(name string, _ *fuse.Context) (string, fuse.Status) {
	path, err := logicalPath(name)
	if err != nil {
		return "", status(err)
	}
	value, err := f.core.ReadLink(f.lifetime, path)
	return value, status(err)
}

func (f *FileSystem) setAttributes(name string, changes core.AttributeChanges, reason string) fuse.Status {
	path, err := logicalPath(name)
	if err != nil {
		return status(err)
	}
	if session := f.session(path); session != nil {
		session.mu.Lock()
		err = session.transaction.SetAttributes(changes)
		if err == nil {
			session.dirty = true
			applyAttributeChanges(&session.attributes, changes)
		}
		session.mu.Unlock()
		return status(err)
	}
	code := f.withMutation(func() error { _, err := f.core.SetAttributes(f.lifetime, path, changes, ""); return err })
	if code == fuse.OK {
		f.changed(reason, "path", path)
	}
	return code
}

func (f *FileSystem) Chmod(name string, mode uint32, _ *fuse.Context) fuse.Status {
	return f.setAttributes(name, core.AttributeChanges{Mode: &mode}, "chmod")
}

func (f *FileSystem) Chown(name string, uid, gid uint32, _ *fuse.Context) fuse.Status {
	changes := core.AttributeChanges{}
	if uid != ^uint32(0) {
		changes.UID = &uid
	}
	if gid != ^uint32(0) {
		changes.GID = &gid
	}
	return f.setAttributes(name, changes, "chown")
}

func (f *FileSystem) Utimens(name string, accessed, modified *time.Time, _ *fuse.Context) fuse.Status {
	return f.setAttributes(name, core.AttributeChanges{Accessed: accessed, Modified: modified}, "utimens")
}

func (f *FileSystem) GetXAttr(name, attribute string, _ *fuse.Context) ([]byte, fuse.Status) {
	if hiddenPath(name) {
		return nil, fuse.ENOENT
	}
	path, err := logicalPath(name)
	if err != nil {
		return nil, status(err)
	}
	value, err := f.core.GetXattr(f.lifetime, path, attribute)
	if core.IsErrorCode(err, core.CodeNotFound) {
		return nil, fuse.ENOATTR
	}
	return value, status(err)
}

func (f *FileSystem) ListXAttr(name string, _ *fuse.Context) ([]string, fuse.Status) {
	if hiddenPath(name) {
		return nil, fuse.ENOENT
	}
	path, err := logicalPath(name)
	if err != nil {
		return nil, status(err)
	}
	names, err := f.core.ListXattrs(f.lifetime, path)
	return names, status(err)
}

func (f *FileSystem) SetXAttr(name, attribute string, data []byte, flags int, _ *fuse.Context) fuse.Status {
	path, err := logicalPath(name)
	if err != nil {
		return status(err)
	}
	code := f.withMutation(func() error {
		return f.core.SetXattr(f.lifetime, path, attribute, append([]byte(nil), data...), core.XattrFlags(flags), "")
	})
	if code == fuse.OK {
		f.changed("setxattr", "path", path)
	}
	return code
}

func (f *FileSystem) RemoveXAttr(name, attribute string, _ *fuse.Context) fuse.Status {
	path, err := logicalPath(name)
	if err != nil {
		return status(err)
	}
	code := f.withMutation(func() error { return f.core.RemoveXattr(f.lifetime, path, attribute, "") })
	if code == fuse.OK {
		f.changed("removexattr", "path", path)
	}
	return code
}
