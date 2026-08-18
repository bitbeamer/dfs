package instance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	dfssetup "github.com/bitbeamer/dfs/internal/setup"
)

type Instance struct {
	FileSystemID string `json:"filesystem_id"`
	Name         string `json:"name"`
	NetworkName  string `json:"network_name"`
	Binary       string `json:"binary"`
	Repository   string `json:"repository"`
	Mountpoint   string `json:"mountpoint"`
	PairingPort  int    `json:"pairing_port"`
	CoreActive   bool   `json:"core_active"`
	MountActive  bool   `json:"mount_active"`
	CoreEnabled  bool   `json:"core_enabled"`
	MountEnabled bool   `json:"mount_enabled"`
	Platform     string `json:"platform"`
	serviceID    string
}

func (i Instance) Active() bool { return i.CoreActive || i.MountActive }

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type manager struct {
	platform   string
	systemdDir string
	launchdDir string
	domain     string
	run        commandRunner
}

func newManager() (*manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	return &manager{
		platform: runtime.GOOS, systemdDir: filepath.Join(configHome, "systemd", "user"),
		launchdDir: filepath.Join(home, "Library", "LaunchAgents"), domain: "gui/" + strconv.Itoa(os.Getuid()),
		run: runCommand,
	}, nil
}

func Discover(ctx context.Context) ([]Instance, error) {
	manager, err := newManager()
	if err != nil {
		return nil, err
	}
	return manager.discover(ctx)
}

func (m *manager) discover(ctx context.Context) ([]Instance, error) {
	switch m.platform {
	case "linux":
		return m.discoverSystemd(ctx)
	case "darwin":
		return m.discoverLaunchd(ctx)
	default:
		return nil, fmt.Errorf("DFS instance administration is unsupported on %s", m.platform)
	}
}

func (m *manager) discoverSystemd(ctx context.Context) ([]Instance, error) {
	entries, err := os.ReadDir(m.systemdDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var instances []Instance
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "dfs-core-") || !strings.HasSuffix(entry.Name(), ".service") {
			continue
		}
		serviceID := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "dfs-core-"), ".service")
		if len(serviceID) != 12 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.systemdDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		arguments, err := systemdExecArguments(string(data))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		instance, err := instanceFromArguments(arguments, serviceID, "linux")
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		instance.CoreActive = m.commandSucceeds(ctx, "systemctl", "--user", "is-active", "--quiet", "dfs-core-"+serviceID+".service")
		instance.MountActive = m.commandSucceeds(ctx, "systemctl", "--user", "is-active", "--quiet", "dfs-mount-"+serviceID+".service")
		instance.CoreEnabled = m.commandSucceeds(ctx, "systemctl", "--user", "is-enabled", "--quiet", "dfs-core-"+serviceID+".service")
		instance.MountEnabled = m.commandSucceeds(ctx, "systemctl", "--user", "is-enabled", "--quiet", "dfs-mount-"+serviceID+".service")
		instances = append(instances, instance)
	}
	sortInstances(instances)
	return instances, nil
}

func (m *manager) discoverLaunchd(ctx context.Context) ([]Instance, error) {
	entries, err := os.ReadDir(m.launchdDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	disabled := map[string]bool{}
	if output, printErr := m.run(ctx, "launchctl", "print-disabled", m.domain); printErr == nil {
		disabled = parseLaunchdDisabled(string(output))
	}
	var instances []Instance
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "io.bitbeamer.dfs.core.") || !strings.HasSuffix(entry.Name(), ".plist") {
			continue
		}
		serviceID := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "io.bitbeamer.dfs.core."), ".plist")
		if len(serviceID) != 12 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.launchdDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		arguments, err := plistProgramArguments(data)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		instance, err := instanceFromArguments(arguments, serviceID, "darwin")
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		instance.CoreActive = m.commandSucceeds(ctx, "launchctl", "print", m.domain+"/io.bitbeamer.dfs.core."+serviceID)
		instance.MountActive = m.commandSucceeds(ctx, "launchctl", "print", m.domain+"/io.bitbeamer.dfs.mount."+serviceID)
		instance.CoreEnabled = !disabled["io.bitbeamer.dfs.core."+serviceID]
		instance.MountEnabled = !disabled["io.bitbeamer.dfs.mount."+serviceID]
		instances = append(instances, instance)
	}
	sortInstances(instances)
	return instances, nil
}

