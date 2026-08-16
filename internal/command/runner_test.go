package command

import (
	"bytes"
	"context"
	"log/slog"
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
