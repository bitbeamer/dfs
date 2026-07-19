package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dfsmount "github.com/bitbeamer/dfs/internal/mount"
	"github.com/bitbeamer/dfs/internal/repository"
	"golang.org/x/sys/unix"
)

func TestEssentialFilesystemSemantics(t *testing.T) {
	mountpoint, repo := mountSemanticTestFS(t)

	t.Run("advisory locks", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		path := filepath.Join(directory, "locked.txt")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart}
		if err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock); err != nil {
			t.Fatalf("set lock: %v", err)
		}
		blocked := lockHelperCommand(path, false)
		if output, err := blocked.CombinedOutput(); err == nil {
			t.Fatalf("second process acquired conflicting lock: %s", output)
		}
		waiting := lockHelperCommand(path, true)
		if err := waiting.Start(); err != nil {
			t.Fatal(err)
		}
		waitDone := make(chan error, 1)
		go func() { waitDone <- waiting.Wait() }()
		select {
		case err := <-waitDone:
			t.Fatalf("blocking lock returned before unlock: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
		lock.Type = unix.F_UNLCK
		if err := unix.FcntlFlock(file.Fd(), unix.F_SETLK, &lock); err != nil {
			t.Fatalf("unlock: %v", err)
		}
		select {
		case err := <-waitDone:
			if err != nil {
				t.Fatalf("blocking lock after unlock: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = waiting.Process.Kill()
			t.Fatal("blocking lock did not acquire after unlock")
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("fsync and flush", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		path := filepath.Join(directory, "durable.txt")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("durable data\n"); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(); err != nil {
			t.Fatalf("fsync: %v", err)
		}
		relative, err := filepath.Rel(mountpoint, path)
		if err != nil {
			t.Fatal(err)
		}
		if content, err := os.ReadFile(filepath.Join(repo.Config.Repository, relative)); err != nil || string(content) != "durable data\n" {
			t.Fatalf("published checkpoint after fsync = %q, %v", content, err)
		}
		if content, err := os.ReadFile(path); err != nil || string(content) != "durable data\n" {
			t.Fatalf("content after fsync = %q, %v", content, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("flush/close: %v", err)
		}
	})

	t.Run("atomic rename overwrite", func(t *testing.T) {
		t.Run("destination exists", func(t *testing.T) {
			assertRenameOverwrite(t, semanticDirectory(t, mountpoint), true)
		})
		t.Run("destination absent", func(t *testing.T) {
			assertRenameOverwrite(t, semanticDirectory(t, mountpoint), false)
		})
	})

	t.Run("rename while open", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		oldPath := filepath.Join(directory, "old.txt")
		newPath := filepath.Join(directory, "new.txt")
		file, err := os.OpenFile(oldPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("renamed while open\n"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
			t.Fatalf("old path exists after close: %v", err)
		}
		if content, err := os.ReadFile(newPath); err != nil || string(content) != "renamed while open\n" {
			t.Fatalf("renamed content = %q, %v", content, err)
		}
	})

	t.Run("open then unlink", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		path := filepath.Join(directory, "unlinked.txt")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("before unlink\n"); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt([]byte("after"), 0); err != nil {
			t.Fatalf("write through unlinked descriptor: %v", err)
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(file)
		if err != nil || !strings.HasPrefix(string(content), "after") {
			t.Fatalf("read through unlinked descriptor = %q, %v", content, err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unlinked path recreated on close: %v", err)
		}
	})

	t.Run("permissions timestamps and extended attributes", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		path := filepath.Join(directory, "metadata.txt")
		if err := os.WriteFile(path, []byte("metadata\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, os.Getuid(), os.Getgid()); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000, 123_000_000)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if err := unix.Setxattr(path, "user.dfs-test", []byte("preserved"), 0); err != nil {
			t.Fatalf("setxattr: %v", err)
		}
		waitForAnnexed(t, filepath.Join(repo.Config.Repository, strings.TrimPrefix(path, mountpoint+string(os.PathSeparator))))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o750 {
			t.Fatalf("permissions after annex sync = %o, want 750", got)
		}
		if !info.ModTime().Equal(stamp) {
			t.Fatalf("mtime after annex sync = %v, want %v", info.ModTime(), stamp)
		}
		relative := strings.TrimPrefix(path, mountpoint+string(os.PathSeparator))
		fresh := dfsmount.NewFileSystem(repo, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		attr, code := fresh.GetAttr(relative, nil)
		if !code.Ok() {
			t.Fatalf("fresh filesystem getattr: %v", code)
		}
		if attr.Mode&0o7777 != 0o750 {
			t.Fatalf("persisted permissions = %o, want 750", attr.Mode&0o7777)
		}
		if attr.Uid != uint32(os.Getuid()) || attr.Gid != uint32(os.Getgid()) {
			t.Fatalf("persisted owner = %d:%d, want %d:%d", attr.Uid, attr.Gid, os.Getuid(), os.Getgid())
		}
		if got := time.Unix(int64(attr.Mtime), int64(attr.Mtimensec)); !got.Equal(stamp) {
			t.Fatalf("persisted mtime = %v, want %v", got, stamp)
		}
		if got := time.Unix(int64(attr.Atime), int64(attr.Atimensec)); !got.Equal(stamp) {
			t.Fatalf("persisted atime = %v, want %v", got, stamp)
		}
		buffer := make([]byte, 64)
		size, err := unix.Getxattr(path, "user.dfs-test", buffer)
		if err != nil || string(buffer[:size]) != "preserved" {
			t.Fatalf("xattr after annex sync = %q, %v", buffer[:size], err)
		}
		size, err = unix.Listxattr(path, nil)
		if err != nil {
			t.Fatalf("listxattr size: %v", err)
		}
		list := make([]byte, size)
		if _, err := unix.Listxattr(path, list); err != nil || !bytes.Contains(list, []byte("user.dfs-test")) {
			t.Fatalf("listxattr = %q, %v", list, err)
		}
		if err := unix.Removexattr(path, "user.dfs-test"); err != nil {
			t.Fatalf("removexattr: %v", err)
		}
		if _, err := unix.Getxattr(path, "user.dfs-test", buffer); err == nil {
			t.Fatal("removed xattr remains readable")
		}
	})

	t.Run("case-only rename", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		lower := filepath.Join(directory, "case-name.txt")
		upper := filepath.Join(directory, "Case-Name.txt")
		if err := os.WriteFile(lower, []byte("case\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(lower, upper); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "Case-Name.txt" {
			t.Fatalf("case-only rename entries = %v", entryNames(entries))
		}
	})

	t.Run("unicode normalization", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		decomposed := filepath.Join(directory, "Cafe\u0301.txt")
		composedName := "Caf\u00e9.txt"
		if err := os.WriteFile(decomposed, []byte("unicode\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != composedName {
			t.Fatalf("normalized entries = %q, want %q", entryNames(entries), composedName)
		}
		if content, err := os.ReadFile(filepath.Join(directory, composedName)); err != nil || string(content) != "unicode\n" {
			t.Fatalf("read normalized path = %q, %v", content, err)
		}
	})

	t.Run("external annex replacements are visible on fresh open", func(t *testing.T) {
		directory := semanticDirectory(t, mountpoint)
		mountedPath := filepath.Join(directory, "remote.txt")
		if err := os.WriteFile(mountedPath, []byte("one\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(mountpoint, mountedPath)
		if err != nil {
			t.Fatal(err)
		}
		backingPath := filepath.Join(repo.Config.Repository, relative)
		waitForAnnexed(t, backingPath)
		assertFileContent(t, mountedPath, "one\n")
		versions := []string{"one\n", "one\ntwo\n", "one\ntwo\nthree\n", "one\ntwo\nthree\nfour\n"}
		targets := make([]string, 1, len(versions))
		initialTarget, err := os.Readlink(backingPath)
		if err != nil {
			t.Fatal(err)
		}
		targets[0] = initialTarget
		for _, content := range versions[1:] {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := repo.Unlock(ctx, filepath.ToSlash(relative)); err != nil {
				cancel()
				t.Fatal(err)
			}
			if err := os.WriteFile(backingPath, []byte(content), 0o644); err != nil {
				cancel()
				t.Fatal(err)
			}
			if committed, err := repo.CommitPending(ctx, "Replace annex key under mount"); err != nil || !committed {
				cancel()
				t.Fatalf("commit external replacement = %v, %v", committed, err)
			}
			cancel()
			assertFileContent(t, mountedPath, content)
			target, err := os.Readlink(backingPath)
			if err != nil {
				t.Fatal(err)
			}
			targets = append(targets, target)
		}
		for index, target := range targets {
			swapSymlink(t, backingPath, target)
			assertFileContent(t, mountedPath, versions[index])
		}

		tail, err := exec.LookPath("tail")
		if err != nil {
			t.Skip("tail is not installed")
		}
		swapSymlink(t, backingPath, targets[0])
		stdoutPath := filepath.Join(t.TempDir(), "tail.stdout")
		stderrPath := filepath.Join(filepath.Dir(stdoutPath), "tail.stderr")
		stdout, err := os.Create(stdoutPath)
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := os.Create(stderrPath)
		if err != nil {
			_ = stdout.Close()
			t.Fatal(err)
		}
		tailContext, cancelTail := context.WithCancel(context.Background())
		follow := exec.CommandContext(tailContext, tail, "-F", mountedPath)
		follow.Stdout = stdout
		follow.Stderr = stderr
		if err := follow.Start(); err != nil {
			cancelTail()
			_ = stdout.Close()
			_ = stderr.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cancelTail()
			_ = follow.Wait()
			_ = stdout.Close()
			_ = stderr.Close()
		})
		waitForTextContains(t, stdoutPath, stderrPath, "one\n", 5*time.Second)
		swapSymlink(t, backingPath, targets[len(targets)-1])
		waitForTextContains(t, stdoutPath, stderrPath, "four\n", 12*time.Second)
	})
}

func swapSymlink(t *testing.T, path, target string) {
	t.Helper()
	temporary := path + ".swap"
	_ = os.Remove(temporary)
	if err := os.Symlink(target, temporary); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

func waitForTextContains(t *testing.T, path, diagnosticsPath, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		content, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(content), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	content, err := os.ReadFile(path)
	diagnostics, diagnosticsErr := os.ReadFile(diagnosticsPath)
	t.Fatalf("%s did not contain %q: content=%q error=%v diagnostics=%q diagnostics_error=%v", path, want, content, err, diagnostics, diagnosticsErr)
}

func TestAdvisoryLockHelper(t *testing.T) {
	if os.Getenv("DFS_LOCK_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	file, err := os.OpenFile(os.Getenv("DFS_LOCK_PATH"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	command := unix.F_SETLK
	if os.Getenv("DFS_LOCK_WAIT") == "1" {
		command = unix.F_SETLKW
	}
	lock := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart}
	if err := unix.FcntlFlock(file.Fd(), command, &lock); err != nil {
		t.Fatal(err)
	}
}

func lockHelperCommand(path string, wait bool) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestAdvisoryLockHelper$")
	waitValue := "0"
	if wait {
		waitValue = "1"
	}
	command.Env = append(os.Environ(), "DFS_LOCK_HELPER=1", "DFS_LOCK_PATH="+path, "DFS_LOCK_WAIT="+waitValue)
	return command
}

func assertRenameOverwrite(t *testing.T, directory string, destinationExists bool) {
	t.Helper()
	source := filepath.Join(directory, "source.txt")
	destination := filepath.Join(directory, "destination.txt")
	if err := os.WriteFile(source, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	var oldDestination *os.File
	if destinationExists {
		if err := os.WriteFile(destination, []byte("destination"), 0o644); err != nil {
			t.Fatal(err)
		}
		var err error
		oldDestination, err = os.Open(destination)
		if err != nil {
			t.Fatal(err)
		}
		defer oldDestination.Close()
	}
	if err := os.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source after rename = %v", err)
	}
	if content, err := os.ReadFile(destination); err != nil || string(content) != "source" {
		t.Fatalf("destination after rename = %q, %v", content, err)
	}
	destinationInfo, err := os.Stat(destination)
	if err != nil || !os.SameFile(sourceInfo, destinationInfo) {
		t.Fatalf("rename did not preserve source inode: source=%v destination=%v err=%v", sourceInfo, destinationInfo, err)
	}
	if oldDestination != nil {
		content, err := io.ReadAll(oldDestination)
		if err != nil || string(content) != "destination" {
			t.Fatalf("open overwritten descriptor = %q, %v", content, err)
		}
	}
}

func mountSemanticTestFS(t *testing.T) (string, *repository.Repository) {
	t.Helper()
	if os.Getenv("DFS_INTEGRATION") == "" {
		t.Skip("set DFS_INTEGRATION=1 to run mount tests")
	}
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}
	home := t.TempDir()
	t.Cleanup(func() { makeWritable(home) })
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname=DFS Test\nemail=dfs@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repo, err := repository.Init(ctx, filepath.Join(home, "repo"), "semantics", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	readyName := ".mount-ready"
	if err := os.WriteFile(filepath.Join(repo.Config.Repository, readyName), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountpoint := filepath.Join(home, "mnt")
	mountContext, cancelMount := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		errCh <- dfsmount.Run(repo, mountpoint, dfsmount.Options{Context: mountContext, Logger: logger})
	}()
	waitForPath(t, filepath.Join(mountpoint, readyName), 10*time.Second)
	t.Cleanup(func() {
		cancelMount()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("mount shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("mount did not stop")
		}
		_ = repo.Close()
	})
	return mountpoint, repo
}

func semanticDirectory(t *testing.T, mountpoint string) string {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	directory := filepath.Join(mountpoint, name)
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	return directory
}

func waitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForAnnexed(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for git-annex to lock %s", path)
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil || string(content) != want {
		t.Fatalf("content of %s = %q, %v; want %q", path, content, err, want)
	}
}

func makeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chmod(path, info.Mode()|0o700)
		}
		return nil
	})
}