func parseLaunchdDisabled(output string) map[string]bool {
	result := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=>", 2)
		if len(parts) != 2 {
			continue
		}
		label := strings.Trim(strings.TrimSpace(parts[0]), "\"")
		value := strings.Trim(strings.TrimSpace(parts[1]), ";")
		if label != "" {
			result[label] = value == "true"
		}
	}
	return result
}

func instanceFromArguments(arguments []string, serviceID, platform string) (Instance, error) {
	instance := Instance{Platform: platform, serviceID: serviceID, PairingPort: 7843}
	if len(arguments) > 0 {
		instance.Binary = arguments[0]
	}
	for index := 0; index < len(arguments)-1; index++ {
		switch arguments[index] {
		case "--repo":
			instance.Repository = arguments[index+1]
		case "--mountpoint":
			instance.Mountpoint = arguments[index+1]
		case "--pair-port":
			instance.PairingPort, _ = strconv.Atoi(arguments[index+1])
		}
	}
	if instance.Repository == "" || instance.Mountpoint == "" || instance.PairingPort < 1 || instance.PairingPort > 65535 {
		return Instance{}, errors.New("managed DFS service has incomplete repository, mountpoint, or port arguments")
	}
	instance.FileSystemID = serviceID
	var cfg struct {
		FileSystemID string `json:"filesystem_id"`
		Name         string `json:"name"`
		NetworkName  string `json:"network_name"`
	}
	data, err := os.ReadFile(filepath.Join(instance.Repository, ".git", "dfs", "config.json"))
	if err == nil && json.Unmarshal(data, &cfg) == nil {
		if cfg.FileSystemID != "" {
			instance.FileSystemID = cfg.FileSystemID
		}
		instance.Name = cfg.Name
		instance.NetworkName = cfg.NetworkName
	}
	return instance, nil
}

func systemdExecArguments(unit string) ([]string, error) {
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			return splitQuoted(strings.TrimPrefix(line, "ExecStart="))
		}
	}
	return nil, errors.New("ExecStart is missing")
}

func splitQuoted(value string) ([]string, error) {
	var arguments []string
	for index := 0; index < len(value); {
		for index < len(value) && (value[index] == ' ' || value[index] == '\t') {
			index++
		}
		if index == len(value) {
			break
		}
		var token strings.Builder
		quoted := false
		if value[index] == '"' {
			quoted = true
			index++
		}
		for index < len(value) {
			character := value[index]
			if character == '\\' && index+1 < len(value) {
				index++
				token.WriteByte(value[index])
				index++
				continue
			}
			if quoted && character == '"' {
				index++
				break
			}
			if !quoted && (character == ' ' || character == '\t') {
				break
			}
			token.WriteByte(character)
			index++
		}
		if quoted && (index == 0 || value[index-1] != '"') {
			return nil, errors.New("unterminated quoted argument")
		}
		arguments = append(arguments, strings.ReplaceAll(token.String(), "%%", "%"))
	}
	return arguments, nil
}

func plistProgramArguments(data []byte) ([]string, error) {
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	var arguments []string
	inArguments := false
	wantArray := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "key" {
				var key string
				if err := decoder.DecodeElement(&key, &value); err != nil {
					return nil, err
				}
				wantArray = key == "ProgramArguments"
			} else if value.Name.Local == "array" && wantArray {
				inArguments = true
				wantArray = false
			} else if value.Name.Local == "string" && inArguments {
				var argument string
				if err := decoder.DecodeElement(&argument, &value); err != nil {
					return nil, err
				}
				arguments = append(arguments, argument)
			}
		case xml.EndElement:
			if value.Name.Local == "array" && inArguments {
				inArguments = false
			}
		}
	}
	if len(arguments) == 0 {
		return nil, errors.New("ProgramArguments is missing")
	}
	return arguments, nil
}

