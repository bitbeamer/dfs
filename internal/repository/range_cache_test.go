package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/store"
)

func rangeTestRepository(t *testing.T, root string, limit int64) *Repository {
	t.Helper()
	stateDirectory := filepath.Join(root, ".git", "dfs")
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(filepath.Join(stateDirectory, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default("range-test", root)
	cfg.CacheLimit = limit
	repo := &Repository{Config: cfg, Store: state}
	t.Cleanup(func() {
		_ = repo.WaitForRangeTasks(context.Background())
		_ = state.Close()
	})
	return repo
}

func rangeTestKey(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "SHA256E-s" + strconv.FormatInt(int64(len(payload)), 10) + "--" + hex.EncodeToString(digest[:]) + ".bin"
}

func payloadFetcher(payload []byte, calls *atomic.Int32) ManagedRangeFetcher {
	return func(_ context.Context, _ *Repository, _ string, offset, length int64, output io.Writer) (int64, error) {
		calls.Add(1)
		_, err := output.Write(payload[offset : offset+length])
		return int64(len(payload)), err
	}
}

func TestSparseRangeCacheSupportsSequentialReadsAndResume(t *testing.T) {
	root := t.TempDir()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 768<<10) // 12 MiB
	key := rangeTestKey(payload)
	var calls atomic.Int32
	repo := rangeTestRepository(t, root, 32<<20)
	repo.SetManagedRangeFetcher(payloadFetcher(payload, &calls))

	for _, offset := range []int64{0, 4 << 20} {
		buffer := make([]byte, 128<<10)
		n, err := repo.ReadRange(context.Background(), "movie.bin", key, int64(len(payload)), offset, buffer)
		if err != nil || n != len(buffer) || !bytes.Equal(buffer, payload[offset:offset+int64(n)]) {
			t.Fatalf("sequential read at %d = %d, %v", offset, n, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("demand transfers = %d, want 2", got)
	}
	if err := repo.WaitForRangeTasks(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A new Repository represents a daemon restart. Reading an already cached
	// extent must resume from peer-private metadata without another transfer.
	restarted := rangeTestRepository(t, root, 32<<20)
	restarted.SetManagedRangeFetcher(payloadFetcher(payload, &calls))
	buffer := make([]byte, 4096)
	if _, err := restarted.ReadRange(context.Background(), "movie.bin", key, int64(len(payload)), (4<<20)+(64<<10), buffer); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("resumed cache transfers = %d, want 2", got)
	}
}

func TestSparseRangeCacheRecoversAfterAnotherProcessDiscardsPartial(t *testing.T) {
	payload := bytes.Repeat([]byte("cross-process-range"), 64<<10)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	key := rangeTestKey(payload)
	var calls atomic.Int32
	repo.SetManagedRangeFetcher(payloadFetcher(payload, &calls))
	buffer := make([]byte, 4096)
	if _, err := repo.ReadRange(context.Background(), "shared.bin", key, int64(len(payload)), 0, buffer); err != nil {
		t.Fatal(err)
	}
	if err := repo.WaitForRangeTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	partial, metadata := repo.rangeCachePaths(key)
	for _, path := range []string{partial, metadata} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	clear(buffer)
	if _, err := repo.ReadRange(context.Background(), "shared.bin", key, int64(len(payload)), 0, buffer); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer, payload[:len(buffer)]) || calls.Load() != 2 {
		t.Fatalf("recovered read calls = %d, data matches = %v", calls.Load(), bytes.Equal(buffer, payload[:len(buffer)]))
	}
}

func TestSparseRangeCacheSupportsFinderQuickLookAndRandomSeeks(t *testing.T) {
	payload := bytes.Repeat([]byte("preview-content-"), 1280<<10)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	var calls atomic.Int32
	repo.SetManagedRangeFetcher(payloadFetcher(payload, &calls))
	key := rangeTestKey(payload)
	// Finder and Quick Look commonly inspect the header, trailer, then a small
	// random media extent without consuming the entire object.
	for _, offset := range []int64{0, int64(len(payload)) - 8192, 10<<20 + 12345} {
		buffer := make([]byte, 4096)
		n, err := repo.ReadRange(context.Background(), "preview.mov", key, int64(len(payload)), offset, buffer)
		if err != nil || !bytes.Equal(buffer[:n], payload[offset:offset+int64(n)]) {
			t.Fatalf("preview read at %d = %d, %v", offset, n, err)
		}
	}
	if calls.Load() >= 4 {
		t.Fatalf("preview issued %d transfers; sparse read-ahead was not reused", calls.Load())
	}
}

func TestConcurrentDuplicateNamesCoalesceAnnexRange(t *testing.T) {
	payload := bytes.Repeat([]byte("same-object"), 1<<20)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	key := rangeTestKey(payload)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	repo.SetManagedRangeFetcher(func(_ context.Context, _ *Repository, _ string, offset, length int64, output io.Writer) (int64, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, err := output.Write(payload[offset : offset+length])
		return int64(len(payload)), err
	})

	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for _, path := range []string{"first/movie.bin", "duplicate/movie.bin"} {
		wait.Add(1)
		go func(path string) {
			defer wait.Done()
			_, err := repo.ReadRange(context.Background(), path, key, int64(len(payload)), 1024, make([]byte, 4096))
			errorsSeen <- err
		}(path)
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("duplicate-name transfers = %d, want 1", got)
	}
}

func TestNonOverlappingAnnexRangesFetchConcurrently(t *testing.T) {
	payload := make([]byte, 8<<20)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	key := rangeTestKey(payload)
	started := make(chan int64, 2)
	release := make(chan struct{})
	repo.SetManagedRangeFetcher(func(_ context.Context, _ *Repository, _ string, offset, length int64, output io.Writer) (int64, error) {
		started <- offset
		<-release
		_, err := output.Write(payload[offset : offset+length])
		return int64(len(payload)), err
	})

	errorsSeen := make(chan error, 2)
	for _, offset := range []int64{0, 4 << 20} {
		go func(offset int64) {
			_, err := repo.ReadRange(context.Background(), "movie.bin", key, int64(len(payload)), offset, make([]byte, 4096))
			errorsSeen <- err
		}(offset)
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("non-overlapping range was serialized")
		}
	}
	close(release)
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatal(err)
		}
	}
}

