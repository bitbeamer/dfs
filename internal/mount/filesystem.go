package mount

import (
	stdcontext "context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/store"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
	"golang.org/x/text/unicode/norm"
)

type changeNotifier interface {
	Notify(reason string)
	BeginWrite()
	EndWrite()
}

type FileSystem struct {
	pathfs.FileSystem
	repo     *repository.Repository
	root     string
	notifier changeNotifier
	logger   *slog.Logger
	sizesMu  sync.Mutex
	sizes    map[string]uint64
	writesMu sync.Mutex
	writes   map[string]*writeTransaction
	attrsMu  sync.Mutex
	attrs    map[string]visibleState
}

type trackedFile struct {
	nodefs.File
	filesystem *FileSystem
	path       string
}

func NewFileSystem(repo *repository.Repository, notifier changeNotifier, logger *slog.Logger) *FileSystem {
	return &FileSystem{
		FileSystem: pathfs.NewLoopbackFileSystem(repo.Config.Repository),
		repo:       repo, root: repo.Config.Repository, notifier: notifier, logger: logger,
		sizes: make(map[string]uint64), writes: make(map[string]*writeTransaction),
		attrs: make(map[string]visibleState),
	}
}

func clean(name string) string {
	name = filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if name == "." {
		return ""
	}
	return norm.NFC.String(strings.TrimPrefix(name, "/"))
}

func hidden(name string) bool {
	name = clean(name)
	first := strings.Split(name, "/")[0]
	return first == ".git" || first == ".dfs"
}

func (f *FileSystem) full(name string) string {
	return filepath.Join(f.root, filepath.FromSlash(clean(name)))
}

func annexSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return false
	}
	target = filepath.ToSlash(target)
	return strings.Contains(target, "/.git/annex/objects/") || strings.HasPrefix(target, ".git/annex/objects/")
}

func (f *FileSystem) hydrate(name string) error {
	path := clean(name)
	started := time.Now()
	f.logger.Info("content hydration started", "path", path)
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 24*time.Hour)
	defer cancel()
	err := f.repo.Fetch(ctx, path, "")
	if err != nil {
		f.logger.Error("content hydration failed", "path", path, "duration", time.Since(started), "error", err)
		return err
	}
	f.logger.Info("content hydration completed", "path", path, "duration", time.Since(started))
	return nil
}

func (f *FileSystem) changed(reason string, attrs ...any) {
	f.sizesMu.Lock()
	clear(f.sizes)
	f.sizesMu.Unlock()
	f.logger.Info("filesystem changed", append([]any{"operation", reason}, attrs...)...)
	if f.notifier != nil {
		f.notifier.Notify(reason)
	}
}

func status(err error) fuse.Status {
	if err == nil {
		return fuse.OK
	}
	return fuse.ToStatus(err)
}

func (f *FileSystem) GetAttr(name string, context *fuse.Context) (*fuse.Attr, fuse.Status) {
	if hidden(name) {
		return nil, fuse.ENOENT
	}
	path := clean(name)
	if attr, ok := f.stagedAttr(path); ok {
		return attr, fuse.OK
	}
	attr, code := f.FileSystem.GetAttr(path, context)
	if code != fuse.OK {
		return attr, code
	}
	locked := annexSymlink(f.full(name))
	if locked {
		// Loopback GetAttr describes the git-annex symlink here. Symlink modes
		// differ by platform (typically 0777 on Linux and 0755 on macOS), so
		// carrying those bits into a regular-file attribute makes every file
		// appear executable and may make it appear world-writable. Use the annex
		// object's permissions and add owner-write for the writable DFS view.
		attr.Mode = syscall.S_IFREG | 0o644
		if info, err := os.Stat(f.full(name)); err == nil {
			attr.Mode = syscall.S_IFREG | uint32(info.Mode().Perm()|0o200)
			attr.Size = uint64(info.Size())
		} else {
			f.sizesMu.Lock()
			size, ok := f.sizes[path]
			f.sizesMu.Unlock()
			if !ok {
				ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 30*time.Second)
				value, sizeErr := f.repo.AnnexFileSize(ctx, path)
				cancel()
				if sizeErr == nil {
					size = uint64(value)
					f.sizesMu.Lock()
					f.sizes[path] = size
					f.sizesMu.Unlock()
				}
			}
			attr.Size = size
		}
		attr.Blocks = (attr.Size + 511) / 512
	}
	f.applyVisibleAttr(path, attr, locked)
	return attr, code
}

