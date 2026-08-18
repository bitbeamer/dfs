package instance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	if instance.FileSystemID != "123456789abcdef0123456789abcdef012345678" || instance.NetworkName != "Home Files" || instance.Name != "ares" || instance.Repository != repository || instance.Mountpoint != mountpoint || instance.PairingPort != 7849 || !instance.CoreActive || instance.MountActive {
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
