package mount

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
)

const (
	transactionRecordVersion = 1
	sessionClaimGrace        = 5 * time.Second
)

type transactionRecord struct {
	Version            int       `json:"version"`
	Path               string    `json:"path"`
	Staging            string    `json:"staging"`
	DestinationExisted bool      `json:"destination_existed"`
	CreatedAt          time.Time `json:"created_at"`
}

type sessionRecord struct {
	Version    int       `json:"version"`
	PID        int       `json:"pid"`
	Hostname   string    `json:"hostname"`
	Token      string    `json:"token"`
	Mountpoint string    `json:"mountpoint"`
	StartedAt  time.Time `json:"started_at"`
}

type recoverySession struct {
	lockPath string
	token    string
}

type recoveryRun struct {
	root        string
	batch       string
	token       string
	logger      *slog.Logger
	quarantined int
}

func transactionRecordPath(root, stagingPath string) string {
	return filepath.Join(root, config.Directory, "transactions", filepath.Base(stagingPath)+".json")
}

func persistTransactionRecord(root string, record transactionRecord) (string, error) {
	directory := filepath.Join(root, config.Directory, "transactions")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create transaction directory: %w", err)
	}
	record.Version = transactionRecordVersion
	record.CreatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	destination := transactionRecordPath(root, record.Staging)
	temporary, err := os.CreateTemp(directory, ".record-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return "", err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		_ = os.Remove(temporaryPath)
		return "", err
	}
	if err := syncDirectory(directory); err != nil {
		return "", err
	}
	return destination, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	err = directory.Sync()
	if runtime.GOOS == "darwin" && errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}

func recoverStartup(ctx context.Context, repo *repository.Repository, mountpoint string, logger *slog.Logger) (*recoverySession, error) {
	run := &recoveryRun{root: repo.Config.Repository, token: randomToken(), logger: logger.With("component", "recovery")}
	session, staleLock, err := run.acquireSession(mountpoint)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*recoverySession, error) {
		if closeErr := session.Close(); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("release recovery session: %w", closeErr))
		}
		return nil, err
	}
	if staleLock != "" {
		if _, err := run.quarantine(staleLock, filepath.Join("session", filepath.Base(staleLock))); err != nil {
			return fail(err)
		}
		run.logger.Warn("abrupt prior mount detected", "recovery_directory", run.batch)
	}
	if err := run.recoverWrites(); err != nil {
		return fail(err)
	}
	if err := run.quarantineGitLocks(); err != nil {
		return fail(err)
	}
	if err := run.quarantineAnnexTemps(); err != nil {
		return fail(err)
	}
	operation, err := run.snapshotIncompleteGitOperation()
	if err != nil {
		return fail(err)
	}
	if operation != "" {
		return fail(fmt.Errorf("incomplete Git operation %s was preserved in %s; resolve or abort it before mounting", operation, run.batch))
	}
	if _, err := repo.CommitPending(ctx, "Recover interrupted DFS update"); err != nil {
		return fail(fmt.Errorf("finish pending repository update: %w", err))
	}
	if err := repo.CheckConsistency(ctx); err != nil {
		return fail(fmt.Errorf("repository consistency check failed: %w", err))
	}
	if run.quarantined > 0 {
		run.logger.Warn("startup recovery completed", "quarantined", run.quarantined, "directory", run.batch)
	} else {
		run.logger.Info("startup recovery completed", "quarantined", 0)
	}
	return session, nil
}

