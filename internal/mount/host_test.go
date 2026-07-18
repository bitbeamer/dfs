package mount

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareMountpointCreatesMissingDirectory(t *testing.T) {
	mountpoint := filepath.Join(t.TempDir(), "nested", "mount")
	if err := prepareMountpoint(mountpoint); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("mountpoint mode = %s, want directory", info.Mode())
	}
}

func TestPrepareMountpointAcceptsExistingDirectory(t *testing.T) {
	if err := prepareMountpoint(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareMountpointRejectsFile(t *testing.T) {
	mountpoint := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(mountpoint, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := prepareMountpoint(mountpoint)
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("prepareMountpoint() error = %v, want not-a-directory error", err)
	}
}

func TestPrepareMountpointExplainsInaccessibleMount(t *testing.T) {
	mountpoint := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink("loop", mountpoint); err != nil {
		t.Fatal(err)
	}
	err := prepareMountpoint(mountpoint)
	if err == nil || !strings.Contains(err.Error(), "unmount any stale DFS/FUSE mount") {
		t.Fatalf("prepareMountpoint() error = %v, want stale-mount guidance", err)
	}
}
