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
	"github.com/hanwen/go-fuse/v2/fuse"
)

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
