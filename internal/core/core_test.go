package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/store"
)

func testService(t *testing.T, retention int) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "dfs"), 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(root, ".git", "dfs", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo := &repository.Repository{Config: config.Default("core-test", root), Store: state}
	service := New(repo, Options{EventRetention: retention})
	t.Cleanup(func() { _ = service.Close(); _ = state.Close() })
	return service, root
}

// contractFrontend intentionally knows only API. It is the minimal reference
// frontend used by the contract suite before any platform adapter is involved.
type contractFrontend struct{ core API }

func (f contractFrontend) put(ctx context.Context, path string, content []byte, operationID string) error {
	transaction, err := f.core.BeginWrite(ctx, WriteRequest{Path: path, Mode: 0o644, Create: true, Exclusive: true, OperationID: operationID})
	if err != nil {
		return err
	}
	defer transaction.Close()
	cut := len(content) / 2
	if _, err := transaction.WriteAt(content[cut:], int64(cut)); err != nil {
		return err
	}
	if _, err := transaction.WriteAt(content[:cut], 0); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (f contractFrontend) read(ctx context.Context, path string) ([]byte, bool, error) {
	handle, err := f.core.OpenRead(ctx, path)
	if err != nil {
		return nil, false, err
	}
	defer handle.Close()
	data := make([]byte, handle.Size())
	n, err := handle.ReadAt(data, 0)
	if errors.Is(err, io.EOF) && n == len(data) {
		err = nil
	}
	_, direct := handle.FileDescriptor()
	return data[:n], direct, err
}

func TestAPIContractThroughFrontend(t *testing.T) {
	service, _ := testService(t, 32)
	frontend := contractFrontend{core: service}
	ctx := context.Background()
	if err := service.MakeDirectory(ctx, "Documents", 0o755, "mkdir-1"); err != nil {
		t.Fatal(err)
	}
	content := []byte("partial writes are assembled atomically")
	if err := frontend.put(ctx, "Documents/item.txt", content, "write-1"); err != nil {
		t.Fatal(err)
	}
	entry, err := service.Lookup(ctx, "Documents/item.txt")
	if err != nil || entry.Attributes.Kind != KindFile || entry.Attributes.Size != int64(len(content)) {
		t.Fatalf("lookup = %#v, %v", entry, err)
	}
	page, err := service.ReadDirectory(ctx, "Documents", PageRequest{Limit: 1})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Name != "item.txt" {
		t.Fatalf("directory page = %#v, %v", page, err)
	}
	read, direct, err := frontend.read(ctx, "Documents/item.txt")
	if err != nil || !direct || !bytes.Equal(read, content) {
		t.Fatalf("read = %q direct=%v err=%v", read, direct, err)
	}
	if err := service.SetXattr(ctx, "Documents/item.txt", "user.test", []byte("value"), XattrCreate, "xattr-1"); err != nil {
		t.Fatal(err)
	}
	if value, err := service.GetXattr(ctx, "Documents/item.txt", "user.test"); err != nil || string(value) != "value" {
		t.Fatalf("xattr = %q, %v", value, err)
	}
	if err := service.Rename(ctx, "Documents/item.txt", "Documents/renamed.txt", "rename-1"); err != nil {
		t.Fatal(err)
	}
	if err := service.Remove(ctx, "Documents/renamed.txt", false, "delete-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Lookup(ctx, "Documents/renamed.txt"); !IsErrorCode(err, CodeNotFound) {
		t.Fatalf("deleted lookup = %v", err)
	}
}

func TestLookupPreservesUserSymlinkIdentity(t *testing.T) {
	service, root := testService(t, 16)
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := service.Symlink(context.Background(), "target.txt", "link", "symlink-1"); err != nil {
		t.Fatal(err)
	}
	entry, err := service.Lookup(context.Background(), "link")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Attributes.Kind != KindSymlink || entry.Attributes.Mode&syscall.S_IFMT != syscall.S_IFLNK {
		t.Fatalf("symlink attributes = %#v", entry.Attributes)
	}
	if target, err := service.ReadLink(context.Background(), "link"); err != nil || target != "target.txt" {
		t.Fatalf("read link = %q, %v", target, err)
	}
}

func TestWriteCommitIsAtomicAndRetryIsIdempotent(t *testing.T) {
	service, root := testService(t, 16)
	ctx := context.Background()
	frontend := contractFrontend{core: service}
	transaction, err := service.BeginWrite(ctx, WriteRequest{Path: "atomic.txt", Create: true, Exclusive: true, OperationID: "atomic-op"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.WriteAt([]byte("new value"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "atomic.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted destination is visible: %v", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	retry, err := service.BeginWrite(ctx, WriteRequest{Path: "atomic.txt", Create: true, Exclusive: true, OperationID: "atomic-op"})
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if _, err := retry.WriteAt([]byte("ignored"), 0); err != nil {
		t.Fatal(err)
	}
	if err := retry.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	content, _, err := frontend.read(ctx, "atomic.txt")
	if err != nil || string(content) != "new value" {
		t.Fatalf("retry changed content: %q, %v", content, err)
	}
	if _, err := service.BeginWrite(ctx, WriteRequest{Path: "other.txt", Create: true, OperationID: "atomic-op"}); !IsErrorCode(err, CodeConflict) {
		t.Fatalf("operation ID reuse = %v", err)
	}
}

func TestFailedWritePublishQuarantinesStagedPayload(t *testing.T) {
	service, root := testService(t, 16)
	if err := os.Mkdir(filepath.Join(root, "parent"), 0o755); err != nil {
		t.Fatal(err)
	}
	transaction, err := service.BeginWrite(context.Background(), WriteRequest{Path: "parent/item.txt", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.WriteAt([]byte("recover me"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "parent")); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(context.Background()); err == nil {
		t.Fatal("commit unexpectedly succeeded after parent removal")
	}
	matches, err := filepath.Glob(filepath.Join(root, ".git", "dfs", "recovery", "*", "writes", "write-*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("quarantined writes = %q, %v", matches, err)
	}
	payload, err := os.ReadFile(matches[0])
	if err != nil || string(payload) != "recover me" {
		t.Fatalf("quarantined payload = %q, %v", payload, err)
	}
}

func TestConcurrentIdempotentMutationExecutesOnce(t *testing.T) {
	service, _ := testService(t, 16)
	ctx := context.Background()
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsSeen <- service.MakeDirectory(ctx, "once", 0o755, "same-operation")
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("idempotent concurrent mutation = %v", err)
		}
	}
}

func TestEventsAreOrderedResumableAndDoNotBlockOnSlowConsumer(t *testing.T) {
	service, _ := testService(t, 3)
	ctx := context.Background()
	subscription, err := service.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	if err := service.MakeDirectory(ctx, "initial", 0o755, "op-initial"); err != nil {
		t.Fatal(err)
	}
	if _, err := subscription.Next(ctx); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		if err := service.MakeDirectory(ctx, fmt.Sprintf("dir-%d", index), 0o755, fmt.Sprintf("op-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := subscription.Next(ctx); !IsErrorCode(err, CodeEventGone) {
		t.Fatalf("slow subscriber = %v", err)
	}
	resumed, err := service.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := resumed.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resumed.Next(ctx)
	if err != nil || second.Cursor != first.Cursor+1 {
		t.Fatalf("event order = %d then %d, %v", first.Cursor, second.Cursor, err)
	}
	after, err := service.Subscribe(ctx, first.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := after.Next(ctx)
	if err != nil || replayed.Cursor != second.Cursor {
		t.Fatalf("resumed event = %#v, %v", replayed, err)
	}
}

func TestContextCancellationAndErrorTaxonomy(t *testing.T) {
	service, _ := testService(t, 4)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Lookup(canceled, "anything"); !IsErrorCode(err, CodeCanceled) {
		t.Fatalf("canceled lookup = %v", err)
	}
	if _, err := service.Lookup(context.Background(), ".git/dfs/state.db"); !IsErrorCode(err, CodePermission) {
		t.Fatalf("private path lookup = %v", err)
	}
	if _, err := service.Lookup(context.Background(), "missing"); !IsErrorCode(err, CodeNotFound) {
		t.Fatalf("missing lookup = %v", err)
	}
	transaction, err := service.BeginWrite(context.Background(), WriteRequest{Path: "partial", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.WriteAt([]byte("discarded"), 0); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Lookup(context.Background(), "partial"); !IsErrorCode(err, CodeNotFound) {
		t.Fatalf("aborted write became visible: %v", err)
	}
}

func TestAdvisoryLockContractBlocksAndResumes(t *testing.T) {
	service, _ := testService(t, 4)
	ctx := context.Background()
	requested := FileLock{Start: 0, End: ^uint64(0), Kind: LockWrite, PID: 10}
	if err := service.SetLock(ctx, "locked.txt", 1, requested, false); err != nil {
		t.Fatal(err)
	}
	if conflict, err := service.GetLock(ctx, "locked.txt", 2, requested); err != nil || conflict.Kind != LockWrite || conflict.PID != 10 {
		t.Fatalf("conflicting lock = %#v, %v", conflict, err)
	}
	if err := service.SetLock(ctx, "locked.txt", 2, requested, false); !IsErrorCode(err, CodeConflict) {
		t.Fatalf("nonblocking conflict = %v", err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- service.SetLock(ctx, "locked.txt", 2, requested, true) }()
	select {
	case err := <-acquired:
		t.Fatalf("blocking lock returned early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlocked := requested
	unlocked.Kind = LockUnlocked
	if err := service.SetLock(ctx, "locked.txt", 1, unlocked, false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking lock did not resume")
	}
}

func TestLargeFilePartialIOAndMultiFilesystemIsolation(t *testing.T) {
	first, _ := testService(t, 8)
	second, _ := testService(t, 8)
	ctx := context.Background()
	const size = 32 << 20
	transaction, err := first.BeginWrite(ctx, WriteRequest{Path: "large.bin", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Truncate(size); err != nil {
		t.Fatal(err)
	}
	marker := []byte("range-marker")
	if n, err := transaction.WriteAt(marker, size-int64(len(marker))); err != nil || n != len(marker) {
		t.Fatalf("partial write = %d, %v", n, err)
	}
	if err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	handle, err := first.OpenRead(ctx, "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	buffer := make([]byte, len(marker))
	if n, err := handle.ReadAt(buffer, size-int64(len(marker))); err != nil || n != len(marker) || !bytes.Equal(buffer, marker) {
		t.Fatalf("partial read = %q %d, %v", buffer, n, err)
	}
	if _, err := second.Lookup(ctx, "large.bin"); !IsErrorCode(err, CodeNotFound) {
		t.Fatalf("filesystem state crossed instances: %v", err)
	}
}

func TestRemoteRangeReadIsCallerPacedAndCloseCancels(t *testing.T) {
	service, root := testService(t, 8)
	const size = 8 << 20
	key := fmt.Sprintf("SHA256E-s%d--%064d", size, 0)
	target := filepath.Join(".git", "annex", "objects", "AA", "BB", key, key)
	if err := os.Symlink(target, filepath.Join(root, "remote.bin")); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	service.repo.SetManagedRangeFetcher(func(ctx context.Context, _ *repository.Repository, _ string, _, _ int64, _ io.Writer) (int64, error) {
		close(started)
		<-ctx.Done()
		return size, ctx.Err()
	})
	handle, err := service.OpenRead(context.Background(), "remote.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, direct := handle.FileDescriptor(); direct {
		t.Fatal("remote range unexpectedly exposed a local descriptor")
	}
	finished := make(chan error, 1)
	go func() { _, err := handle.ReadAt(make([]byte, 1), 0); finished <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("range read did not start")
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !IsErrorCode(err, CodeCanceled) {
			t.Fatalf("closed range read = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing range handle did not cancel transfer")
	}
}

func TestCachedAnnexReadUsesLocalDescriptorWithoutTransport(t *testing.T) {
	service, root := testService(t, 8)
	content := []byte("cached annex content")
	key := fmt.Sprintf("SHA256E-s%d--%064d", len(content), 0)
	object := filepath.Join(root, ".git", "annex", "objects", "AA", "BB", key, key)
	if err := os.MkdirAll(filepath.Dir(object), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(object, content, 0o444); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(root, object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "cached.bin")); err != nil {
		t.Fatal(err)
	}
	var transportCalls int
	service.repo.SetManagedRangeFetcher(func(context.Context, *repository.Repository, string, int64, int64, io.Writer) (int64, error) {
		transportCalls++
		return 0, errors.New("transport must not be used")
	})
	handle, err := service.OpenRead(context.Background(), "cached.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if _, direct := handle.FileDescriptor(); !direct || !handle.DirectIO() {
		t.Fatalf("cached annex provenance direct=%v directIO=%v", direct, handle.DirectIO())
	}
	buffer := make([]byte, len(content))
	if n, err := handle.ReadAt(buffer, 0); err != nil || n != len(content) || !bytes.Equal(buffer, content) {
		t.Fatalf("cached annex read = %q %d, %v", buffer, n, err)
	}
	if transportCalls != 0 {
		t.Fatalf("cached annex used transport %d times", transportCalls)
	}
}

func TestRealRepositoryPolicyHealthAndHistoryContract(t *testing.T) {
	if err := repository.CheckDependencies(); err != nil {
		t.Skip(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Core Test\nemail = core@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, err := repository.Init(ctx, filepath.Join(t.TempDir(), "repository"), "core-policy", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	t.Cleanup(func() {
		_ = filepath.Walk(repo.Config.Repository, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if info.IsDir() {
				return os.Chmod(path, 0o700)
			}
			return os.Chmod(path, 0o600)
		})
	})
	service := New(repo, Options{})
	defer service.Close()
	frontend := contractFrontend{core: service}
	if err := frontend.put(ctx, "policy.txt", []byte("policy content"), "policy-write"); err != nil {
		t.Fatal(err)
	}
	if committed, err := repo.CommitPending(ctx, "Add policy fixture"); err != nil || !committed {
		t.Fatalf("commit fixture = %v, %v", committed, err)
	}
	if err := service.SetPin(ctx, "policy.txt", PinLocal, true); err != nil {
		t.Fatal(err)
	}
	pins, err := service.Pins(ctx)
	if err != nil || len(pins) != 1 || pins[0] != (Pin{Path: "policy.txt", Scope: PinLocal}) {
		t.Fatalf("pins = %#v, %v", pins, err)
	}
	health, err := service.Health(ctx)
	if err != nil || health.LogicalFiles != 1 || health.LogicalBytes != int64(len("policy content")) || len(health.Pins) != 1 {
		t.Fatalf("health = %#v, %v", health, err)
	}
	revisions, err := service.History(ctx, "policy.txt")
	if err != nil || len(revisions) == 0 || revisions[0].ID == "" {
		t.Fatalf("history = %#v, %v", revisions, err)
	}
	if err := service.SetPin(ctx, "policy.txt", PinLocal, false); err != nil {
		t.Fatal(err)
	}
}
