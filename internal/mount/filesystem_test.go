package mount

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/store"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/hanwen/go-fuse/v2/fuse/nodefs"
)

func TestStatusDoesNotReportGenericFailuresAsUnimplemented(t *testing.T) {
	if got := status(errors.New("transfer failed")); got != fuse.EIO {
		t.Fatalf("generic error status = %v, want EIO", got)
	}
	if got := status(context.DeadlineExceeded); got != fuse.ToStatus(syscall.ETIMEDOUT) {
		t.Fatalf("deadline status = %v, want ETIMEDOUT", got)
	}
	if got := status(syscall.EACCES); got != fuse.EACCES {
		t.Fatalf("errno status = %v, want EACCES", got)
	}
}

func TestAnnexSizeFromTarget(t *testing.T) {
	target := ".git/annex/objects/2W/V5/SHA256E-s2--4355/SHA256E-s2--4355"
	if size, ok := annexSizeFromTarget(target); !ok || size != 2 {
		t.Fatalf("annex size = %d, %v", size, ok)
	}
	if _, ok := annexSizeFromTarget(".git/annex/objects/key"); ok {
		t.Fatal("invalid annex target has a size")
	}
}

type attrFile struct {
	nodefs.File
	called chan struct{}
	attr   fuse.Attr
}

type recordingContentInvalidator struct {
	paths []string
}

