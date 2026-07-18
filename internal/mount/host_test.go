package mount

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type failingUnmountServer struct{ err error }

func (s failingUnmountServer) Unmount() error { return s.err }

func TestUnmountFailureDoesNotWaitForServe(t *testing.T) {
	wantErr := errors.New("mount is busy")
	serveDone := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- unmountAndWait(failingUnmountServer{err: wantErr}, serveDone)
	}()

	select {
	case err := <-result:
		if !errors.Is(err, wantErr) {
			t.Fatalf("unmountAndWait() error = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("unmountAndWait() waited for Serve after unmount failed")
	}
}

func TestPrepareMountpointCreatesMissingDirectory(t *testing.T) {
	mountpoint := filepath.Join(t.TempDir(), "nested", "mount")
	if _, err := prepareMountpoint(mountpoint); err != nil {
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
	if _, err := prepareMountpoint(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareMountpointRejectsFile(t *testing.T) {
	mountpoint := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(mountpoint, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := prepareMountpoint(mountpoint)
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("prepareMountpoint() error = %v, want not-a-directory error", err)
	}
}

func TestPrepareMountpointExplainsInaccessibleMount(t *testing.T) {
	mountpoint := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink("loop", mountpoint); err != nil {
		t.Fatal(err)
	}
	_, err := prepareMountpoint(mountpoint)
	if err == nil || !strings.Contains(err.Error(), "unmount any stale DFS/FUSE mount") {
		t.Fatalf("prepareMountpoint() error = %v, want stale-mount guidance", err)
	}
}

func TestPrepareMountpointDetachesStaleFuseMount(t *testing.T) {
	mountpoint := "/stale/mount"
	directory := t.TempDir()
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	statCalls := 0
	cleanupCalls := 0
	access := mountpointAccess{
		stat: func(path string) (os.FileInfo, error) {
			statCalls++
			if statCalls == 1 {
				return nil, &os.PathError{Op: "stat", Path: path, Err: syscall.ENOTCONN}
			}
			return info, nil
		},
		mkdirAll: os.MkdirAll,
		clearStale: func(path string) error {
			cleanupCalls++
			if path != mountpoint {
				t.Fatalf("clearStale() path = %q, want %q", path, mountpoint)
			}
			return nil
		},
	}

	cleared, err := prepareMountpointWithAccess(mountpoint, access)
	if err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("prepareMountpointWithAccess() did not report stale mount cleanup")
	}
	if cleanupCalls != 1 || statCalls != 2 {
		t.Fatalf("cleanup calls = %d, stat calls = %d; want 1 and 2", cleanupCalls, statCalls)
	}
}

func TestStaleMountErrorsByPlatform(t *testing.T) {
	tests := []struct {
		name string
		goos string
		err  error
		want bool
	}{
		{name: "Linux disconnected endpoint", goos: "linux", err: syscall.ENOTCONN, want: true},
		{name: "macOS disconnected endpoint", goos: "darwin", err: syscall.ENXIO, want: true},
		{name: "Linux unrelated device error", goos: "linux", err: syscall.ENXIO, want: false},
		{name: "macOS permission error", goos: "darwin", err: syscall.EACCES, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := &os.PathError{Op: "stat", Path: "/mount", Err: test.err}
			if got := isStaleMountErrorForOS(err, test.goos); got != test.want {
				t.Fatalf("isStaleMountErrorForOS(%v, %q) = %v, want %v", err, test.goos, got, test.want)
			}
		})
	}
}