func (f *FileSystem) Open(name string, flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	if hidden(name) {
		return nil, fuse.ENOENT
	}
	path := clean(name)
	writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	if writable {
		file, err := f.openStaged(path, flags, 0, false)
		return file, status(err)
	}
	if staged, ok, err := f.openStagedRead(path); ok {
		if err != nil {
			return nil, status(err)
		}
		f.repo.Touch(path)
		return &trackedFile{File: staged, filesystem: f, path: path}, fuse.OK
	}
	file, annexed, err := f.openBackingRead(path, flags)
	if err != nil {
		return nil, status(err)
	}
	f.repo.Touch(path)
	f.logger.Debug("file opened", "path", path, "writable", false, "flags", flags)
	tracked := &trackedFile{File: file, filesystem: f, path: path}
	if annexed {
		// Git-annex publishes a new version by replacing the symlink in the
		// worktree. Stable FUSE inode identities can otherwise retain pages from
		// the previous target, so every fresh annex open must bypass that cache.
		return &nodefs.WithFlags{File: tracked, FuseFlags: fuse.FOPEN_DIRECT_IO}, fuse.OK
	}
	return tracked, fuse.OK
}

// openBackingRead pins one worktree version to an open file descriptor while
// excluding DFS sync/commit operations that replace annex links. A broken
// annex link is hydrated outside the repository lock and then opened again.
func (f *FileSystem) openBackingRead(path string, flags uint32) (nodefs.File, bool, error) {
	// Match go-fuse's loopback behavior: the kernel translates append writes to
	// explicit offsets before they reach a file handle.
	flags &^= syscall.O_APPEND
	for attempt := 0; attempt < 2; attempt++ {
		var (
			handle     *os.File
			annexed    bool
			needsFetch bool
		)
		err := f.repo.WithWorkTreeLock(func() error {
			fullPath := f.full(path)
			annexed = annexSymlink(fullPath)
			var openErr error
			handle, openErr = os.OpenFile(fullPath, int(flags), 0)
			if openErr != nil && annexed && errors.Is(openErr, os.ErrNotExist) {
				needsFetch = true
				return nil
			}
			return openErr
		})
		if err != nil {
			return nil, annexed, err
		}
		if !needsFetch {
			return nodefs.NewLoopbackFile(handle), annexed, nil
		}
		if err := f.hydrate(path); err != nil {
			return nil, true, err
		}
	}
	return nil, true, os.ErrNotExist
}

func (f *FileSystem) Create(name string, flags uint32, mode uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	if hidden(name) {
		return nil, fuse.EACCES
	}
	path := clean(name)
	file, err := f.openStaged(path, flags, os.FileMode(mode), true)
	if err != nil {
		return nil, status(err)
	}
	f.logger.Info("file created", "path", path)
	return file, fuse.OK
}

func (t *trackedFile) Read(dest []byte, off int64) (fuse.ReadResult, fuse.Status) {
	t.filesystem.repo.Touch(t.path)
	return t.File.Read(dest, off)
}

func (t *trackedFile) GetAttr(out *fuse.Attr) fuse.Status {
	var code fuse.Status
	err := t.filesystem.repo.WithWorkTreeLock(func() error {
		code = t.File.GetAttr(out)
		if code != fuse.OK {
			return nil
		}
		if attr, ok := t.filesystem.stagedAttr(t.path); ok {
			if attr.Ino != 0 {
				out.Ino = attr.Ino
			}
			return nil
		}
		// FileSystem.GetAttr presents the inode and metadata captured when a
		// write was published. Apply the same view to open handles: git-annex
		// may replace the worktree file with a symlink to an object, but that
		// internal representation change must not make fstat disagree with stat
		// and cause name-following readers to reopen identical content.
		t.filesystem.applyVisibleAttr(t.path, out, annexSymlink(t.filesystem.full(t.path)))
		return nil
	})
	if err != nil {
		return status(err)
	}
	return code
}

func (t *trackedFile) Release() {
	t.File.Release()
}

func (f *FileSystem) OpenDir(name string, context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	if hidden(name) {
		return nil, fuse.ENOENT
	}
	entries, code := f.FileSystem.OpenDir(clean(name), context)
	if code != fuse.OK {
		return nil, code
	}
	result := entries[:0]
	for _, entry := range entries {
		if entry.Name != ".git" && entry.Name != ".dfs" {
			entry.Name = norm.NFC.String(entry.Name)
			result = append(result, entry)
		}
	}
	return result, fuse.OK
}

func (f *FileSystem) Truncate(name string, size uint64, context *fuse.Context) fuse.Status {
	file, err := f.openStaged(clean(name), syscall.O_WRONLY, 0, false)
	if err != nil {
		return status(err)
	}
	code := file.Truncate(size)
	if code == fuse.OK {
		code = file.Flush()
	}
	file.Release()
	return code
}