func (i *recordingContentInvalidator) InvalidateContent(path string) {
	i.paths = append(i.paths, path)
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

func TestReadDoesNotWriteAccessMetadataPerRequest(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	path := "large.bin"
	backingPath := filepath.Join(root, path)
	if err := os.WriteFile(backingPath, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	filesystem.repo.Touch(path) // Open records one cache access before reads begin.
	openedAt := filesystem.repo.Store.LastAccess(path)
	handle, err := os.Open(backingPath)
	if err != nil {
		t.Fatal(err)
	}
	tracked := &trackedFile{
		File: nodefs.NewLoopbackFile(handle), filesystem: filesystem, path: path,
	}
	buffer := make([]byte, 4096)
	for offset := int64(0); offset < 1<<20; offset += int64(len(buffer)) {
		result, code := tracked.Read(buffer, offset)
		if code != fuse.OK {
			t.Fatalf("read at %d = %v", offset, code)
		}
		data, status := result.Bytes(buffer)
		if status != fuse.OK || len(data) != len(buffer) {
			t.Fatalf("read result at %d = %d bytes, %v", offset, len(data), status)
		}
	}
	tracked.Release()
	if after := filesystem.repo.Store.LastAccess(path); !after.Equal(openedAt) {
		t.Fatalf("reads rewrote cache access metadata: before=%v after=%v", openedAt, after)
	}
}

func TestHydrationStopsWhenMountLifetimeIsCanceled(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	lifetime, cancel := context.WithCancel(context.Background())
	filesystem.lifetime = lifetime
	started := make(chan struct{})
	filesystem.repo.SetManagedFetcher(func(ctx context.Context, _ *repository.Repository, _, _ string) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	finished := make(chan error, 1)
	go func() { finished <- filesystem.hydrate("unavailable.bin") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hydration did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("canceled hydration succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("hydration did not stop after mount cancellation")
	}
}

func TestConcurrentHydrationOfSameAnnexObjectIsCoalesced(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	target := filepath.Join(root, ".git", "annex", "objects", "AA", "BB", "SHA256E-s2--same", "SHA256E-s2--same")
	paths := []string{"first.bin", filepath.Join("nested", "second.bin")}
	for _, path := range paths {
		linkPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
			t.Fatal(err)
		}
		relativeTarget, err := filepath.Rel(filepath.Dir(linkPath), target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(relativeTarget, linkPath); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var callsMu sync.Mutex
	filesystem.repo.SetManagedFetcher(func(context.Context, *repository.Repository, string, string) error {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return nil
	})

	results := make(chan error, 2)
	go func() { results <- filesystem.hydrate(filepath.ToSlash(paths[0])) }()
	<-started
	go func() { results <- filesystem.hydrate(filepath.ToSlash(paths[1])) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls != 1 {
		t.Fatalf("managed fetch calls = %d, want 1", calls)
	}
}

func TestOpenUncachedAnnexStreamsRangeBeforeWholeHydration(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	const size = 8 << 20
	key := "SHA256E-s8388608--0000000000000000000000000000000000000000000000000000000000000000.bin"
	target := filepath.Join(root, ".git", "annex", "objects", "AA", "BB", key, key)
	link := filepath.Join(root, "preview.bin")
	relative, err := filepath.Rel(filepath.Dir(link), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relative, link); err != nil {
		t.Fatal(err)
	}
	var rangeCalls int
	filesystem.repo.SetManagedRangeFetcher(func(_ context.Context, _ *repository.Repository, _ string, _, length int64, output io.Writer) (int64, error) {
		rangeCalls++
		_, err := output.Write(bytes.Repeat([]byte{0x5a}, int(length)))
		return size, err
	})
	var hydrationCalls int
	filesystem.repo.SetManagedFetcher(func(context.Context, *repository.Repository, string, string) error {
		hydrationCalls++
		return errors.New("whole hydration must not run")
	})

	handle, code := filesystem.Open("preview.bin", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatalf("open uncached annex = %v", code)
	}
	if rangeCalls != 0 || hydrationCalls != 0 {
		t.Fatalf("open transferred content: ranges=%d hydration=%d", rangeCalls, hydrationCalls)
	}
	buffer := make([]byte, 4096)
	result, code := handle.Read(buffer, 1024)
	if code != fuse.OK {
		t.Fatalf("range read = %v", code)
	}
	data, code := result.Bytes(buffer)
	if code != fuse.OK || len(data) != len(buffer) || !bytes.Equal(data, bytes.Repeat([]byte{0x5a}, len(buffer))) {
		t.Fatalf("range data = %d bytes, %v", len(data), code)
	}
	if rangeCalls != 1 || hydrationCalls != 0 {
		t.Fatalf("transfers after read: ranges=%d hydration=%d", rangeCalls, hydrationCalls)
	}
	handle.Release()
}

func TestReleaseCancelsUncachedAnnexRange(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	key := "SHA256E-s8388608--0000000000000000000000000000000000000000000000000000000000000000.bin"
	target := filepath.Join(root, ".git", "annex", "objects", "AA", "BB", key, key)
	if err := os.Symlink(target, filepath.Join(root, "cancel.bin")); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	filesystem.repo.SetManagedRangeFetcher(func(ctx context.Context, _ *repository.Repository, _ string, _, _ int64, _ io.Writer) (int64, error) {
		close(started)
		<-ctx.Done()
		finished <- ctx.Err()
		return 0, ctx.Err()
	})
	handle, code := filesystem.Open("cancel.bin", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatalf("open = %v", code)
	}
	readDone := make(chan fuse.Status, 1)
	go func() {
		_, status := handle.Read(make([]byte, 4096), 0)
		readDone <- status
	}()
	<-started
	handle.Release()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("range context error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("release did not cancel range request")
	}
	if code := <-readDone; code != fuse.EINTR {
		t.Fatalf("canceled range status = %v", code)
	}
}

func TestOnlyGitMetadataIsHidden(t *testing.T) {
	tests := []struct {
		path   string
		hidden bool
	}{
		{".git/dfs/state.db", true},
		{"project/.git/config", true},
		{"project/.git/hooks/pre-commit", true},
		{"project/.gitignore", false},
		{".dfs/user-data", false},
	}
	for _, test := range tests {
		if got := hidden(test.path); got != test.hidden {
			t.Errorf("hidden(%q) = %v, want %v", test.path, got, test.hidden)
		}
	}
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
	invalidator := &recordingContentInvalidator{}
	filesystem.cacheInvalidator = invalidator
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
	var attr fuse.Attr
	if code := file.GetAttr(&attr); code != fuse.OK {
		t.Fatalf("GetAttr() status = %v", code)
	}
	if len(invalidator.paths) != 0 {
		t.Fatalf("unchanged annex target invalidated paths %q", invalidator.paths)
	}
}

func TestAnnexTargetChangeRefreshesFollowingHandle(t *testing.T) {
	root := t.TempDir()
	filesystem := testFileSystem(t, root)
	invalidator := &recordingContentInvalidator{}
	filesystem.cacheInvalidator = invalidator
	objects := filepath.Join(root, ".git", "annex", "objects")
	oldObject := filepath.Join(objects, "old", "content")
	newObject := filepath.Join(objects, "new", "content")
	for path, content := range map[string]string{
		oldObject: "old\n", newObject: "old\nnew\n",
	} {
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
	initialAttr, code := filesystem.GetAttr("annex.txt", nil)
	if code != fuse.OK {
		t.Fatalf("initial GetAttr() = %v", code)
	}
	handle, code := filesystem.Open("annex.txt", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatalf("Open() = %v", code)
	}
	defer handle.Release()

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
	var handleAttr fuse.Attr
	if code := handle.GetAttr(&handleAttr); code != fuse.OK {
		t.Fatalf("GetAttr() after replacement = %v", code)
	}
	if handleAttr.Ino != initialAttr.Ino {
		t.Fatalf("following handle inode changed from %d to %d", initialAttr.Ino, handleAttr.Ino)
	}
	pathAttr, code := filesystem.GetAttr("annex.txt", nil)
	if code != fuse.OK {
		t.Fatalf("GetAttr() after replacement = %v", code)
	}
	if pathAttr.Ino != initialAttr.Ino {
		t.Fatalf("path inode changed from %d to %d", initialAttr.Ino, pathAttr.Ino)
	}
	buffer := make([]byte, 4)
	result, code := handle.Read(buffer, 4)
	if code != fuse.OK {
		t.Fatalf("Read() after replacement = %v", code)
	}
	content, code := result.Bytes(buffer)
	if code != fuse.OK || string(content) != "new\n" {
		t.Fatalf("Read() at prior EOF = %q, %v", content, code)
	}
	if len(invalidator.paths) != 1 || invalidator.paths[0] != "annex.txt" {
		t.Fatalf("refreshed annex target invalidated paths %q", invalidator.paths)
	}
}

func TestTrackedFileGetAttrDoesNotWaitForWorkTreeUpdates(t *testing.T) {
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

	done := make(chan struct{})
	file := &trackedFile{
		File:       &attrFile{File: nodefs.NewDefaultFile()},
		filesystem: filesystem,
		path:       "annex.txt",
	}
	go func() {
		file.GetAttr(&fuse.Attr{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("read-only handle attributes waited for the repository lock")
	}
	close(release)
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