func Find(instances []Instance, selector string) (Instance, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return Instance{}, errors.New("DFS instance name, ID, or repository is required")
	}
	var matches []Instance
	for _, instance := range instances {
		if selector == instance.FileSystemID || strings.HasPrefix(instance.FileSystemID, selector) || selector == instance.Name || selector == instance.NetworkName || filepath.Clean(selector) == filepath.Clean(instance.Repository) || filepath.Clean(selector) == filepath.Clean(instance.Mountpoint) {
			matches = append(matches, instance)
		}
	}
	if len(matches) != 1 {
		return Instance{}, fmt.Errorf("selector %q matches %d DFS instances", selector, len(matches))
	}
	return matches[0], nil
}

func Start(ctx context.Context, instances []Instance) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	return manager.start(ctx, instances)
}

func (m *manager) start(ctx context.Context, instances []Instance) error {
	var failures []error
	for _, instance := range instances {
		var err error
		if m.platform == "linux" {
			if _, err = m.run(ctx, "systemctl", "--user", "start", "dfs-core-"+instance.serviceID+".service"); err == nil {
				_, err = m.run(ctx, "systemctl", "--user", "start", "dfs-mount-"+instance.serviceID+".service")
			}
		} else {
			for _, kind := range []string{"core", "mount"} {
				label := "io.bitbeamer.dfs." + kind + "." + instance.serviceID
				path := filepath.Join(m.launchdDir, label+".plist")
				if !m.commandSucceeds(ctx, "launchctl", "print", m.domain+"/"+label) {
					if _, err = m.run(ctx, "launchctl", "bootstrap", m.domain, path); err != nil {
						break
					}
				}
				if _, err = m.run(ctx, "launchctl", "kickstart", m.domain+"/"+label); err != nil {
					break
				}
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("start %s: %w", instanceLabel(instance), err))
		}
	}
	return errors.Join(failures...)
}

func Restart(ctx context.Context, instances []Instance) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	if err := manager.stop(ctx, instances); err != nil {
		return err
	}
	return manager.start(ctx, instances)
}

func Stop(ctx context.Context, instances []Instance) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	return manager.stop(ctx, instances)
}

func (m *manager) stop(ctx context.Context, instances []Instance) error {
	var failures []error
	for _, instance := range instances {
		var err error
		if m.platform == "linux" {
			_, err = m.run(ctx, "systemctl", "--user", "stop", "dfs-mount-"+instance.serviceID+".service", "dfs-core-"+instance.serviceID+".service")
		} else {
			var labels []string
			if instance.MountActive {
				labels = append(labels, "io.bitbeamer.dfs.mount."+instance.serviceID)
			}
			if instance.CoreActive {
				labels = append(labels, "io.bitbeamer.dfs.core."+instance.serviceID)
			}
			for _, label := range labels {
				_, stopErr := m.run(ctx, "launchctl", "bootout", m.domain+"/"+label)
				if stopErr != nil && err == nil {
					err = stopErr
				}
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("stop %s: %w", instanceLabel(instance), err))
		}
	}
	return errors.Join(failures...)
}

func Update(ctx context.Context, instances []Instance, binary, installer string) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	return manager.upgrade(ctx, instances, binary, installer)
}

func ValidateUpgrade(ctx context.Context, instances []Instance, binary, installer string) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	_, _, _, err = manager.validateUpgrade(ctx, instances, binary, installer)
	return err
}

func Repair(ctx context.Context, instances []Instance, binary, installer string) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	return manager.repair(ctx, instances, binary, installer)
}

// update is retained for package-level tests of the old implementation entry
// point. Public callers use Repair or Update, whose semantics are distinct.
func (m *manager) update(ctx context.Context, instances []Instance, binary, installer string) error {
	return m.repair(ctx, instances, binary, installer)
}