func TestSmallRandomReadUsesDemandSizedForegroundWindow(t *testing.T) {
	payload := make([]byte, 8<<20)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	var requested atomic.Int64
	repo.SetManagedRangeFetcher(func(_ context.Context, _ *Repository, _ string, offset, length int64, output io.Writer) (int64, error) {
		requested.Store(length)
		_, err := output.Write(payload[offset : offset+length])
		return int64(len(payload)), err
	})
	if _, err := repo.ReadRange(context.Background(), "preview.mov", rangeTestKey(payload), int64(len(payload)), 200<<10, make([]byte, 128<<10)); err != nil {
		t.Fatal(err)
	}
	if got := requested.Load(); got > rangeDemandQuantum {
		t.Fatalf("foreground fetch = %d bytes, want at most %d", got, rangeDemandQuantum)
	}
}

func TestRangeReadCancellationStopsTransfer(t *testing.T) {
	payload := make([]byte, 8<<20)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	started := make(chan struct{})
	repo.SetManagedRangeFetcher(func(ctx context.Context, _ *Repository, _ string, _, _ int64, _ io.Writer) (int64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := repo.ReadRange(ctx, "cancel.bin", rangeTestKey(payload), int64(len(payload)), 0, make([]byte, 4096))
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v", err)
	}
}

func TestRangeReadCancellationStopsBackgroundReadAhead(t *testing.T) {
	payload := make([]byte, 8<<20)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	readAheadStarted := make(chan struct{})
	repo.SetManagedRangeFetcher(func(ctx context.Context, _ *Repository, _ string, offset, length int64, output io.Writer) (int64, error) {
		if offset == 0 {
			_, err := output.Write(payload[:length])
			return int64(len(payload)), err
		}
		close(readAheadStarted)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	key := rangeTestKey(payload)
	if _, err := repo.ReadRange(ctx, "sequential.bin", key, int64(len(payload)), 0, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReadRange(ctx, "sequential.bin", key, int64(len(payload)), 4096, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readAheadStarted:
	case <-time.After(time.Second):
		t.Fatal("sequential read-ahead did not start")
	}
	cancel()
	waitCtx, stopWaiting := context.WithTimeout(context.Background(), time.Second)
	defer stopWaiting()
	if err := repo.WaitForRangeTasks(waitCtx); err != nil {
		t.Fatalf("canceled read-ahead did not stop: %v", err)
	}
}

func TestPartialRangeCacheEvictsOldestInactiveObject(t *testing.T) {
	root := t.TempDir()
	repo := rangeTestRepository(t, root, rangeDemandQuantum)
	payload := make([]byte, 8<<20)
	var calls atomic.Int32
	repo.SetManagedRangeFetcher(payloadFetcher(payload, &calls))
	firstKey := rangeTestKey(append([]byte("first"), payload...))
	secondPayload := append([]byte("second"), payload...)
	secondKey := rangeTestKey(secondPayload)
	// Use matching source sizes while keeping two distinct keys.
	firstPayload := append([]byte("first"), payload...)
	repo.SetManagedRangeFetcher(payloadFetcher(firstPayload, &calls))
	if _, err := repo.ReadRange(context.Background(), "first.bin", firstKey, int64(len(firstPayload)), 0, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if err := repo.WaitForRangeTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstPartial, firstMetadata := repo.rangeCachePaths(firstKey)
	repo.SetManagedRangeFetcher(payloadFetcher(secondPayload, &calls))
	if _, err := repo.ReadRange(context.Background(), "second.bin", secondKey, int64(len(secondPayload)), 0, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{firstPartial, firstMetadata} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("old partial cache %s still exists: %v", filepath.Base(path), err)
		}
	}
}

func TestRangeReadFallsBackToBoundedDurableHydration(t *testing.T) {
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
			if err == nil {
				if entry.IsDir() {
					_ = os.Chmod(path, 0o755)
				} else {
					_ = os.Chmod(path, 0o644)
				}
			}
			return nil
		})
	})
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Range Test\nemail = range@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo, err := Init(ctx, filepath.Join(home, "repo"), "range-test", 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	payload := bytes.Repeat([]byte("durable-content"), 64<<10)
	path := filepath.Join(repo.Config.Repository, "archive.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CommitPending(ctx, "Add durable fallback fixture"); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(home, "storage")
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.runner.Run(ctx, "git", "annex", "initremote", "durable", "type=directory", "directory="+storage, "encryption=none"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.runner.Run(ctx, "git", "annex", "copy", "--to=durable", "--", "archive.bin"); err != nil {
		t.Fatal(err)
	}
	key, err := repo.LookupKey(ctx, "archive.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.runner.Run(ctx, "git", "annex", "drop", "--force", "--", "archive.bin"); err != nil {
		t.Fatal(err)
	}
	repo.SetManagedRangeFetcher(func(context.Context, *Repository, string, int64, int64, io.Writer) (int64, error) {
		return 0, ErrContentUnavailable
	})
	buffer := make([]byte, 4096)
	n, err := repo.ReadRange(ctx, "archive.bin", key, int64(len(payload)), 1024, buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer[:n], payload[1024:1024+n]) {
		t.Fatal("durable fallback returned incorrect bytes")
	}
	if diagnostics := repo.ContentReadDiagnostics(); diagnostics.LastPlan != "durable-full-hydration" || diagnostics.LastOutcome != "ready" {
		t.Fatalf("durable fallback diagnostics = %#v", diagnostics)
	}
}

func TestContentUnavailableReasonSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("read failed: %w", &ContentUnavailableError{Reason: AvailabilityKnownHoldersOffline, Detail: "iris is offline"})
	if !errors.Is(err, ErrContentUnavailable) {
		t.Fatal("typed availability error does not wrap the public sentinel")
	}
	if reason := ContentAvailabilityReason(err); reason != AvailabilityKnownHoldersOffline {
		t.Fatalf("availability reason = %q", reason)
	}
}

func TestRangeReadDoesNotTryDurableFallbackWhenPeersAreKnownOffline(t *testing.T) {
	payload := bytes.Repeat([]byte("offline-content"), 1024)
	repo := rangeTestRepository(t, t.TempDir(), 32<<20)
	repo.SetManagedRangeFetcher(func(context.Context, *Repository, string, int64, int64, io.Writer) (int64, error) {
		return 0, &ContentUnavailableError{Reason: AvailabilityKnownHoldersOffline, Detail: "known holder is offline"}
	})
	_, err := repo.ReadRange(context.Background(), "offline.bin", rangeTestKey(payload), int64(len(payload)), 0, make([]byte, 512))
	if reason := ContentAvailabilityReason(err); reason != AvailabilityKnownHoldersOffline {
		t.Fatalf("range read reason = %q (%v), want %q", reason, err, AvailabilityKnownHoldersOffline)
	}
	if diagnostics := repo.ContentReadDiagnostics(); diagnostics.LastPlan == "durable-full-hydration" {
		t.Fatalf("known-offline range read attempted durable fallback: %#v", diagnostics)
	}
}
