package mount

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/store"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

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
