package mount

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
)

func newRecoveryRun(t *testing.T, root string) *recoveryRun {
	t.Helper()
	return &recoveryRun{
		root: root, token: "test-token",
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func writeRecoveryTransaction(t *testing.T, root, path, staging string, existed bool) string {
	t.Helper()
	recordPath, err := persistTransactionRecord(root, transactionRecord{
		Path: path, Staging: staging, DestinationExisted: existed,
	})
	if err != nil {
		t.Fatal(err)
	}
	return recordPath
}

func TestRecoveryQuarantinesInterruptedWriteAndPreservesDestination(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, config.Directory, "staging"), 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "document.txt")
	if err := os.WriteFile(destination, []byte("last valid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagingName := "write-interrupted"
	staging := filepath.Join(root, config.Directory, "staging", stagingName)
	if err := os.WriteFile(staging, []byte("partial edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRecoveryTransaction(t, root, "document.txt", stagingName, true)
	run := newRecoveryRun(t, root)

	if err := run.recoverWrites(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(destination)
	if err != nil || string(content) != "last valid\n" {
		t.Fatalf("published destination = %q, %v", content, err)
	}
	quarantined, err := os.ReadFile(filepath.Join(run.batch, "writes", stagingName))
	if err != nil || string(quarantined) != "partial edit" {
		t.Fatalf("quarantined write = %q, %v", quarantined, err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging file still exists: %v", err)
	}
}

func TestRecoveryRemovesOnlyProvenInterruptedCreatePlaceholder(t *testing.T) {
	root := t.TempDir()
	stagingDirectory := filepath.Join(root, config.Directory, "staging")
	if err := os.MkdirAll(stagingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "new.txt")
	if err := os.WriteFile(destination, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stagingName := "write-new"
	if err := os.WriteFile(filepath.Join(stagingDirectory, stagingName), []byte("unfinished"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRecoveryTransaction(t, root, "new.txt", stagingName, false)
	run := newRecoveryRun(t, root)

	if err := run.recoverWrites(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("interrupted create placeholder still exists: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(run.batch, "writes", stagingName)); err != nil || string(content) != "unfinished" {
		t.Fatalf("quarantined create = %q, %v", content, err)
	}
}

func TestRecoveryKeepsCompletedAtomicPublish(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "published.txt")
	if err := os.WriteFile(destination, []byte("published\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordPath := writeRecoveryTransaction(t, root, "published.txt", "write-published", true)
	run := newRecoveryRun(t, root)

	if err := run.recoverWrites(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(destination); err != nil || string(content) != "published\n" {
		t.Fatalf("published destination = %q, %v", content, err)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("completed transaction record still exists: %v", err)
	}
	if run.batch != "" {
		t.Fatalf("completed publish unexpectedly quarantined into %s", run.batch)
	}
}

func TestRecoveryQuarantinesLegacyStagingFile(t *testing.T) {
	root := t.TempDir()
	stagingDirectory := filepath.Join(root, config.Directory, "staging")
	if err := os.MkdirAll(stagingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingDirectory, "write-legacy"), []byte("unknown"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newRecoveryRun(t, root)
	if err := run.recoverWrites(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(run.batch, "writes", "legacy-write-legacy")); err != nil || string(content) != "unknown" {
		t.Fatalf("legacy quarantine = %q, %v", content, err)
	}
}

func TestMountSessionRejectsLiveOwnerAndReclaimsDeadOwner(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, config.Directory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	lockPath := filepath.Join(directory, "mount.lock")
	writeSession := func(pid int, token string) {
		t.Helper()
		data, err := json.Marshal(sessionRecord{Version: 1, PID: pid, Hostname: hostname, Token: token, Mountpoint: "/mnt"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lockPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	writeSession(os.Getpid(), "live")
	run := newRecoveryRun(t, root)
	if _, _, err := run.acquireSession("/new", false); err == nil || !strings.Contains(err.Error(), "already mounted") {
		t.Fatalf("live session error = %v", err)
	}
	if _, _, err := run.acquireSession("/new", true); err == nil || !strings.Contains(err.Error(), "already mounted") {
		t.Fatalf("explicit recovery overrode live local session: %v", err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	writeSession(1<<30, "dead")
	session, stale, err := run.acquireSession("/new", false)
	if err != nil {
		t.Fatal(err)
	}
	if stale == "" {
		t.Fatal("dead session was not reclaimed")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMountSessionRequiresExplicitRecoveryForAnotherHost(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, config.Directory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(directory, "mount.lock")
	record := sessionRecord{Version: 1, PID: 1234, Hostname: "another-host", Token: "stale", Mountpoint: "/old"}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	run := newRecoveryRun(t, root)
	if _, _, err := run.acquireSession("/new", false); err == nil || !strings.Contains(err.Error(), "--recover-stale-session") {
		t.Fatalf("cross-host session error = %v", err)
	}
	session, stale, err := run.acquireSession("/new", true)
	if err != nil {
		t.Fatal(err)
	}
	if stale == "" {
		t.Fatal("explicit recovery did not reclaim cross-host session")
	}
	if content, err := os.ReadFile(stale); err != nil || string(content) != string(data) {
		t.Fatalf("recovered session record = %q, %v", content, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMountSessionDoesNotStealFreshPartialClaim(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, config.Directory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(directory, "mount.lock")
	if err := os.WriteFile(lockPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newRecoveryRun(t, root)
	if _, _, err := run.acquireSession("/new", false); err == nil || !strings.Contains(err.Error(), "retry shortly") {
		t.Fatalf("partial session claim error = %v", err)
	}
	if content, err := os.ReadFile(lockPath); err != nil || string(content) != "{" {
		t.Fatalf("partial session claim was replaced: %q, %v", content, err)
	}
}

func TestRecoverySnapshotsIncompleteGitOperationWithoutRemovingIt(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, ".git", "rebase-merge", "head-name")
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newRecoveryRun(t, root)
	operation, err := run.snapshotIncompleteGitOperation()
	if err != nil {
		t.Fatal(err)
	}
	if operation != "rebase-merge" {
		t.Fatalf("operation = %q", operation)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("original Git recovery state was removed: %v", err)
	}
	copy := filepath.Join(run.batch, "git-state", "rebase-merge", "head-name")
	if content, err := os.ReadFile(copy); err != nil || string(content) != "refs/heads/main\n" {
		t.Fatalf("Git state snapshot = %q, %v", content, err)
	}
}

func TestSessionRecordClosePreservesReplacementOwner(t *testing.T) {
	root := t.TempDir()
	run := newRecoveryRun(t, root)
	session, _, err := run.acquireSession("/mnt", false)
	if err != nil {
		t.Fatal(err)
	}
	replacement := sessionRecord{Version: 1, PID: os.Getpid(), Hostname: "replacement", Token: "other"}
	data, _ := json.Marshal(replacement)
	if err := os.WriteFile(session.lockPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err == nil {
		t.Fatal("session close removed a replacement owner's lock")
	}
	if _, err := os.Stat(session.lockPath); err != nil {
		t.Fatal(err)
	}
}

func TestValidRecoveryPath(t *testing.T) {
	tests := map[string]bool{"file.txt": true, "dir/file.txt": true, "../escape": false, "/absolute": false, "": false}
	for path, want := range tests {
		if got := validRecoveryPath(path); got != want {
			t.Errorf("validRecoveryPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestTransactionRecordTimestampIsDurable(t *testing.T) {
	root := t.TempDir()
	path := writeRecoveryTransaction(t, root, "file", "write-time", true)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record transactionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.CreatedAt.IsZero() || time.Since(record.CreatedAt) > time.Minute {
		t.Fatalf("transaction timestamp = %v", record.CreatedAt)
	}
}

func TestRecoverStartupPreservesLastPublishedVersion(t *testing.T) {
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
			if err == nil {
				if info.IsDir() {
					_ = os.Chmod(path, 0o700)
				} else {
					_ = os.Chmod(path, 0o600)
				}
			}
			return nil
		})
	})
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname=Recovery Test\nemail=recovery@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo, err := repository.Init(ctx, filepath.Join(home, "repo"), "recovery", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	destination := filepath.Join(repo.Config.Repository, "document.txt")
	if err := os.WriteFile(destination, []byte("last published\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CommitPending(ctx, "Add document"); err != nil {
		t.Fatal(err)
	}
	stagingDirectory := filepath.Join(repo.Config.Repository, config.Directory, "staging")
	if err := os.MkdirAll(stagingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	stagingName := "write-crashed"
	if err := os.WriteFile(filepath.Join(stagingDirectory, stagingName), []byte("torn update"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRecoveryTransaction(t, repo.Config.Repository, "document.txt", stagingName, true)
	if err := os.WriteFile(filepath.Join(repo.Config.Repository, ".git", "index.lock"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	annexTemp := filepath.Join(repo.Config.Repository, ".git", "annex", "tmp", "partial-transfer")
	if err := os.MkdirAll(filepath.Dir(annexTemp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(annexTemp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	hostname, _ := os.Hostname()
	staleSession, _ := json.Marshal(sessionRecord{Version: 1, PID: 1 << 30, Hostname: hostname, Token: "dead", Mountpoint: "/old"})
	if err := os.WriteFile(filepath.Join(repo.Config.Repository, config.Directory, "mount.lock"), staleSession, 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	session, err := recoverStartup(ctx, repo, filepath.Join(home, "mnt"), logger, false)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if content, err := os.ReadFile(destination); err != nil || string(content) != "last published\n" {
		t.Fatalf("last published version = %q, %v", content, err)
	}
	var recovered []string
	recoveryRoot := filepath.Join(repo.Config.Repository, config.Directory, "recovery")
	if err := filepath.Walk(recoveryRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			recovered = append(recovered, filepath.Base(path))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(recovered, " ")
	for _, expected := range []string{stagingName, "index.lock", "partial-transfer"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("recovery files %q do not contain %q", joined, expected)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo.Config.Repository, config.Directory, "mount.lock")); !os.IsNotExist(err) {
		t.Fatalf("clean recovery left mount session record: %v", err)
	}
}