func (f *FileSystem) Mkdir(name string, mode uint32, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	code := f.FileSystem.Mkdir(clean(name), mode, context)
	if code == fuse.OK {
		f.changed("mkdir", "path", clean(name))
	}
	return code
}

func (f *FileSystem) Mknod(name string, mode uint32, dev uint32, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	code := f.FileSystem.Mknod(clean(name), mode, dev, context)
	if code == fuse.OK {
		f.changed("mknod", "path", clean(name))
	}
	return code
}

func (f *FileSystem) Rename(oldName, newName string, context *fuse.Context) fuse.Status {
	if hidden(oldName) || hidden(newName) {
		return fuse.EACCES
	}
	oldPath, newPath := clean(oldName), clean(newName)
	if oldPath == newPath {
		return fuse.OK
	}
	code := f.FileSystem.Rename(oldPath, newPath, context)
	if code == fuse.OK {
		if err := f.renameWrite(oldPath, newPath); err != nil {
			return status(err)
		}
		if f.repo.Store != nil {
			if err := f.repo.Store.RenameFileState(oldPath, newPath); err != nil {
				return status(err)
			}
		}
		f.renameVisible(oldPath, newPath)
		f.changed("rename", "old_path", oldPath, "new_path", newPath)
	}
	return code
}

func (f *FileSystem) Rmdir(name string, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	code := f.FileSystem.Rmdir(clean(name), context)
	if code == fuse.OK {
		if f.repo.Store != nil {
			if err := f.repo.Store.RemoveFileState(clean(name)); err != nil {
				return status(err)
			}
		}
		f.removeVisible(clean(name))
		f.changed("rmdir", "path", clean(name))
	}
	return code
}

func (f *FileSystem) Unlink(name string, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	path := clean(name)
	code := f.FileSystem.Unlink(path, context)
	if code == fuse.OK {
		f.unlinkWrite(path)
		if f.repo.Store != nil {
			if err := f.repo.Store.RemoveFileState(path); err != nil {
				return status(err)
			}
		}
		f.removeVisible(path)
		f.changed("unlink", "path", path)
	}
	return code
}

func (f *FileSystem) Link(oldName, newName string, context *fuse.Context) fuse.Status {
	if hidden(oldName) || hidden(newName) {
		return fuse.EACCES
	}
	code := f.FileSystem.Link(clean(oldName), clean(newName), context)
	if code == fuse.OK {
		f.changed("link", "old_path", clean(oldName), "new_path", clean(newName))
	}
	return code
}

func (f *FileSystem) Symlink(value, linkName string, context *fuse.Context) fuse.Status {
	if hidden(linkName) {
		return fuse.EACCES
	}
	code := f.FileSystem.Symlink(value, clean(linkName), context)
	if code == fuse.OK {
		f.changed("symlink", "path", clean(linkName))
	}
	return code
}

func (f *FileSystem) Chmod(name string, mode uint32, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	path := clean(name)
	if transaction := f.writeAt(path); transaction != nil {
		if err := os.Chmod(transaction.stagingPath, os.FileMode(mode)); err != nil {
			return status(err)
		}
		f.markDirty(transaction)
		return status(f.captureVisible(path))
	}
	if annexSymlink(f.full(path)) {
		attr, code := f.GetAttr(path, context)
		if code != fuse.OK {
			return code
		}
		attr.Mode = attr.Mode&syscall.S_IFMT | mode&0o7777
		attr.Ctime, attr.Ctimensec = splitTime(time.Now())
		if err := f.saveVisible(path, attr, f.visibleSignature(path)); err != nil {
			return status(err)
		}
		f.changed("chmod", "path", path, "mode", mode)
		return fuse.OK
	}
	code := f.FileSystem.Chmod(path, mode, context)
	if code == fuse.OK {
		if err := f.captureVisible(path); err != nil {
			return status(err)
		}
		f.changed("chmod", "path", path, "mode", mode)
	}
	return code
}

func (f *FileSystem) Chown(name string, uid, gid uint32, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	path := clean(name)
	if transaction := f.writeAt(path); transaction != nil {
		if err := os.Chown(transaction.stagingPath, chownID(uid), chownID(gid)); err != nil {
			return status(err)
		}
		f.markDirty(transaction)
		return status(f.captureVisible(path))
	}
	if annexSymlink(f.full(path)) {
		attr, code := f.GetAttr(path, context)
		if code != fuse.OK {
			return code
		}
		if uid != ^uint32(0) {
			attr.Uid = uid
		}
		if gid != ^uint32(0) {
			attr.Gid = gid
		}
		attr.Ctime, attr.Ctimensec = splitTime(time.Now())
		if err := f.saveVisible(path, attr, f.visibleSignature(path)); err != nil {
			return status(err)
		}
		f.changed("chown", "path", path, "uid", uid, "gid", gid)
		return fuse.OK
	}
	code := f.FileSystem.Chown(path, uid, gid, context)
	if code == fuse.OK {
		if err := f.captureVisible(path); err != nil {
			return status(err)
		}
		f.changed("chown", "path", path, "uid", uid, "gid", gid)
	}
	return code
}

