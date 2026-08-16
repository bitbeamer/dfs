package command

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerLogsCommandLifecycleAtDebugLevel(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	runner := Runner{Directory: t.TempDir(), Logger: logger}

	if _, err := runner.Run(context.Background(), "git", "--version"); err != nil {
		t.Fatal(err)
	}

	logOutput := output.String()
	for _, expected := range []string{"msg=\"command started\"", "msg=\"command completed\"", "command=\"git --version\"", "duration="} {
		if !strings.Contains(logOutput, expected) {
			t.Fatalf("debug log does not contain %q: %s", expected, logOutput)
		}
	}
}

func TestRunnerCancellationDoesNotWaitForChildProcessPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (Runner{Directory: t.TempDir()}).Run(ctx, "sh", "-c", "sleep 30 & wait")
	if err == nil {
		t.Fatal("cancelled command unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("cancelled command waited for descendant pipes for %s", elapsed)
	}
}

func TestRunnerCancellationAllowsTransactionalCleanup(t *testing.T) {
	directory := t.TempDir()
	lock := filepath.Join(directory, "index.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := (Runner{Directory: directory}).Run(ctx, "sh", "-c",
		"trap 'rm -f index.lock; exit 143' TERM; touch index.lock; while :; do sleep 1; done")
	if err == nil {
		t.Fatal("cancelled command unexpectedly succeeded")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("transaction lock remains after cancellation: %v", err)
	}
}