func (m *manager) repair(ctx context.Context, instances []Instance, binary, installer string) error {
	binary, err := filepath.Abs(binary)
	if err != nil {
		return err
	}
	if info, err := os.Stat(binary); err != nil || info.Mode()&0o111 == 0 {
		return fmt.Errorf("DFS update binary is not executable: %s", binary)
	}
	installer, err = resolveInstaller(m.platform, installer)
	if err != nil {
		return err
	}
	var failures []error
	for _, instance := range instances {
		definitions, snapshotErr := m.snapshotDefinitions(instance)
		if snapshotErr != nil {
			failures = append(failures, fmt.Errorf("repair %s: snapshot service definitions: %w", instanceLabel(instance), snapshotErr))
			continue
		}
		_, installErr := m.run(ctx, installer, "--pair-port", strconv.Itoa(instance.PairingPort), "--no-start", "--no-enable", instance.Repository, instance.Mountpoint, binary)
		if installErr == nil {
			installErr = m.restoreState(ctx, instance)
		}
		if installErr == nil {
			installErr = m.verifyRunningState(ctx, instance)
		}
		if installErr != nil {
			rollbackErr := m.restoreDefinitions(ctx, definitions, instance)
			failures = append(failures, errors.Join(fmt.Errorf("repair %s: %w", instanceLabel(instance), installErr), errorWithContext("restore previous service definitions", rollbackErr)))
		}
	}
	return errors.Join(failures...)
}

type definitionSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func (m *manager) definitionPaths(instance Instance) []string {
	if m.platform == "linux" {
		return []string{
			filepath.Join(m.systemdDir, "dfs-core-"+instance.serviceID+".service"),
			filepath.Join(m.systemdDir, "dfs-mount-"+instance.serviceID+".service"),
		}
	}
	return []string{
		filepath.Join(m.launchdDir, "io.bitbeamer.dfs.core."+instance.serviceID+".plist"),
		filepath.Join(m.launchdDir, "io.bitbeamer.dfs.mount."+instance.serviceID+".plist"),
	}
}

