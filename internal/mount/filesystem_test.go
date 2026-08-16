package mount

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/core"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/store"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type recordingNotifier struct {
	mu     sync.Mutex
	notify []string
	begins int
	ends   int
}

func (n *recordingNotifier) Notify(reason string) {
	n.mu.Lock()
	n.notify = append(n.notify, reason)
	n.mu.Unlock()
}
func (n *recordingNotifier) BeginWrite() { n.mu.Lock(); n.begins++; n.mu.Unlock() }
func (n *recordingNotifier) EndWrite()   { n.mu.Lock(); n.ends++; n.mu.Unlock() }

func testFileSystem(t testing.TB, root string) (*FileSystem, *repository.Repository, *core.Service) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".git", "dfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, ".git", "dfs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	repo := &repository.Repository{Config: config.Default("mount-test", root), Store: state}
	service := core.New(repo, core.Options{})
	t.Cleanup(func() { _ = service.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewFileSystem(service, nil, logger), repo, service
}

func TestStatusDoesNotReportGenericFailuresAsUnimplemented(t *testing.T) {
	if got := status(errors.New("transfer failed")); got != fuse.EIO {
		t.Fatalf("generic error status = %v, want EIO", got)
	}
	if got := status(&core.Error{Code: core.CodeNotSupported, Op: "test"}); got != fuse.ENOSYS {
		t.Fatalf("unsupported status = %v, want ENOSYS", got)
	}
}

func TestPrivateCoreStateIsNeverVisible(t *testing.T) {
	filesystem, _, _ := testFileSystem(t, t.TempDir())
	for _, path := range []string{".git", ".git/dfs", "folder/.git/config"} {
		if _, code := filesystem.GetAttr(path, nil); code != fuse.ENOENT {
			t.Errorf("GetAttr(%q) = %v, want ENOENT", path, code)
		}
	}
	entries, code := filesystem.OpenDir("", nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	for _, entry := range entries {
		if entry.Name == ".git" {
			t.Fatal("private repository metadata appeared in root directory")
		}
	}
}

func TestCreateIsVisibleBeforeCloseAndPublishedAtomically(t *testing.T) {
	root := t.TempDir()
	filesystem, _, _ := testFileSystem(t, root)
	handle, code := filesystem.Create("new.txt", syscall.O_WRONLY|syscall.O_EXCL, 0o640, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := filesystem.GetAttr("new.txt", nil); code != fuse.OK {
		t.Fatalf("staged GetAttr = %v", code)
	}
	entries, code := filesystem.OpenDir("", nil)
	if code != fuse.OK || len(entries) != 1 || entries[0].Name != "new.txt" {
		t.Fatalf("staged directory = %#v, %v", entries, code)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted backing file visible: %v", err)
	}
	if written, code := handle.Write([]byte("complete"), 0); code != fuse.OK || written != 8 {
		t.Fatalf("write = %d, %v", written, code)
	}
	handle.Release()
	if content, err := os.ReadFile(filepath.Join(root, "new.txt")); err != nil || string(content) != "complete" {
		t.Fatalf("published content = %q, %v", content, err)
	}
}

func TestMultipleHandlesShareOneWriteUntilFinalRelease(t *testing.T) {
	root := t.TempDir()
	filesystem, _, _ := testFileSystem(t, root)
	first, code := filesystem.Create("shared.txt", syscall.O_RDWR, 0o644, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	second, code := filesystem.Open("shared.txt", syscall.O_RDWR, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := first.Write([]byte("first"), 0); code != fuse.OK {
		t.Fatal(code)
	}
	first.Release()
	if _, err := os.Stat(filepath.Join(root, "shared.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first release published shared transaction: %v", err)
	}
	if _, code := second.Write([]byte("second"), 5); code != fuse.OK {
		t.Fatal(code)
	}
	second.Release()
	if content, err := os.ReadFile(filepath.Join(root, "shared.txt")); err != nil || string(content) != "firstsecond" {
		t.Fatalf("shared content = %q, %v", content, err)
	}
}

func TestRenameAndDeleteOpenCreate(t *testing.T) {
	root := t.TempDir()
	filesystem, _, _ := testFileSystem(t, root)
	handle, code := filesystem.Create("before.txt", syscall.O_WRONLY, 0o644, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := handle.Write([]byte("renamed"), 0); code != fuse.OK {
		t.Fatal(code)
	}
	if code := filesystem.Rename("before.txt", "after.txt", nil); code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := filesystem.GetAttr("before.txt", nil); code == fuse.OK {
		t.Fatal("old staged name remains visible")
	}
	if _, code := filesystem.GetAttr("after.txt", nil); code != fuse.OK {
		t.Fatalf("new staged name = %v", code)
	}
	handle.Release()
	if content, err := os.ReadFile(filepath.Join(root, "after.txt")); err != nil || string(content) != "renamed" {
		t.Fatalf("renamed publish = %q, %v", content, err)
	}
	deleted, code := filesystem.Create("deleted.txt", syscall.O_WRONLY, 0o644, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := deleted.Write([]byte("discard"), 0); code != fuse.OK {
		t.Fatal(code)
	}
	if code := filesystem.Unlink("deleted.txt", nil); code != fuse.OK {
		t.Fatal(code)
	}
	deleted.Release()
	if _, err := os.Stat(filepath.Join(root, "deleted.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted open create was published: %v", err)
	}
}

func TestReadAndMetadataOperationsUseCore(t *testing.T) {
	root := t.TempDir()
	filesystem, _, _ := testFileSystem(t, root)
	if err := os.WriteFile(filepath.Join(root, "item.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	handle, code := filesystem.Open("item.txt", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	buffer := make([]byte, 7)
	result, code := handle.Read(buffer, 0)
	if code != fuse.OK {
		t.Fatal(code)
	}
	data, code := result.Bytes(buffer)
	if code != fuse.OK || string(data) != "content" {
		t.Fatalf("read = %q, %v", data, code)
	}
	handle.Release()
	if code := filesystem.Chmod("item.txt", 0o444, nil); code != fuse.OK {
		t.Fatal(code)
	}
	attr, code := filesystem.GetAttr("item.txt", nil)
	if code != fuse.OK || attr.Mode&0o777 != 0o444 {
		t.Fatalf("mode = %o, %v", attr.Mode, code)
	}
	if code := filesystem.SetXAttr("item.txt", "user.test", []byte("value"), 0, nil); code != fuse.OK {
		t.Fatal(code)
	}
	if value, code := filesystem.GetXAttr("item.txt", "user.test", nil); code != fuse.OK || string(value) != "value" {
		t.Fatalf("xattr = %q, %v", value, code)
	}
	// A read-only file remains deletable because deletion is a parent-directory
	// mutation, matching the Dolphin failure that motivated this regression.
	if code := filesystem.Unlink("item.txt", nil); code != fuse.OK {
		t.Fatalf("delete read-only file = %v", code)
	}
}

func TestOpenReadHandleSurvivesRenameAndUnlink(t *testing.T) {
	root := t.TempDir()
	filesystem, _, _ := testFileSystem(t, root)
	path := filepath.Join(root, "item.txt")
	if err := os.WriteFile(path, []byte("stable snapshot"), 0o640); err != nil {
		t.Fatal(err)
	}
	handle, code := filesystem.Open("item.txt", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if code := filesystem.Rename("item.txt", "renamed.txt", nil); code != fuse.OK {
		t.Fatal(code)
	}
	var attributes fuse.Attr
	if code := handle.GetAttr(&attributes); code != fuse.OK || attributes.Size != uint64(len("stable snapshot")) {
		t.Fatalf("renamed handle attributes = %#v, %v", attributes, code)
	}
	if code := filesystem.Unlink("renamed.txt", nil); code != fuse.OK {
		t.Fatal(code)
	}
	buffer := make([]byte, len("stable snapshot"))
	result, code := handle.Read(buffer, 0)
	if code != fuse.OK {
		t.Fatal(code)
	}
	content, code := result.Bytes(buffer)
	if code != fuse.OK || string(content) != "stable snapshot" {
		t.Fatalf("unlinked handle content = %q, %v", content, code)
	}
	handle.Release()
}

func TestVisibleMetadataSurvivesAnnexBackingReplacement(t *testing.T) {
	root := t.TempDir()
	filesystem, _, _ := testFileSystem(t, root)
	handle, code := filesystem.Create("annexed.bin", syscall.O_WRONLY, 0o640, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := handle.Write([]byte("annex content"), 0); code != fuse.OK {
		t.Fatal(code)
	}
	handle.Release()
	key := "SHA256E-s13--" + strings.Repeat("a", 64)
	object := filepath.Join(root, ".git", "annex", "objects", "AA", "BB", key, key)
	if err := os.MkdirAll(filepath.Dir(object), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, []byte("annex content"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "annexed.bin")); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(root, object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "annexed.bin")); err != nil {
		t.Fatal(err)
	}
	attributes, code := filesystem.GetAttr("annexed.bin", nil)
	if code != fuse.OK || attributes.Mode&0o777 != 0o640 || attributes.Size != 13 {
		t.Fatalf("logical annex attributes = mode %o size %d, %v", attributes.Mode, attributes.Size, code)
	}
	if code := filesystem.Unlink("annexed.bin", nil); code != fuse.OK {
		t.Fatalf("delete annex with visible metadata = %v", code)
	}
}

func TestFsyncPublishesAndHandleRemainsWritable(t *testing.T) {
	root := t.TempDir()
	filesystem, _, _ := testFileSystem(t, root)
	handle, code := filesystem.Create("checkpoint.txt", syscall.O_RDWR, 0o644, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := handle.Write([]byte("one"), 0); code != fuse.OK {
		t.Fatal(code)
	}
	if code := handle.Fsync(0); code != fuse.OK {
		t.Fatal(code)
	}
	if content, err := os.ReadFile(filepath.Join(root, "checkpoint.txt")); err != nil || string(content) != "one" {
		t.Fatalf("checkpoint = %q, %v", content, err)
	}
	if _, code := handle.Write([]byte("two"), 3); code != fuse.OK {
		t.Fatal(code)
	}
	handle.Release()
	if content, err := os.ReadFile(filepath.Join(root, "checkpoint.txt")); err != nil || string(content) != "onetwo" {
		t.Fatalf("post-checkpoint write = %q, %v", content, err)
	}
}

func TestNotifierBalancesWriteLifetime(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "dfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, ".git", "dfs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	repo := &repository.Repository{Config: config.Default("notify", root), Store: state}
	service := core.New(repo, core.Options{})
	defer service.Close()
	notifier := &recordingNotifier{}
	filesystem := NewFileSystem(service, notifier, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handle, code := filesystem.Create("notify.txt", syscall.O_WRONLY, 0o644, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	if _, code := handle.Write([]byte("x"), 0); code != fuse.OK {
		t.Fatal(code)
	}
	handle.Release()
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if notifier.begins != 1 || notifier.ends != 1 || len(notifier.notify) != 1 {
		t.Fatalf("notifier = begins %d ends %d notifications %v", notifier.begins, notifier.ends, notifier.notify)
	}
}

func TestAdapterCancellationStopsRemoteRangeRead(t *testing.T) {
	root := t.TempDir()
	filesystem, repo, _ := testFileSystem(t, root)
	const size = 8 << 20
	key := "SHA256E-s8388608--" + strings.Repeat("0", 64)
	target := filepath.Join(".git", "annex", "objects", "AA", "BB", key, key)
	if err := os.Symlink(target, filepath.Join(root, "remote.bin")); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	repo.SetManagedRangeFetcher(func(ctx context.Context, _ *repository.Repository, _ string, _, _ int64, _ io.Writer) (int64, error) {
		close(started)
		<-ctx.Done()
		return size, ctx.Err()
	})
	lifetime, cancel := context.WithCancel(context.Background())
	filesystem.lifetime = lifetime
	handle, code := filesystem.Open("remote.bin", syscall.O_RDONLY, nil)
	if code != fuse.OK {
		t.Fatal(code)
	}
	finished := make(chan fuse.Status, 1)
	go func() { _, code := handle.Read(make([]byte, 1), 0); finished <- code }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("range did not start")
	}
	cancel()
	select {
	case code := <-finished:
		if code != fuse.EINTR {
			t.Fatalf("canceled read = %v", code)
		}
	case <-time.After(time.Second):
		t.Fatal("range did not cancel")
	}
	handle.Release()
}

func TestFilesystemAdapterSourceDoesNotImportRepositoryOrTransport(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test")
	}
	content, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "filesystem.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, forbidden := range []string{"internal/repository", "internal/managed", "internal/peer", "git-annex", "exec.Command"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("FUSE adapter bypasses core API through %q", forbidden)
		}
	}
}
