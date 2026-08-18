package instance

import (
	"context"
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
)

type Instance struct {
	FileSystemID string `json:"filesystem_id"`
	Name         string `json:"name"`
	NetworkName  string `json:"network_name"`
	Repository   string `json:"repository"`
	Mountpoint   string `json:"mountpoint"`
	PairingPort  int    `json:"pairing_port"`
	CoreActive   bool   `json:"core_active"`
	MountActive  bool   `json:"mount_active"`
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
		instances = append(instances, instance)
	}
	sortInstances(instances)
	return instances, nil
}

func instanceFromArguments(arguments []string, serviceID, platform string) (Instance, error) {
	instance := Instance{Platform: platform, serviceID: serviceID, PairingPort: 7843}
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
		if selector == instance.FileSystemID || strings.HasPrefix(instance.FileSystemID, selector) || selector == instance.Name || selector == instance.NetworkName || filepath.Clean(selector) == filepath.Clean(instance.Repository) {
			matches = append(matches, instance)
		}
	}
	if len(matches) != 1 {
		return Instance{}, fmt.Errorf("selector %q matches %d DFS instances", selector, len(matches))
	}
	return matches[0], nil
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
	return manager.update(ctx, instances, binary, installer)
}

func (m *manager) update(ctx context.Context, instances []Instance, binary, installer string) error {
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
		wasActive := instance.Active()
		_, installErr := m.run(ctx, installer, "--pair-port", strconv.Itoa(instance.PairingPort), instance.Repository, instance.Mountpoint, binary)
		if installErr == nil && !wasActive {
			started := instance
			started.CoreActive = true
			started.MountActive = true
			installErr = m.stop(ctx, []Instance{started})
		}
		if installErr != nil {
			failures = append(failures, fmt.Errorf("update %s: %w", instanceLabel(instance), installErr))
		}
	}
	return errors.Join(failures...)
}

func Uninstall(ctx context.Context, instances []Instance) error {
	manager, err := newManager()
	if err != nil {
		return err
	}
	return manager.uninstall(ctx, instances)
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