func (m *manager) snapshotDefinitions(instance Instance) ([]definitionSnapshot, error) {
	var snapshots []definitionSnapshot
	for _, path := range m.definitionPaths(instance) {
		snapshot := definitionSnapshot{path: path}
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, snapshot)
			continue
		}
		if err != nil {
			return nil, err
		}
		snapshot.data, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		snapshot.mode = info.Mode().Perm()
		snapshot.exists = true
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func (m *manager) restoreDefinitions(ctx context.Context, snapshots []definitionSnapshot, instance Instance) error {
	var failures []error
	for _, snapshot := range snapshots {
		if snapshot.exists {
			if err := os.WriteFile(snapshot.path, snapshot.data, snapshot.mode); err != nil {
				failures = append(failures, err)
			}
		} else if err := os.Remove(snapshot.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	if m.platform == "linux" {
		if _, err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			failures = append(failures, err)
		}
	}
	if err := m.restoreState(ctx, instance); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (m *manager) verifyRunningState(ctx context.Context, instance Instance) error {
	var failures []error
	checks := []struct {
		kind   string
		active bool
	}{
		{kind: "core", active: instance.CoreActive},
		{kind: "mount", active: instance.MountActive},
	}
	for _, check := range checks {
		if !check.active {
			continue
		}
		if m.platform == "linux" {
			if !m.commandSucceeds(ctx, "systemctl", "--user", "is-active", "--quiet", "dfs-"+check.kind+"-"+instance.serviceID+".service") {
				failures = append(failures, fmt.Errorf("%s service did not become active", check.kind))
			}
		} else if !m.commandSucceeds(ctx, "launchctl", "print", m.domain+"/io.bitbeamer.dfs."+check.kind+"."+instance.serviceID) {
			failures = append(failures, fmt.Errorf("%s service did not become active", check.kind))
		}
	}
	return errors.Join(failures...)
}

func (m *manager) restoreState(ctx context.Context, instance Instance) error {
	if m.platform == "linux" {
		for _, item := range []struct {
			name    string
			enabled bool
			active  bool
		}{
			{name: "dfs-core-" + instance.serviceID + ".service", enabled: instance.CoreEnabled, active: instance.CoreActive},
			{name: "dfs-mount-" + instance.serviceID + ".service", enabled: instance.MountEnabled, active: instance.MountActive},
		} {
			action := "disable"
			if item.enabled {
				action = "enable"
			}
			if _, err := m.run(ctx, "systemctl", "--user", action, item.name); err != nil {
				return err
			}
			if item.active {
				if _, err := m.run(ctx, "systemctl", "--user", "start", item.name); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, item := range []struct {
		kind    string
		enabled bool
		active  bool
	}{
		{kind: "core", enabled: instance.CoreEnabled, active: instance.CoreActive},
		{kind: "mount", enabled: instance.MountEnabled, active: instance.MountActive},
	} {
		label := "io.bitbeamer.dfs." + item.kind + "." + instance.serviceID
		action := "disable"
		if item.enabled {
			action = "enable"
		}
		if _, err := m.run(ctx, "launchctl", action, m.domain+"/"+label); err != nil {
			return err
		}
		if item.active {
			path := filepath.Join(m.launchdDir, label+".plist")
			if _, err := m.run(ctx, "launchctl", "bootstrap", m.domain, path); err != nil {
				return err
			}
			if _, err := m.run(ctx, "launchctl", "kickstart", m.domain+"/"+label); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *manager) upgrade(ctx context.Context, instances []Instance, candidate, installer string) error {
	candidate, installed, installer, err := m.validateUpgrade(ctx, instances, candidate, installer)
	if err != nil {
		return err
	}
	installDirectory := filepath.Dir(installed)
	backup, err := stageCopy(installed, installDirectory, ".dfs-rollback-")
	if err != nil {
		return fmt.Errorf("stage DFS rollback executable: %w", err)
	}
	defer os.Remove(backup)
	staged, err := stageCopy(candidate, installDirectory, ".dfs-upgrade-")
	if err != nil {
		return fmt.Errorf("stage DFS upgrade executable: %w", err)
	}
	if err := os.Rename(staged, installed); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("activate DFS upgrade executable: %w", err)
	}
	if err := syncDirectory(installDirectory); err != nil {
		persistErr := fmt.Errorf("persist DFS upgrade executable: %w", err)
		rollbackErr := os.Rename(backup, installed)
		if rollbackErr == nil {
			rollbackErr = syncDirectory(installDirectory)
		}
		return errors.Join(persistErr, errorWithContext("rollback failed", rollbackErr))
	}
	if err := m.repair(ctx, instances, installed, installer); err != nil {
		rollbackErr := os.Rename(backup, installed)
		if rollbackErr == nil {
			rollbackErr = syncDirectory(installDirectory)
		}
		if rollbackErr == nil {
			rollbackErr = m.repair(ctx, instances, installed, installer)
		}
		return errors.Join(fmt.Errorf("upgrade failed: %w", err), errorWithContext("rollback failed", rollbackErr))
	}
	return nil
}

func (m *manager) validateUpgrade(ctx context.Context, instances []Instance, candidate, installer string) (string, string, string, error) {
	if len(instances) == 0 {
		return "", "", "", errors.New("no managed DFS filesystem services found")
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", "", "", err
	}
	if info, statErr := os.Stat(candidate); statErr != nil || info.Mode()&0o111 == 0 {
		return "", "", "", fmt.Errorf("DFS upgrade candidate is not executable: %s", candidate)
	}
	if _, err := m.run(ctx, candidate, "--version"); err != nil {
		return "", "", "", fmt.Errorf("validate DFS upgrade candidate: %w", err)
	}
	installed := instances[0].Binary
	if installed == "" {
		return "", "", "", errors.New("managed DFS service does not identify its installed executable")
	}
	installed, err = filepath.Abs(installed)
	if err != nil {
		return "", "", "", err
	}
	for _, instance := range instances[1:] {
		binary, absErr := filepath.Abs(instance.Binary)
		if absErr != nil || binary != installed {
			return "", "", "", errors.New("managed DFS services do not share one installed executable")
		}
	}
	same, err := sameFileContent(candidate, installed)
	if err != nil {
		return "", "", "", err
	}
	if same {
		return "", "", "", errors.New("upgrade candidate is identical to the installed DFS executable; use service repair to reinstall definitions")
	}
	installer, err = resolveInstaller(m.platform, installer)
	if err != nil {
		return "", "", "", err
	}
	return candidate, installed, installer, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func sameFileContent(first, second string) (bool, error) {
	firstData, err := os.ReadFile(first)
	if err != nil {
		return false, err
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		return false, err
	}
	return sha256.Sum256(firstData) == sha256.Sum256(secondData), nil
}

func stageCopy(source, destinationDirectory, pattern string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	output, err := os.CreateTemp(destinationDirectory, pattern)
	if err != nil {
		return "", err
	}
	path := output.Name()
	failed := true
	defer func() {
		_ = output.Close()
		if failed {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return "", err
	}
	if err := output.Chmod(info.Mode().Perm()); err != nil {
		return "", err
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	if err := output.Close(); err != nil {
		return "", err
	}
	failed = false
	return path, nil
}

func errorWithContext(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func Uninstall(ctx context.Context, instances []Instance) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	return manager.uninstall(ctx, instances)
}

// UninstallAndPurge removes managed service definitions and permanently
// deletes their local repositories. Repository targets are validated before
// any services are changed.
func UninstallAndPurge(ctx context.Context, instances []Instance) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	return manager.uninstallAndPurge(ctx, instances)
}

func (m *manager) uninstallAndPurge(ctx context.Context, instances []Instance) error {
	targets, err := purgeTargets(instances)
	if err != nil {
		return err
	}
	if err := m.uninstall(ctx, instances); err != nil {
		return err
	}
	var failures []error
	for _, target := range targets {
		if err := purgeRepository(ctx, target); err != nil {
			failures = append(failures, fmt.Errorf("purge repository %s: %w", target, err))
			continue
		}
		if err := dfssetup.Forget(target); err != nil {
			failures = append(failures, fmt.Errorf("remove setup state for %s: %w", target, err))
		}
	}
	return errors.Join(failures...)
}

func purgeTargets(instances []Instance) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory for purge safety checks: %w", err)
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(instances))
	targets := make([]string, 0, len(instances))
	for _, instance := range instances {
		target := strings.TrimSpace(instance.Repository)
		if target == "" {
			return nil, fmt.Errorf("refusing to purge %s: repository path is empty", instanceLabel(instance))
		}
		if !filepath.IsAbs(target) {
			return nil, fmt.Errorf("refusing to purge %s: repository path is not absolute: %s", instanceLabel(instance), target)
		}
		target, err = filepath.Abs(target)
		if err != nil {
			return nil, fmt.Errorf("resolve repository path %q: %w", instance.Repository, err)
		}
		target = filepath.Clean(target)
		root := filepath.VolumeName(target) + string(os.PathSeparator)
		if target == root || target == home {
			return nil, fmt.Errorf("refusing to purge unsafe repository path %s", target)
		}
		info, statErr := os.Lstat(target)
		switch {
		case errors.Is(statErr, os.ErrNotExist):
		case statErr != nil:
			return nil, fmt.Errorf("inspect repository path %s: %w", target, statErr)
		case info.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("refusing to purge repository through symbolic link %s", target)
		case !info.IsDir():
			return nil, fmt.Errorf("refusing to purge repository path that is not a directory: %s", target)
		default:
			gitInfo, gitErr := os.Stat(filepath.Join(target, ".git"))
			if gitErr != nil || !gitInfo.IsDir() {
				return nil, fmt.Errorf("refusing to purge path without a DFS Git repository: %s", target)
			}
		}
		if !seen[target] {
			seen[target] = true
			targets = append(targets, target)
		}
	}
	return targets, nil
}

func purgeRepository(ctx context.Context, root string) error {
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			// git-annex freezes object directories at 0555. Restore owner
			// access before RemoveAll descends into and empties them.
			return os.Chmod(path, info.Mode().Perm()|0o700)
		}
		return nil
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("make repository removable: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func (m *manager) uninstall(ctx context.Context, instances []Instance) error {
	var failures []error
	for _, instance := range instances {
		if m.platform == "linux" {
			mountUnit := "dfs-mount-" + instance.serviceID + ".service"
			coreUnit := "dfs-core-" + instance.serviceID + ".service"
			if _, err := m.run(ctx, "systemctl", "--user", "disable", "--now", mountUnit, coreUnit); err != nil {
				failures = append(failures, fmt.Errorf("uninstall %s: stop managed services before removing definitions: %w", instanceLabel(instance), err))
				continue
			}
			for _, path := range []string{filepath.Join(m.systemdDir, mountUnit), filepath.Join(m.systemdDir, coreUnit)} {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					failures = append(failures, fmt.Errorf("uninstall %s: %w", instanceLabel(instance), err))
				}
			}
		} else {
			stopFailed := false
			for _, kind := range []string{"mount", "core"} {
				label := "io.bitbeamer.dfs." + kind + "." + instance.serviceID
				active := instance.MountActive
				if kind == "core" {
					active = instance.CoreActive
				}
				if active {
					if _, err := m.run(ctx, "launchctl", "bootout", m.domain+"/"+label); err != nil {
						failures = append(failures, fmt.Errorf("uninstall %s: stop %s before removing its definition: %w", instanceLabel(instance), kind, err))
						stopFailed = true
					}
				}
			}
			if stopFailed {
				continue
			}
			for _, kind := range []string{"mount", "core"} {
				label := "io.bitbeamer.dfs." + kind + "." + instance.serviceID
				if err := os.Remove(filepath.Join(m.launchdDir, label+".plist")); err != nil && !errors.Is(err, os.ErrNotExist) {
					failures = append(failures, fmt.Errorf("uninstall %s: %w", instanceLabel(instance), err))
				}
			}
		}
	}
	if m.platform == "linux" {
		if _, err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			failures = append(failures, fmt.Errorf("reload systemd user services: %w", err))
		}
	}
	return errors.Join(failures...)
}

func resolveInstaller(platform, explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	name := "install-cachyos.sh"
	if platform == "darwin" {
		name = "install-macos.sh"
	} else if platform != "linux" {
		return "", fmt.Errorf("DFS instance administration is unsupported on %s", platform)
	}
	candidates := []string{filepath.Join("scripts", name)}
	if home, err := os.UserHomeDir(); err == nil {
		if platform == "darwin" {
			candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "DFS", "scripts", name))
		} else {
			candidates = append(candidates, filepath.Join(home, ".local", "lib", "dfs", name))
		}
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "..", "scripts", name))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return filepath.Abs(candidate)
		}
	}
	return "", fmt.Errorf("cannot find scripts/%s; run from the DFS source tree or pass --installer", name)
}

func (m *manager) commandSucceeds(ctx context.Context, name string, arguments ...string) bool {
	_, err := m.run(ctx, name, arguments...)
	return err == nil
}

func runCommand(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return output, errors.New(message)
	}
	return output, nil
}

func sortInstances(instances []Instance) {
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].NetworkName != instances[j].NetworkName {
			return instances[i].NetworkName < instances[j].NetworkName
		}
		return instances[i].FileSystemID < instances[j].FileSystemID
	})
}

func instanceLabel(instance Instance) string {
	if instance.NetworkName != "" {
		return instance.NetworkName
	}
	if instance.Name != "" {
		return instance.Name
	}
	return instance.serviceID
}