func (run *recoveryRun) acquireSession(mountpoint string) (*recoverySession, string, error) {
	directory := filepath.Join(run.root, config.Directory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, "", err
	}
	lockPath := filepath.Join(directory, "mount.lock")
	hostname, err := os.Hostname()
	if err != nil {
		return nil, "", fmt.Errorf("determine hostname for mount session: %w", err)
	}
	if hostname == "" {
		return nil, "", errors.New("determine hostname for mount session: hostname is empty")
	}
	record := sessionRecord{Version: 1, PID: os.Getpid(), Hostname: hostname, Token: run.token, Mountpoint: mountpoint, StartedAt: time.Now().UTC()}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return nil, "", err
	}
	data = append(data, '\n')
	var stalePath string
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, err := file.Write(data); err != nil {
				_ = file.Close()
				_ = os.Remove(lockPath)
				return nil, "", err
			}
			if err := file.Sync(); err != nil {
				_ = file.Close()
				_ = os.Remove(lockPath)
				return nil, "", err
			}
			if err := file.Close(); err != nil {
				_ = os.Remove(lockPath)
				return nil, "", err
			}
			if err := syncDirectory(directory); err != nil {
				_ = os.Remove(lockPath)
				return nil, "", err
			}
			return &recoverySession{lockPath: lockPath, token: run.token}, stalePath, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
		existing, readErr := readSessionRecord(lockPath)
		if readErr == nil && sessionMayBeActive(existing, hostname) {
			return nil, "", fmt.Errorf("repository is already mounted by pid %d on %s at %s", existing.PID, existing.Hostname, existing.Mountpoint)
		}
		if readErr != nil {
			if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) < sessionClaimGrace {
				return nil, "", errors.New("repository mount session is being acquired; retry shortly")
			}
		}
		candidate := lockPath + ".stale-" + run.token
		if err := os.Rename(lockPath, candidate); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, "", fmt.Errorf("claim stale mount session: %w", err)
		}
		stalePath = candidate
	}
	return nil, "", errors.New("could not acquire repository mount session")
}

func readSessionRecord(path string) (sessionRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sessionRecord{}, err
	}
	var record sessionRecord
	err = json.Unmarshal(data, &record)
	return record, err
}

