package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewMountLoggerHonorsLevelAndLogFile(t *testing.T) {
	var stderr bytes.Buffer
	logFile := filepath.Join(t.TempDir(), "mount.log")
	logger, closer, err := newMountLogger("info", "text", logFile, &stderr, false)
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("hidden")
	logger.Info("mounted", "peer", "test")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if permission := info.Mode().Perm(); permission != 0o600 {
		t.Fatalf("log file permission = %o, want 600", permission)
	}

	for name, content := range map[string]string{
		"stderr": stderr.String(),
		"file":   readTestFile(t, logFile),
	} {
		if strings.Contains(content, "hidden") {
			t.Fatalf("%s contains debug entry at info level: %s", name, content)
		}
		if !strings.Contains(content, "msg=mounted") || !strings.Contains(content, "peer=test") {
			t.Fatalf("%s does not contain structured info entry: %s", name, content)
		}
	}
}

func TestNewMountLoggerRejectsInvalidLevel(t *testing.T) {
	if _, _, err := newMountLogger("chatty", "text", "", &bytes.Buffer{}, false); err == nil {
		t.Fatal("invalid log level unexpectedly succeeded")
	}
}

func TestNewMountLoggerRejectsInvalidFormat(t *testing.T) {
	if _, _, err := newMountLogger("info", "binary", "", &bytes.Buffer{}, false); err == nil {
		t.Fatal("invalid log format unexpectedly succeeded")
	}
}

func TestFuseDebugForcesDebugLogging(t *testing.T) {
	var output bytes.Buffer
	logger, _, err := newMountLogger("error", "text", "", &output, true)
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("fuse detail")
	if !strings.Contains(output.String(), "fuse detail") {
		t.Fatalf("fuse debug did not enable debug-level output: %s", output.String())
	}
}

func TestNewMountLoggerSupportsJSON(t *testing.T) {
	var output bytes.Buffer
	logger, _, err := newMountLogger("info", "json", "", &output, false)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("mounted", "peer", "test")
	if content := output.String(); !strings.Contains(content, `"msg":"mounted"`) || !strings.Contains(content, `"peer":"test"`) {
		t.Fatalf("JSON log is not structured: %s", content)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
