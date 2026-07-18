package command

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
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