func sessionMayBeActive(record sessionRecord, hostname string) bool {
	if record.Hostname != "" && record.Hostname != hostname {
		return true
	}
	if record.PID <= 0 || record.Hostname == "" {
		return false
	}
	process, err := os.FindProcess(record.PID)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (session *recoverySession) Close() error {
	data, err := os.ReadFile(session.lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var record sessionRecord
	if json.Unmarshal(data, &record) != nil || record.Token != session.token {
		return errors.New("mount session lock ownership changed; refusing to remove it")
	}
	if err := os.Remove(session.lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(session.lockPath))
}

func (run *recoveryRun) recoveryDirectory() (string, error) {
	if run.batch != "" {
		return run.batch, nil
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + run.token
	run.batch = filepath.Join(run.root, config.Directory, "recovery", name)
	if err := os.MkdirAll(run.batch, 0o700); err != nil {
		return "", err
	}
	return run.batch, nil
}

func (run *recoveryRun) quarantine(source, relative string) (string, error) {
	batch, err := run.recoveryDirectory()
	if err != nil {
		return "", err
	}
	destination := filepath.Join(batch, relative)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(source, destination); err != nil {
		return "", fmt.Errorf("quarantine %s: %w", source, err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return "", fmt.Errorf("persist quarantine destination %s: %w", destination, err)
	}
	if err := syncDirectory(filepath.Dir(source)); err != nil {
		return "", fmt.Errorf("persist quarantine removal %s: %w", source, err)
	}
	run.quarantined++
	return destination, nil
}

func (run *recoveryRun) recoverWrites() error {
	recordDirectory := filepath.Join(run.root, config.Directory, "transactions")
	records, err := os.ReadDir(recordDirectory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	handled := map[string]bool{}
	for _, entry := range records {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		recordPath := filepath.Join(recordDirectory, entry.Name())
		data, readErr := os.ReadFile(recordPath)
		var record transactionRecord
		decodeErr := json.Unmarshal(data, &record)
		if readErr != nil || decodeErr != nil || record.Version != transactionRecordVersion || !validRecoveryPath(record.Path) || filepath.Base(record.Staging) != record.Staging {
			if _, err := run.quarantine(recordPath, filepath.Join("writes", "invalid-"+entry.Name())); err != nil {
				return err
			}
			continue
		}
		handled[record.Staging] = true
		stagingPath := filepath.Join(run.root, config.Directory, "staging", record.Staging)
		if _, statErr := os.Lstat(stagingPath); errors.Is(statErr, fs.ErrNotExist) {
			if err := os.Remove(recordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := syncDirectory(recordDirectory); err != nil {
				return err
			}
			continue
		} else if statErr != nil {
			return statErr
		}
		// Preserve the destination mapping before moving the only partial payload.
		// If recovery itself is interrupted, the copied manifest still identifies
		// data already moved into this batch.
		if err := run.snapshot(recordPath, filepath.Join("writes", entry.Name())); err != nil {
			return err
		}
		if _, err := run.quarantine(stagingPath, filepath.Join("writes", record.Staging)); err != nil {
			return err
		}
		if err := os.Remove(recordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := syncDirectory(recordDirectory); err != nil {
			return err
		}
		if !record.DestinationExisted {
			destination := filepath.Join(run.root, filepath.FromSlash(record.Path))
			if info, err := os.Lstat(destination); err == nil && info.Mode().IsRegular() && info.Size() == 0 {
				if err := os.Remove(destination); err != nil {
					return err
				}
				if err := syncDirectory(filepath.Dir(destination)); err != nil {
					return err
				}
			}
		}
	}
	stagingDirectory := filepath.Join(run.root, config.Directory, "staging")
	staged, err := os.ReadDir(stagingDirectory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, entry := range staged {
		if handled[entry.Name()] {
			continue
		}
		if _, err := run.quarantine(filepath.Join(stagingDirectory, entry.Name()), filepath.Join("writes", "legacy-"+entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func validRecoveryPath(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	return cleaned == path && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func (run *recoveryRun) quarantineGitLocks() error {
	gitDirectory := filepath.Join(run.root, ".git")
	locks := []string{
		filepath.Join(gitDirectory, "index.lock"),
		filepath.Join(gitDirectory, "HEAD.lock"),
		filepath.Join(gitDirectory, "packed-refs.lock"),
		filepath.Join(gitDirectory, "config.lock"),
		filepath.Join(gitDirectory, "shallow.lock"),
		filepath.Join(gitDirectory, "annex", "index.lock"),
	}
	for _, tree := range []string{filepath.Join(gitDirectory, "refs"), filepath.Join(gitDirectory, "logs")} {
		if err := filepath.WalkDir(tree, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".lock") {
				locks = append(locks, path)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	for _, lock := range locks {
		if _, err := os.Lstat(lock); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		relative, err := filepath.Rel(gitDirectory, lock)
		if err != nil {
			return err
		}
		if _, err := run.quarantine(lock, filepath.Join("locks", relative)); err != nil {
			return err
		}
	}
	return nil
}

func (run *recoveryRun) quarantineAnnexTemps() error {
	for _, name := range []string{"tmp", "othertmp", "transfers"} {
		directory := filepath.Join(run.root, ".git", "annex", name)
		entries, err := os.ReadDir(directory)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, entry := range entries {
			if _, err := run.quarantine(filepath.Join(directory, entry.Name()), filepath.Join("annex", name, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (run *recoveryRun) snapshotIncompleteGitOperation() (string, error) {
	states := []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG", "rebase-apply", "rebase-merge", "sequencer"}
	var found []string
	for _, name := range states {
		source := filepath.Join(run.root, ".git", name)
		if _, err := os.Lstat(source); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return "", err
		}
		if err := run.snapshot(source, filepath.Join("git-state", name)); err != nil {
			return "", err
		}
		found = append(found, name)
	}
	return strings.Join(found, ","), nil
}

func (run *recoveryRun) snapshot(source, relative string) error {
	batch, err := run.recoveryDirectory()
	if err != nil {
		return err
	}
	destination := filepath.Join(batch, relative)
	if err := copyPath(source, destination); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return err
	}
	run.quarantined++
	return nil
}

func copyPath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(destination, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
		}
		return syncDirectory(destination)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to copy special recovery file %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func randomToken() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}
