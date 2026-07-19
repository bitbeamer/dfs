package mount

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/store"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

type attrFile struct {
	nodefs.File
	called chan struct{}
	attr   fuse.Attr
}

func (f *attrFile) GetAttr(out *fuse.Attr) fuse.Status {
	if f.called != nil {
		close(f.called)
	}
	*out = f.attr
	return fuse.OK
}

func testFileSystem(t *testing.T, root string) *FileSystem {
	t.Helper()
	state, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo := &repository.Repository{Config: config.Default("test", root), Store: state}
	return NewFileSystem(repo, nil, logger)
}

func TestGetAttrUsesAnnexObjectPermissions(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo := &repository.Repository{Config: config.Default("test", root)}
	filesystem := NewFileSystem(repo, nil, logger)

	tests := []struct {
		name       string
		objectMode os.FileMode
		wantMode   uint32
	}{
		{name: "ordinary", objectMode: 0o444, wantMode: 0o644},
		{name: "private", objectMode: 0o400, wantMode: 0o600},
		{name: "executable", objectMode: 0o555, wantMode: 0o755},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := filepath.Join(root, ".git", "annex", "objects", test.name, "content")
			if err := os.MkdirAll(filepath.Dir(object), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(object, []byte("content"), test.objectMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(object, test.objectMode); err != nil {
				t.Fatal(err)
			}
			name := test.name + ".txt"
			target, err := filepath.Rel(root, object)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}

			attr, code := filesystem.GetAttr(name, nil)
			if code != fuse.OK {
				t.Fatalf("GetAttr() status = %v", code)
			}
			if got := attr.Mode & 0o777; got != test.wantMode {
				t.Fatalf("GetAttr() permissions = %#o, want %#o", got, test.wantMode)
			}
			if attr.Mode&syscall.S_IFMT != syscall.S_IFREG {
				t.Fatalf("GetAttr() type = %#o, want regular file", attr.Mode&syscall.S_IFMT)
			}
		})
	}
}

func TestOpenAnnexFileUsesDirectIO(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	object := filepath.Join(root, ".git", "annex", "objects", "key", "content")
	if err := os.MkdirAll(filepath.Dir(object), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte("current version\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(root, object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "annex.txt")); err != nil {
		t.Fatal(err)
	}

	file, code := filesystem.Open("annex.txt", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatalf("Open() status = %v", code)
	}
	defer file.Release()
	wrapped, ok := file.(*nodefs.WithFlags)
	if !ok {
		t.Fatalf("Open() file = %T, want *nodefs.WithFlags", file)
	}
	if wrapped.FuseFlags&fuse.FOPEN_DIRECT_IO == 0 {
		t.Fatalf("Open() flags = %#x, want FOPEN_DIRECT_IO", wrapped.FuseFlags)
	}
	buffer := make([]byte, 64)
	result, code := file.Read(buffer, 0)
	if code != fuse.OK {
		t.Fatalf("Read() status = %v", code)
	}
	content, code := result.Bytes(buffer)
	if code != fuse.OK || string(content) != "current version\n" {
		t.Fatalf("Read() = %q, %v", content, code)
	}
}

func TestAnnexTargetChangeGivesFreshHandlePathInode(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	objects := filepath.Join(root, ".git", "annex", "objects")
	oldObject := filepath.Join(objects, "SHA256E-s4--old", "content")
	newObject := filepath.Join(objects, "SHA256E-s4--new", "content")
	for path, content := range map[string]string{oldObject: "old\n", newObject: "new\n"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "annex.txt")
	oldTarget, err := filepath.Rel(root, oldObject)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldTarget, link); err != nil {
		t.Fatal(err)
	}
	filesystem.attrs["annex.txt"] = visibleState{
		attr:      fuse.Attr{Ino: 42, Size: 4, Mode: syscall.S_IFREG | 0o644},
		signature: "old",
	}

	oldHandle, code := filesystem.Open("annex.txt", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatalf("open old target: %v", code)
	}
	defer oldHandle.Release()

	newTarget, err := filepath.Rel(root, newObject)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement")
	if err := os.Symlink(newTarget, replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, link); err != nil {
		t.Fatal(err)
	}

	pathAttr, code := filesystem.GetAttr("annex.txt", nil)
	if code != fuse.OK {
		t.Fatalf("GetAttr() after target change = %v", code)
	}
	freshHandle, code := filesystem.Open("annex.txt", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatalf("open new target: %v", code)
	}
	defer freshHandle.Release()
	var freshAttr fuse.Attr
	if code := freshHandle.GetAttr(&freshAttr); code != fuse.OK {
		t.Fatalf("fresh handle GetAttr() = %v", code)
	}
	if pathAttr.Ino != freshAttr.Ino {
		t.Fatalf("path inode %d does not identify fresh handle inode %d", pathAttr.Ino, freshAttr.Ino)
	}

	buffer := make([]byte, 4)
	result, code := freshHandle.Read(buffer, 0)
	if code != fuse.OK {
		t.Fatalf("fresh handle Read() = %v", code)
	}
	content, code := result.Bytes(buffer)
	if code != fuse.OK || string(content) != "new\n" {
		t.Fatalf("fresh handle content = %q, %v", content, code)
	}
}

func TestTrackedFileGetAttrWaitsForWorkTreeUpdates(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	locked := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = filesystem.repo.WithWorkTreeLock(func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	called := make(chan struct{})
	done := make(chan struct{})
	file := &trackedFile{
		File:       &attrFile{File: nodefs.NewDefaultFile(), called: called},
		filesystem: filesystem,
		path:       "annex.txt",
	}
	go func() {
		file.GetAttr(&fuse.Attr{})
		close(done)
	}()
	select {
	case <-called:
		t.Fatal("handle attributes observed the worktree during an annex update")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handle attributes did not resume after the annex update")
	}
}

func TestTrackedFileGetAttrUsesVisibleInode(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	filesystem.attrs["annex.txt"] = visibleState{
		attr: fuse.Attr{Ino: 42, Size: 7, Mode: syscall.S_IFREG | 0o644},
	}
	file := &trackedFile{
		File:       &attrFile{File: nodefs.NewDefaultFile(), attr: fuse.Attr{Ino: 99, Size: 7}},
		filesystem: filesystem,
		path:       "annex.txt",
	}
	var attr fuse.Attr
	if code := file.GetAttr(&attr); code != fuse.OK {
		t.Fatalf("GetAttr() = %v", code)
	}
	if attr.Ino != 42 {
		t.Fatalf("GetAttr().Ino = %d, want preserved visible inode 42", attr.Ino)
	}
}
