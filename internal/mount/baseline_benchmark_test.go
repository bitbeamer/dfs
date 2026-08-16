package mount

import (
	"fmt"
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
)

const (
	baselineDirectoryEntries = 256
	baselineBulkSize         = 8 << 20
	baselineBlockSize        = 128 << 10
)

func benchmarkFileSystem(b *testing.B) (*FileSystem, string) {
	b.Helper()
	root := b.TempDir()
	state, err := store.Open(filepath.Join(b.TempDir(), "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = state.Close() })
	repo := &repository.Repository{Config: config.Default("benchmark", root), Store: state}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	filesystem := NewFileSystem(repo, nil, logger)
	return filesystem, root
}

func BenchmarkBaselineNamespace(b *testing.B) {
	filesystem, root := benchmarkFileSystem(b)
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o755); err != nil {
		b.Fatal(err)
	}
	for index := 0; index < baselineDirectoryEntries; index++ {
		name := filepath.Join(root, "directory", fmt.Sprintf("entry-%03d", index))
		if err := os.WriteFile(name, []byte("baseline"), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("Enumerate256", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			entries, code := filesystem.OpenDir("directory", nil)
			if code != fuse.OK || len(entries) != baselineDirectoryEntries {
				b.Fatalf("OpenDir = %d entries, %v", len(entries), code)
			}
		}
	})
	b.Run("Lookup", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			attr, code := filesystem.GetAttr("directory/entry-127", nil)
			if code != fuse.OK || attr == nil {
				b.Fatalf("GetAttr = %v, %v", attr, code)
			}
		}
	})
	b.Run("Attributes", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			attr, code := filesystem.GetAttr("directory", nil)
			if code != fuse.OK || attr == nil {
				b.Fatalf("GetAttr = %v, %v", attr, code)
			}
		}
	})
}

func BenchmarkBaselineCachedContent(b *testing.B) {
	filesystem, root := benchmarkFileSystem(b)
	data := make([]byte, baselineBulkSize)
	for index := range data {
		data[index] = byte(index)
	}
	if err := os.WriteFile(filepath.Join(root, "cached.bin"), data, 0o644); err != nil {
		b.Fatal(err)
	}

	b.Run("Open", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			handle, code := filesystem.Open("cached.bin", syscall.O_RDONLY, nil)
			if code != fuse.OK {
				b.Fatalf("Open = %v", code)
			}
			handle.Release()
		}
	})
	b.Run("Read128KiB", func(b *testing.B) {
		handle, code := filesystem.Open("cached.bin", syscall.O_RDONLY, nil)
		if code != fuse.OK {
			b.Fatalf("Open = %v", code)
		}
		defer handle.Release()
		buffer := make([]byte, baselineBlockSize)
		b.SetBytes(baselineBlockSize)
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			result, status := handle.Read(buffer, int64(index%64)*baselineBlockSize)
			if status != fuse.OK {
				b.Fatalf("Read = %v", status)
			}
			if _, status = result.Bytes(buffer); status != fuse.OK {
				b.Fatalf("Read result = %v", status)
			}
		}
	})
	b.Run("Read8MiB", func(b *testing.B) {
		handle, code := filesystem.Open("cached.bin", syscall.O_RDONLY, nil)
		if code != fuse.OK {
			b.Fatalf("Open = %v", code)
		}
		defer handle.Release()
		buffer := make([]byte, baselineBulkSize)
		b.SetBytes(baselineBulkSize)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			result, status := handle.Read(buffer, 0)
			if status != fuse.OK {
				b.Fatalf("Read = %v", status)
			}
			if _, status = result.Bytes(buffer); status != fuse.OK {
				b.Fatalf("Read result = %v", status)
			}
		}
	})
}

func BenchmarkBaselineMutations(b *testing.B) {
	b.Run("CreateAndCommit128KiB", func(b *testing.B) {
		filesystem, _ := benchmarkFileSystem(b)
		data := make([]byte, baselineBlockSize)
		b.SetBytes(baselineBlockSize)
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			name := fmt.Sprintf("create-%d.bin", index)
			handle, code := filesystem.Create(name, syscall.O_WRONLY|syscall.O_EXCL, 0o644, nil)
			if code != fuse.OK {
				b.Fatalf("Create = %v", code)
			}
			if _, code = handle.Write(data, 0); code != fuse.OK {
				b.Fatalf("Write = %v", code)
			}
			if code = handle.Flush(); code != fuse.OK {
				b.Fatalf("Flush = %v", code)
			}
			handle.Release()
		}
	})
	b.Run("Rename", func(b *testing.B) {
		filesystem, root := benchmarkFileSystem(b)
		if err := os.WriteFile(filepath.Join(root, "rename-a"), nil, 0o644); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			oldName, newName := "rename-a", "rename-b"
			if index%2 != 0 {
				oldName, newName = newName, oldName
			}
			if code := filesystem.Rename(oldName, newName, nil); code != fuse.OK {
				b.Fatalf("Rename = %v", code)
			}
		}
	})
	b.Run("Delete", func(b *testing.B) {
		filesystem, root := benchmarkFileSystem(b)
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			name := fmt.Sprintf("delete-%d", index)
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
				b.Fatal(err)
			}
			if code := filesystem.Unlink(name, nil); code != fuse.OK {
				b.Fatalf("Unlink = %v", code)
			}
		}
	})
}