func (f *FileSystem) Utimens(name string, atime, mtime *time.Time, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	path := clean(name)
	if transaction := f.writeAt(path); transaction != nil {
		if err := setStagedTimes(transaction.stagingPath, atime, mtime); err != nil {
			return status(err)
		}
		f.markDirty(transaction)
		return status(f.captureVisible(path))
	}
	if annexSymlink(f.full(path)) {
		attr, code := f.GetAttr(path, context)
		if code != fuse.OK {
			return code
		}
		if atime != nil {
			attr.Atime, attr.Atimensec = splitTime(*atime)
		}
		if mtime != nil {
			attr.Mtime, attr.Mtimensec = splitTime(*mtime)
		}
		attr.Ctime, attr.Ctimensec = splitTime(time.Now())
		if err := f.saveVisible(path, attr, f.visibleSignature(path)); err != nil {
			return status(err)
		}
		f.changed("utimens", "path", path)
		return fuse.OK
	}
	code := f.FileSystem.Utimens(path, atime, mtime, context)
	if code == fuse.OK {
		if err := f.captureVisible(path); err != nil {
			return status(err)
		}
		f.changed("utimens", "path", path)
	}
	return code
}

func (f *FileSystem) GetXAttr(name, attr string, context *fuse.Context) ([]byte, fuse.Status) {
	if hidden(name) {
		return nil, fuse.ENOENT
	}
	if _, code := f.GetAttr(clean(name), context); code != fuse.OK {
		return nil, code
	}
	if f.repo.Store == nil {
		return nil, fuse.ENOSYS
	}
	value, err := f.repo.Store.XAttr(clean(name), attr)
	if errors.Is(err, store.ErrXAttrNotFound) {
		return nil, fuse.ENOATTR
	}
	return value, status(err)
}

func (f *FileSystem) SetXAttr(name, attr string, data []byte, flags int, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	path := clean(name)
	if _, code := f.GetAttr(path, context); code != fuse.OK {
		return code
	}
	if f.repo.Store == nil {
		return fuse.ENOSYS
	}
	err := f.repo.Store.SetXAttr(path, attr, append([]byte(nil), data...), flags)
	switch {
	case errors.Is(err, store.ErrXAttrExists):
		return status(syscall.EEXIST)
	case errors.Is(err, store.ErrXAttrNotFound):
		return fuse.ENOATTR
	case err != nil:
		return status(err)
	}
	f.changed("setxattr", "path", path, "attribute", attr)
	return fuse.OK
}

func (f *FileSystem) ListXAttr(name string, context *fuse.Context) ([]string, fuse.Status) {
	if hidden(name) {
		return nil, fuse.ENOENT
	}
	if _, code := f.GetAttr(clean(name), context); code != fuse.OK {
		return nil, code
	}
	if f.repo.Store == nil {
		return nil, fuse.ENOSYS
	}
	names, err := f.repo.Store.ListXAttrs(clean(name))
	return names, status(err)
}

func (f *FileSystem) RemoveXAttr(name, attr string, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	path := clean(name)
	if _, code := f.GetAttr(path, context); code != fuse.OK {
		return code
	}
	if f.repo.Store == nil {
		return fuse.ENOSYS
	}
	err := f.repo.Store.RemoveXAttr(path, attr)
	if errors.Is(err, store.ErrXAttrNotFound) {
		return fuse.ENOATTR
	}
	if err != nil {
		return status(err)
	}
	f.changed("removexattr", "path", path, "attribute", attr)
	return fuse.OK
}

func splitTime(value time.Time) (uint64, uint32) {
	return uint64(value.Unix()), uint32(value.Nanosecond())
}

func chownID(value uint32) int {
	if value == ^uint32(0) {
		return -1
	}
	return int(value)
}

func setStagedTimes(path string, atime, mtime *time.Time) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	attr := attrFromInfo(info)
	if attr == nil {
		return fmt.Errorf("read staged timestamps for %s", path)
	}
	access := time.Unix(int64(attr.Atime), int64(attr.Atimensec))
	modified := time.Unix(int64(attr.Mtime), int64(attr.Mtimensec))
	if atime != nil {
		access = *atime
	}
	if mtime != nil {
		modified = *mtime
	}
	return os.Chtimes(path, access, modified)
}
