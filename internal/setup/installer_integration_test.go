package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallersCreateIndependentFilesystemInstances(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installer scripts require a Unix shell")
	}
	home := t.TempDir()
	fakeBin := filepath.Join(home, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "systemctl"), "#!/bin/sh\ncase \"$*\" in *is-active*) exit 1;; esac\nexit 0\n")
	writeExecutable(t, filepath.Join(fakeBin, "launchctl"), "#!/bin/sh\n[ \"${1:-}\" = print ] && exit 1\nexit 0\n")
	for _, name := range []string{"codesign", "plutil", "git-annex", "fusermount3"} {
		writeExecutable(t, filepath.Join(fakeBin, name), "#!/bin/sh\nexit 0\n")
	}
	sourceBinary := filepath.Join(home, "source-dfs")
	writeExecutable(t, sourceBinary, "#!/bin/sh\ncase \"$0\" in *DFS.app/Contents/MacOS/dfs|*/.local/bin/dfs) exit 0;; *) exit 1;; esac\n")

	firstRepo, firstID := testRepository(t, home, "first")
	secondRepo, secondID := testRepository(t, home, "second")
	environment := append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for index, item := range []struct {
		repository string
		id         string
	}{
		{firstRepo, firstID}, {secondRepo, secondID},
	} {
		mountpoint := filepath.Join(home, fmt.Sprintf("linux-mount-%d", index))
		runInstaller(t, environment, "../../scripts/install-cachyos.sh", "--pair-port", fmt.Sprint(7901+index), item.repository, mountpoint, sourceBinary)
		unit := filepath.Join(home, ".config", "systemd", "user", "dfs-mount-"+item.id[:12]+".service")
		assertContains(t, unit, "--pair-port "+fmt.Sprint(7901+index))
	}
	firstUnit := filepath.Join(home, ".config", "systemd", "user", "dfs-mount-"+firstID[:12]+".service")
	secondUnit := filepath.Join(home, ".config", "systemd", "user", "dfs-mount-"+secondID[:12]+".service")
	if _, err := os.Stat(firstUnit); err != nil {
		t.Fatalf("first Linux instance was replaced: %v", err)
	}
	if _, err := os.Stat(secondUnit); err != nil {
		t.Fatalf("second Linux instance missing: %v", err)
	}

	for index, item := range []struct {
		repository string
		id         string
	}{
		{firstRepo, firstID}, {secondRepo, secondID},
	} {
		mountpoint := filepath.Join(home, fmt.Sprintf("mac-mount-%d", index))
		runInstaller(t, environment, "../../scripts/install-macos.sh", "--pair-port", fmt.Sprint(7911+index), item.repository, mountpoint, sourceBinary)
		plist := filepath.Join(home, "Library", "LaunchAgents", "io.bitbeamer.dfs.mount."+item.id[:12]+".plist")
		assertContains(t, plist, "<string>"+fmt.Sprint(7911+index)+"</string>")
	}
	firstPlist := filepath.Join(home, "Library", "LaunchAgents", "io.bitbeamer.dfs.mount."+firstID[:12]+".plist")
	secondPlist := filepath.Join(home, "Library", "LaunchAgents", "io.bitbeamer.dfs.mount."+secondID[:12]+".plist")
	if _, err := os.Stat(firstPlist); err != nil {
		t.Fatalf("first macOS instance was replaced: %v", err)
	}
	if _, err := os.Stat(secondPlist); err != nil {
		t.Fatalf("second macOS instance missing: %v", err)
	}
}

func testRepository(t *testing.T, home, name string) (string, string) {
	t.Helper()
	repository := filepath.Join(home, name)
	commands := [][]string{
		{"init", "-b", "main", repository},
		{"-C", repository, "config", "user.name", "DFS Test"},
		{"-C", repository, "config", "user.email", "dfs@example.invalid"},
		{"-C", repository, "commit", "--allow-empty", "-m", "Initialize"},
	}
	for _, arguments := range commands {
		if output, err := exec.Command("git", arguments...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
		}
	}
	output, err := exec.Command("git", "-C", repository, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repository, strings.TrimSpace(string(output))
}

func runInstaller(t *testing.T, environment []string, arguments ...string) {
	t.Helper()
	command := exec.Command("bash", arguments...)
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("installer %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertContains(t *testing.T, path, wanted string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), wanted) {
		t.Fatalf("%s does not contain %q\n%s", path, wanted, contents)
	}
}
