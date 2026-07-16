package mount

import (
	stdcontext "context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
	"github.com/hanwen/go-fuse/v2/fuse/pathfs"
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
	sizesMu  sync.Mutex
	sizes    map[string]uint64
}

type trackedFile struct {
	nodefs.File
	filesystem *FileSystem
	path       string
	writable   bool
	once       sync.Once
}

func NewFileSystem(repo *repository.Repository, notifier changeNotifier) *FileSystem {
	return &FileSystem{
		FileSystem: pathfs.NewLoopbackFileSystem(repo.Config.Repository),
		repo:       repo, root: repo.Config.Repository, notifier: notifier,
		sizes: make(map[string]uint64),
	}
}

func clean(name string) string {
	name = filepath.ToSlash(filepath.Clean(filepath.FromSlash(name)))
	if name == "." {
		return ""
	}
	return strings.TrimPrefix(name, "/")
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
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 24*time.Hour)
	defer cancel()
	return f.repo.Fetch(ctx, clean(name), "")
}

func (f *FileSystem) prepareWrite(name string) error {
	full := f.full(name)
	if !annexSymlink(full) {
		return nil
	}
	if _, err := os.Stat(full); err != nil {
		if err := f.hydrate(name); err != nil {
			return err
		}
	}
	ctx, cancel := stdcontext.WithTimeout(stdcontext.Background(), 10*time.Minute)
	defer cancel()
	return f.repo.Unlock(ctx, clean(name))
}

func (f *FileSystem) changed(reason string) {
	f.sizesMu.Lock()
	clear(f.sizes)
	f.sizesMu.Unlock()
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
	attr, code := f.FileSystem.GetAttr(name, context)
	if code != fuse.OK || !annexSymlink(f.full(name)) {
		return attr, code
	}
	attr.Mode = (attr.Mode & 0o7777) | syscall.S_IFREG
	if info, err := os.Stat(f.full(name)); err == nil {
		attr.Size = uint64(info.Size())
	} else {
		path := clean(name)
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
	return attr, code
}

func (f *FileSystem) Open(name string, flags uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	if hidden(name) {
		return nil, fuse.ENOENT
	}
	writable := flags&syscall.O_ACCMODE != syscall.O_RDONLY
	if writable {
		if f.notifier != nil {
			f.notifier.BeginWrite()
		}
		if err := f.prepareWrite(name); err != nil {
			if f.notifier != nil {
				f.notifier.EndWrite()
			}
			return nil, status(err)
		}
	} else if annexSymlink(f.full(name)) {
		if _, err := os.Stat(f.full(name)); err != nil {
			if err := f.hydrate(name); err != nil {
				return nil, status(err)
			}
		}
	}
	file, code := f.FileSystem.Open(name, flags, context)
	if code != fuse.OK {
		if writable && f.notifier != nil {
			f.notifier.EndWrite()
		}
		return nil, code
	}
	f.repo.Touch(clean(name))
	return &trackedFile{File: file, filesystem: f, path: clean(name), writable: writable}, fuse.OK
}

func (f *FileSystem) Create(name string, flags uint32, mode uint32, context *fuse.Context) (nodefs.File, fuse.Status) {
	if hidden(name) {
		return nil, fuse.EACCES
	}
	if f.notifier != nil {
		f.notifier.BeginWrite()
	}
	file, code := f.FileSystem.Create(name, flags, mode, context)
	if code != fuse.OK {
		if f.notifier != nil {
			f.notifier.EndWrite()
		}
		return nil, code
	}
	return &trackedFile{File: file, filesystem: f, path: clean(name), writable: true}, fuse.OK
}

func (t *trackedFile) Read(dest []byte, off int64) (fuse.ReadResult, fuse.Status) {
	t.filesystem.repo.Touch(t.path)
	return t.File.Read(dest, off)
}

func (t *trackedFile) Release() {
	t.File.Release()
	if t.writable {
		t.once.Do(func() {
			if t.filesystem.notifier != nil {
				t.filesystem.notifier.EndWrite()
			} else {
				t.filesystem.changed("completed write")
			}
		})
	}
}

func (f *FileSystem) OpenDir(name string, context *fuse.Context) ([]fuse.DirEntry, fuse.Status) {
	if hidden(name) {
		return nil, fuse.ENOENT
	}
	entries, code := f.FileSystem.OpenDir(name, context)
	if code != fuse.OK {
		return nil, code
	}
	result := entries[:0]
	for _, entry := range entries {
		if entry.Name != ".git" && entry.Name != ".dfs" {
			result = append(result, entry)
		}
	}
	return result, fuse.OK
}

func (f *FileSystem) Truncate(name string, size uint64, context *fuse.Context) fuse.Status {
	if err := f.prepareWrite(name); err != nil {
		return status(err)
	}
	code := f.FileSystem.Truncate(name, size, context)
	if code == fuse.OK {
		f.changed("truncate")
	}
	return code
}

func (f *FileSystem) Mkdir(name string, mode uint32, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	code := f.FileSystem.Mkdir(name, mode, context)
	if code == fuse.OK {
		f.changed("mkdir")
	}
	return code
}

func (f *FileSystem) Mknod(name string, mode uint32, dev uint32, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	code := f.FileSystem.Mknod(name, mode, dev, context)
	if code == fuse.OK {
		f.changed("mknod")
	}
	return code
}

func (f *FileSystem) Rename(oldName, newName string, context *fuse.Context) fuse.Status {
	if hidden(oldName) || hidden(newName) {
		return fuse.EACCES
	}
	code := f.FileSystem.Rename(oldName, newName, context)
	if code == fuse.OK {
		f.changed("rename")
	}
	return code
}

func (f *FileSystem) Rmdir(name string, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	code := f.FileSystem.Rmdir(name, context)
	if code == fuse.OK {
		f.changed("rmdir")
	}
	return code
}

func (f *FileSystem) Unlink(name string, context *fuse.Context) fuse.Status {
	if hidden(name) {
		return fuse.EACCES
	}
	code := f.FileSystem.Unlink(name, context)
	if code == fuse.OK {
		f.changed("unlink")
	}
	return code
}

func (f *FileSystem) Link(oldName, newName string, context *fuse.Context) fuse.Status {
	if hidden(oldName) || hidden(newName) {
		return fuse.EACCES
	}
	code := f.FileSystem.Link(oldName, newName, context)
	if code == fuse.OK {
		f.changed("link")
	}
	return code
}

func (f *FileSystem) Symlink(value, linkName string, context *fuse.Context) fuse.Status {
	if hidden(linkName) {
		return fuse.EACCES
	}
	code := f.FileSystem.Symlink(value, linkName, context)
	if code == fuse.OK {
		f.changed("symlink")
	}
	return code
}

func (f *FileSystem) Chmod(name string, mode uint32, context *fuse.Context) fuse.Status {
	code := f.FileSystem.Chmod(name, mode, context)
	if code == fuse.OK {
		f.changed("chmod")
	}
	return code
}

func (f *FileSystem) Chown(name string, uid, gid uint32, context *fuse.Context) fuse.Status {
	code := f.FileSystem.Chown(name, uid, gid, context)
	if code == fuse.OK {
		f.changed("chown")
	}
	return code
}

func (f *FileSystem) Utimens(name string, atime, mtime *time.Time, context *fuse.Context) fuse.Status {
	code := f.FileSystem.Utimens(name, atime, mtime, context)
	if code == fuse.OK {
		f.changed("utimens")
	}
	return code
}
