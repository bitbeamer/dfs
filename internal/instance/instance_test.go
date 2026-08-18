package instance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bitbeamer/dfs/internal/setup"
)

func TestDiscoverSystemdInstancesUsesInstalledServiceDefinitions(t *testing.T) {
	home := t.TempDir()
	serviceID := "123456789abc"
	repository := filepath.Join(home, "Home Files", "repository")
	mountpoint := filepath.Join(home, "Mounted Files")
	if err := os.MkdirAll(filepath.Join(repository, ".git", "dfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"filesystem_id":"123456789abcdef0123456789abcdef012345678","name":"ares","network_name":"Home Files"}`
	if err := os.WriteFile(filepath.Join(repository, ".git", "dfs", "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	health := `{"pid":4242,"repository":` + fmt.Sprintf("%q", repository) + `}`
	if err := os.WriteFile(filepath.Join(repository, ".git", "dfs", "health.json"), []byte(health), 0o600); err != nil {
		t.Fatal(err)
	}
	unitDirectory := filepath.Join(home, "units")
	if err := os.MkdirAll(unitDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := "[Service]\nExecStart=\"/home/otto/.local/bin/dfs\" --repo \"" + repository + "\" internal core --managed --pair-port 7849 --mountpoint \"" + mountpoint + "\"\n"
	if err := os.WriteFile(filepath.Join(unitDirectory, "dfs-core-"+serviceID+".service"), []byte(unit), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &manager{platform: "linux", systemdDir: unitDirectory, run: func(_ context.Context, _ string, arguments ...string) ([]byte, error) {
		if strings.Contains(strings.Join(arguments, " "), "dfs-core-") {
			return nil, nil
		}
		return nil, errors.New("inactive")
	}}
	instances, err := manager.discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 {
		t.Fatalf("instances = %#v", instances)
	}
	instance := instances[0]
	if instance.FileSystemID != "123456789abcdef0123456789abcdef012345678" || instance.NetworkName != "Home Files" || instance.Name != "ares" || instance.Repository != repository || instance.Mountpoint != mountpoint || instance.PairingPort != 7849 || instance.processID != 4242 || !instance.CoreActive || instance.MountActive {
		t.Fatalf("discovered instance = %#v", instance)
	}
}

func TestSystemdArgumentParserHandlesGeneratedEscaping(t *testing.T) {
	arguments, err := systemdExecArguments("[Service]\nExecStart=\"/path/dfs\" --repo \"/data/100%% files/repo\" internal core --mountpoint \"/mnt/quoted\\\"name\" --pair-port 7843\n")
	if err != nil {
		t.Fatal(err)
	}
	wanted := []string{"/path/dfs", "--repo", "/data/100% files/repo", "internal", "core", "--mountpoint", "/mnt/quoted\"name", "--pair-port", "7843"}
	if !reflect.DeepEqual(arguments, wanted) {
		t.Fatalf("arguments = %#v, want %#v", arguments, wanted)
	}
}

func TestPlistProgramArguments(t *testing.T) {
	data := []byte(`<?xml version="1.0"?><plist><dict><key>Label</key><string>ignored</string><key>ProgramArguments</key><array><string>/Applications/DFS</string><string>--repo</string><string>/Users/otto/DFS &amp; Files</string></array><key>Other</key><array><string>ignored</string></array></dict></plist>`)
	arguments, err := plistProgramArguments(data)
	if err != nil {
		t.Fatal(err)
	}
	wanted := []string{"/Applications/DFS", "--repo", "/Users/otto/DFS & Files"}
	if !reflect.DeepEqual(arguments, wanted) {
		t.Fatalf("arguments = %#v, want %#v", arguments, wanted)
	}
}

func TestRepairPreservesStoppedEnabledInstanceState(t *testing.T) {
	home := t.TempDir()
	binary := filepath.Join(home, "dfs")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	manager := &manager{platform: "linux", run: func(_ context.Context, name string, arguments ...string) ([]byte, error) {
		calls = append(calls, strings.Join(append([]string{name}, arguments...), " "))
		return nil, nil
	}}
	instance := Instance{Repository: "/repo", Mountpoint: "/mount", PairingPort: 7850, CoreEnabled: true, MountEnabled: true, serviceID: "123456789abc"}
	if err := manager.update(context.Background(), []Instance{instance}, binary, filepath.Join(home, "installer")); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || !strings.Contains(calls[0], "--pair-port 7850 --no-start --no-enable /repo /mount "+binary) ||
		!strings.Contains(calls[1], "systemctl --user enable dfs-core-123456789abc.service") ||
		!strings.Contains(calls[2], "systemctl --user enable dfs-mount-123456789abc.service") {
		t.Fatalf("update calls = %#v", calls)
	}
	for _, call := range calls {
		if strings.Contains(call, " start ") {
			t.Fatalf("repair started a stopped service: %s", call)
		}
	}
}

func TestParseLaunchdDisabledState(t *testing.T) {
	disabled := parseLaunchdDisabled(`disabled services = {
		"io.bitbeamer.dfs.core.123456789abc" => true;
		"io.bitbeamer.dfs.mount.123456789abc" => false;
	}`)
	if !disabled["io.bitbeamer.dfs.core.123456789abc"] || disabled["io.bitbeamer.dfs.mount.123456789abc"] {
		t.Fatalf("disabled launch agents = %#v", disabled)
	}
}

func TestUpgradeRollsBackExecutableWhenRepairFails(t *testing.T) {
	home := t.TempDir()
	installed := filepath.Join(home, "dfs")
	candidate := filepath.Join(home, "candidate")
	if err := os.WriteFile(installed, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("new executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	installerCalls := 0
	manager := &manager{platform: "linux", run: func(_ context.Context, name string, arguments ...string) ([]byte, error) {
		if name == candidate {
			return []byte("dfs dev"), nil
		}
		if name == filepath.Join(home, "installer") {
			installerCalls++
			if installerCalls == 1 {
				return nil, errors.New("new service failed health verification")
			}
		}
		return nil, nil
	}}
	instance := Instance{Binary: installed, Repository: "/repo", Mountpoint: "/mount", PairingPort: 7843, serviceID: "123456789abc"}
	if err := manager.upgrade(context.Background(), []Instance{instance}, candidate, filepath.Join(home, "installer")); err == nil {
		t.Fatal("upgrade unexpectedly succeeded")
	}
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "old executable" {
		t.Fatalf("installed executable after rollback = %q", data)
	}
	if installerCalls != 2 {
		t.Fatalf("installer calls = %d, want failed upgrade plus rollback repair", installerCalls)
	}
}

func TestFindRequiresUnambiguousInstance(t *testing.T) {
	instances := []Instance{{FileSystemID: "aaaaaaaaaaaa1111", NetworkName: "Home"}, {FileSystemID: "bbbbbbbbbbbb2222", NetworkName: "Home"}}
	if _, err := Find(instances, "Home"); err == nil {
		t.Fatal("ambiguous display name unexpectedly selected an instance")
	}
	selected, err := Find(instances, "aaaaaaaaaaaa")
	if err != nil || selected.FileSystemID != instances[0].FileSystemID {
		t.Fatalf("selected instance = %#v, %v", selected, err)
	}
}

func TestUninstallRemovesServicesButRetainsRepository(t *testing.T) {
	home := t.TempDir()
	repository := filepath.Join(home, "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	serviceID := "123456789abc"
	unitDirectory := filepath.Join(home, "units")
	if err := os.MkdirAll(unitDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"mount", "core"} {
		if err := os.WriteFile(filepath.Join(unitDirectory, "dfs-"+kind+"-"+serviceID+".service"), []byte("unit"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	manager := &manager{platform: "linux", systemdDir: unitDirectory, run: func(_ context.Context, name string, arguments ...string) ([]byte, error) {
		calls = append(calls, strings.Join(append([]string{name}, arguments...), " "))
		return nil, nil
	}}
	if err := manager.uninstall(context.Background(), []Instance{{Repository: repository, serviceID: serviceID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(repository); err != nil {
		t.Fatalf("repository was removed: %v", err)
	}
	for _, kind := range []string{"mount", "core"} {
		if _, err := os.Stat(filepath.Join(unitDirectory, "dfs-"+kind+"-"+serviceID+".service")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s unit remains: %v", kind, err)
		}
	}
	if len(calls) != 2 || !strings.Contains(calls[0], "disable --now") || !strings.Contains(calls[1], "daemon-reload") {
		t.Fatalf("uninstall calls = %#v", calls)
	}
}

func TestUninstallAndPurgeRemovesFrozenAnnexRepository(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repository := filepath.Join(home, ".local", "share", "dfs", "repository")
	objectDirectory := filepath.Join(repository, ".git", "annex", "objects", "AA", "BB", "SHA256E-s4--test.txt")
	if err := os.MkdirAll(objectDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	object := filepath.Join(objectDirectory, "SHA256E-s4--test.txt")
	if err := os.WriteFile(object, []byte("test"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(objectDirectory, 0o555); err != nil {
		t.Fatal(err)
	}
	serviceID := "123456789abc"
	unitDirectory := filepath.Join(home, "units")
	if err := os.MkdirAll(unitDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"mount", "core"} {
		if err := os.WriteFile(filepath.Join(unitDirectory, "dfs-"+kind+"-"+serviceID+".service"), []byte("unit"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &manager{platform: "linux", systemdDir: unitDirectory, run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}}
	setupState, err := setup.StatePath(repository)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(setupState), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setupState, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.uninstallAndPurge(context.Background(), []Instance{{Repository: repository, serviceID: serviceID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(repository); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged repository remains: %v", err)
	}
	if _, err := os.Stat(setupState); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged setup state remains: %v", err)
	}
}

func TestUninstallWaitsForCoreProcessBeforeRemovingDefinitions(t *testing.T) {
	home := t.TempDir()
	unitDirectory := filepath.Join(home, "units")
	if err := os.MkdirAll(unitDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	serviceID := "123456789abc"
	for _, kind := range []string{"mount", "core"} {
		if err := os.WriteFile(filepath.Join(unitDirectory, "io.bitbeamer.dfs."+kind+"."+serviceID+".plist"), []byte("plist"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	checks := 0
	manager := &manager{platform: "darwin", launchdDir: unitDirectory, domain: "gui/501", run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, nil
	}, alive: func(pid int) bool {
		checks++
		return pid == 1234 && checks == 1
	}}
	err := manager.uninstall(context.Background(), []Instance{{Repository: filepath.Join(home, "repository"), serviceID: serviceID, CoreActive: true, processID: 1234}})
	if err != nil {
		t.Fatal(err)
	}
	if checks < 2 {
		t.Fatalf("core process checked %d time(s), want shutdown wait", checks)
	}
}

func TestUninstallAndPurgeRejectsUnsafeTargetBeforeChangingServices(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	calls := 0
	manager := &manager{platform: "linux", systemdDir: filepath.Join(home, "units"), run: func(context.Context, string, ...string) ([]byte, error) {
		calls++
		return nil, nil
	}}
	err := manager.uninstallAndPurge(context.Background(), []Instance{{Repository: home, serviceID: "123456789abc"}})
	if err == nil || !strings.Contains(err.Error(), "unsafe repository path") {
		t.Fatalf("unsafe purge error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("service commands ran before purge validation: %d", calls)
	}
}

func TestPurgeTargetsRejectAmbiguousPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	nonRepository := filepath.Join(home, "not-a-repository")
	if err := os.MkdirAll(nonRepository, 0o755); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(home, "repository")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(home, "repository-link")
	if err := os.Symlink(repository, symlink); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"relative":       "repository",
		"non-repository": nonRepository,
		"symlink":        symlink,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := purgeTargets([]Instance{{Repository: target}}); err == nil {
				t.Fatalf("purge target %q was accepted", target)
			}
		})
	}
}

func TestUninstallAndPurgeRetainsRepositoryWhenServiceStopFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repository := filepath.Join(home, "repository")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &manager{platform: "linux", systemdDir: filepath.Join(home, "units"), run: func(_ context.Context, _ string, arguments ...string) ([]byte, error) {
		if len(arguments) > 1 && arguments[1] == "disable" {
			return nil, errors.New("stop failed")
		}
		return nil, nil
	}}
	err := manager.uninstallAndPurge(context.Background(), []Instance{{Repository: repository, serviceID: "123456789abc"}})
	if err == nil || !strings.Contains(err.Error(), "stop managed services") {
		t.Fatalf("failed uninstall error = %v", err)
	}
	if _, err := os.Stat(repository); err != nil {
		t.Fatalf("repository was purged after service stop failure: %v", err)
	}
}
