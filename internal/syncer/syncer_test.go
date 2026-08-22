package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestEndWriteRequeuesDeferredReason(t *testing.T) {
	scheduler := &Scheduler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		events: make(chan string, 1),
	}
	scheduler.BeginWrite()
	scheduler.deferredReason = "completed write"
	scheduler.EndWrite()
	select {
	case reason := <-scheduler.events:
		if reason != "completed write" {
			t.Fatalf("deferred reason = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred synchronization was not requeued")
	}
}

func TestOpenWriterOnlyDefersWorktreeChangingPasses(t *testing.T) {
	for _, reason := range []string{"managed Git receive", "periodic", receiveAndPublishReason, maintenanceReceiveReason} {
		if !requiresWriterDrain(reason) {
			t.Errorf("%q does not wait for open writers", reason)
		}
	}
	for _, reason := range []string{"completed write", "rename", "pin policy changed"} {
		if requiresWriterDrain(reason) {
			t.Errorf("push-only reason %q unnecessarily waits for open writers", reason)
		}
	}
}

func TestContinuousEventsCannotStarveDebounce(t *testing.T) {
	runs := make(chan string, 4)
	scheduler := &Scheduler{
		interval:     time.Hour,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		events:       make(chan string, 128),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		syncOverride: func(reason string) { runs <- reason },
	}
	go scheduler.loop()
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		scheduler.Notify("filesystem change")
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case <-runs:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("continuous events postponed synchronization indefinitely")
	}
	close(scheduler.stop)
	<-scheduler.done
}

func TestShutdownDoesNotStartFreshSynchronization(t *testing.T) {
	runs := make(chan string, 1)
	scheduler := &Scheduler{
		interval:     time.Hour,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		events:       make(chan string, 1),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		syncOverride: func(reason string) { runs <- reason },
	}
	go scheduler.loop()
	close(scheduler.stop)
	<-scheduler.done
	select {
	case reason := <-runs:
		t.Fatalf("shutdown started a fresh %q synchronization", reason)
	default:
	}
}

func TestBeginWritePreemptsOutgoingSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := &Scheduler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		active: "completed write",
		cancel: cancel,
	}

	scheduler.BeginWrite()
	scheduler.EndWrite()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("BeginWrite did not preempt outgoing synchronization")
	}
}

func TestBeginWriteDoesNotPreemptReceivedMerge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler := &Scheduler{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		active: "managed Git receive",
		cancel: cancel,
	}

	scheduler.BeginWrite()
	scheduler.EndWrite()
	select {
	case <-ctx.Done():
		t.Fatal("BeginWrite preempted received merge")
	default:
	}
}

func TestPinAndPeerReconciliationEventsRunMaintenance(t *testing.T) {
	for _, reason := range []string{"pin policy changed", "peer requested membership reconciliation"} {
		if !isMaintenanceReason(reason) {
			t.Errorf("%q does not run reconciliation and pin hydration", reason)
		}
	}
	for _, reason := range []string{"filesystem change", "managed Git receive"} {
		if isMaintenanceReason(reason) {
			t.Fatalf("%q unexpectedly runs maintenance", reason)
		}
	}
}

func TestMergeReasonsPreservesReceiveAndPublishWork(t *testing.T) {
	for _, pair := range [][2]string{
		{"completed write", "managed Git receive"},
		{"managed Git receive", "completed write"},
		{receiveAndPublishReason, "rename"},
		{receiveAndPublishReason, "managed Git receive"},
	} {
		if got := mergeReasons(pair[0], pair[1]); got != receiveAndPublishReason {
			t.Errorf("mergeReasons(%q, %q) = %q, want %q", pair[0], pair[1], got, receiveAndPublishReason)
		}
	}
}

func TestMergeReasonsPreservesReceiveDuringMaintenance(t *testing.T) {
	for _, pair := range [][2]string{
		{"periodic", "managed Git receive"},
		{"managed Git receive", "startup"},
		{receiveAndPublishReason, "pin policy changed"},
		{maintenanceReceiveReason, "completed write"},
	} {
		if got := mergeReasons(pair[0], pair[1]); got != maintenanceReceiveReason {
			t.Errorf("mergeReasons(%q, %q) = %q, want %q", pair[0], pair[1], got, maintenanceReceiveReason)
		}
	}
}

func TestSyncUntilConvergedStopsImmediatelyWhenUnchanged(t *testing.T) {
	passes := 0
	tree := "initial"
	got, paths, err := syncUntilConverged(context.Background(), 4, 2,
		func(context.Context) (string, error) { return tree, nil },
		func(context.Context, string, string) ([]string, error) { return nil, nil },
		func(context.Context) error { passes++; return nil },
		nil,
	)
	if err != nil || got != 1 || passes != 1 || len(paths) != 0 {
		t.Fatalf("syncUntilConverged = passes %d/%d, error %v", got, passes, err)
	}
}

func TestSyncUntilConvergedSupportsSingleEventPass(t *testing.T) {
	tree := "before"
	got, paths, err := syncUntilConverged(context.Background(), 1, 0,
		func(context.Context) (string, error) { return tree, nil },
		func(context.Context, string, string) ([]string, error) { return []string{"new.txt"}, nil },
		func(context.Context) error { tree = "after"; return nil },
		nil,
	)
	if err != nil || got != 1 || len(paths) != 1 || paths[0] != "new.txt" {
		t.Fatalf("single event pass = passes %d, paths %q, error %v", got, paths, err)
	}
}

func TestSyncUntilConvergedReportsPathsChangedByFailedPass(t *testing.T) {
	tree := "before"
	_, paths, err := syncUntilConverged(context.Background(), 4, 2,
		func(context.Context) (string, error) { return tree, nil },
		func(_ context.Context, before, after string) ([]string, error) {
			return []string{"old.txt", "new.txt"}, nil
		},
		func(context.Context) error {
			tree = "after"
			return errors.New("push rejected")
		},
		nil,
	)
	if err == nil || len(paths) != 2 || paths[0] != "old.txt" || paths[1] != "new.txt" {
		t.Fatalf("failed synchronization returned paths %q, error %v", paths, err)
	}
}

func TestSyncUntilConvergedRequiresTwoStablePassesAfterChange(t *testing.T) {
	sequence := []string{"initial", "remote-change", "remote-change", "remote-change"}
	position := 0
	var invalidated []string
	got, paths, err := syncUntilConverged(context.Background(), 4, 2,
		func(context.Context) (string, error) { return sequence[position], nil },
		func(_ context.Context, before, after string) ([]string, error) {
			return []string{before + "-to-" + after}, nil
		},
		func(context.Context) error { position++; return nil },
		func(paths []string) { invalidated = append(invalidated, paths...) },
	)
	if err != nil || got != 3 || position != 3 || len(paths) != 1 || paths[0] != "initial-to-remote-change" || len(invalidated) != 1 || invalidated[0] != paths[0] {
		t.Fatalf("syncUntilConverged = passes %d, position %d, paths %q, invalidated %q, error %v", got, position, paths, invalidated, err)
	}
}

func TestSyncUntilConvergedFailsWhenTreeKeepsChanging(t *testing.T) {
	position := 0
	_, _, err := syncUntilConverged(context.Background(), 3, 2,
		func(context.Context) (string, error) { return string(rune('a' + position)), nil },
		func(context.Context, string, string) ([]string, error) { return nil, nil },
		func(context.Context) error { position++; return nil },
		nil,
	)
	if err == nil {
		t.Fatal("syncUntilConverged unexpectedly accepted an unstable tree")
	}
}
