package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
	"golang.org/x/sys/unix"
)

type Phase string

const (
	PhaseDiscovered           Phase = "discovered"
	PhaseApprovalRequested    Phase = "approval requested"
	PhaseApproved             Phase = "approved"
	PhaseRepositoryPrepared   Phase = "repository prepared"
	PhaseMembershipReconciled Phase = "membership reconciled"
	PhaseServiceInstalled     Phase = "service installed"
	PhaseMounted              Phase = "mounted"
	PhaseVerified             Phase = "verified"
)

type State struct {
	Version        int       `json:"version"`
	Phase          Phase     `json:"phase"`
	Invitation     string    `json:"invitation,omitempty"`
	FileSystemID   string    `json:"filesystem_id"`
	PeerID         string    `json:"peer_id"`
	Name           string    `json:"name"`
	Repository     string    `json:"repository"`
	Mountpoint     string    `json:"mountpoint"`
	CacheLimit     int64     `json:"cache_limit_bytes"`
	Timeout        int64     `json:"discovery_timeout_nanoseconds"`
	NetworkName    string    `json:"network_name,omitempty"`
	OfferingPeer   string    `json:"offering_peer,omitempty"`
	Installer      string    `json:"installer,omitempty"`
	Binary         string    `json:"binary,omitempty"`
	OwnsRepository bool      `json:"owns_repository"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Options struct {
	Invitation string
	Repository string
	Mountpoint string
	Name       string
	CacheLimit int64
	Timeout    time.Duration
	Resume     bool
	Installer  string
	Binary     string
	Out        io.Writer
	Approve    func(*State) error
}

func StatePath(repositoryPath string) (string, error) {
	_ = repositoryPath
	root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(root, "dfs", "setup", "state.json"), nil
}

func Run(ctx context.Context, options Options) (*State, error) {
	path, err := StatePath(options.Repository)
	if err != nil {
		return nil, err
	}
	unlock, err := lock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, path, err := loadOrCreate(options)
	if err != nil {
		return nil, err
	}
	advance := func(phase Phase) error {
		state.Phase = phase
		state.UpdatedAt = time.Now().UTC()
		return save(path, state)
	}
	if before(state.Phase, PhaseApproved) {
		if options.Approve != nil {
			if err := options.Approve(state); err != nil {
				return nil, err
			}
		}
		if err := advance(PhaseApproved); err != nil {
			return nil, err
		}
	}
	if state.Installer == "" || state.Binary == "" {
		state.Installer, err = resolveInstaller(options.Installer)
		if err != nil {
			return nil, err
		}
		state.Binary = options.Binary
		if state.Binary == "" {
			state.Binary, err = os.Executable()
			if err != nil {
				return nil, err
			}
		}
		state.Binary, err = filepath.Abs(state.Binary)
		if err != nil {
			return nil, err
		}
		if err := save(path, state); err != nil {
			return nil, err
		}
	}
	if before(state.Phase, PhaseRepositoryPrepared) {
		if _, err := os.Stat(config.Path(state.Repository)); err == nil {
			if _, completeErr := peer.CompletePairing(ctx, state.Repository); completeErr != nil && !strings.Contains(completeErr.Error(), "no incomplete") {
				return nil, fmt.Errorf("resume reciprocal pairing: %w", completeErr)
			}
			repo, openErr := repository.Open(state.Repository)
			if openErr != nil {
				return nil, openErr
			}
			state.NetworkName = repo.Config.NetworkName
			_ = repo.Close()
		} else {
			if state.OwnsRepository {
				if removeErr := os.RemoveAll(state.Repository); removeErr != nil {
					return nil, fmt.Errorf("clear interrupted repository clone: %w", removeErr)
				}
			}
			result, joinErr := peer.PairAndJoinWithOptions(ctx, state.Invitation, state.Repository, state.Name, state.CacheLimit, time.Duration(state.Timeout), true, peer.PairOptions{
				PeerID: state.PeerID, StateDirectory: filepath.Join(filepath.Dir(path), "pairing"),
			})
			if joinErr != nil {
				return nil, joinErr
			}
			state.NetworkName = result.NetworkName
			state.OfferingPeer = result.OfferingPeer
			if closeErr := result.Repository.Close(); closeErr != nil {
				return nil, closeErr
			}
		}
		state.Invitation = ""
		if err := advance(PhaseRepositoryPrepared); err != nil {
			return nil, err
		}
	}
	if before(state.Phase, PhaseMembershipReconciled) {
		if err := advance(PhaseMembershipReconciled); err != nil {
			return nil, err
		}
	}
	if before(state.Phase, PhaseServiceInstalled) {
		if err := install(ctx, options, state); err != nil {
			return nil, err
		}
		if err := advance(PhaseServiceInstalled); err != nil {
			return nil, err
		}
	}
	if before(state.Phase, PhaseMounted) {
		if _, err := os.Stat(state.Mountpoint); err != nil {
			return nil, fmt.Errorf("inspect DFS mountpoint: %w", err)
		}
		if err := advance(PhaseMounted); err != nil {
			return nil, err
		}
	}
	if before(state.Phase, PhaseVerified) {
		command := exec.CommandContext(ctx, state.Binary, "--repo", state.Repository, "health")
		command.Stdout, command.Stderr = options.Out, options.Out
		if err := command.Run(); err != nil {
			return nil, fmt.Errorf("verify installed DFS service: %w", err)
		}
		if err := advance(PhaseVerified); err != nil {
			return nil, err
		}
	}
	_ = os.RemoveAll(filepath.Join(filepath.Dir(path), "pairing"))
	return state, nil
}

func Abort(ctx context.Context, repositoryPath, installer string, out io.Writer) error {
	path, err := StatePath(repositoryPath)
	if err != nil {
		return err
	}
	unlock, err := lock(path)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := load(path)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no DFS setup transaction is recorded")
	}
	if err != nil {
		return err
	}
	if state.Phase == PhaseVerified {
		return errors.New("DFS setup is already complete; uninstall it explicitly instead of aborting")
	}
	if !before(state.Phase, PhaseServiceInstalled) {
		resolved := state.Installer
		if installer != "" {
			resolved, err = resolveInstaller(installer)
		}
		if resolved == "" {
			resolved, err = resolveInstaller("")
		}
		resolveErr := err
		if resolveErr != nil {
			return resolveErr
		}
		command := exec.CommandContext(ctx, resolved, "--uninstall")
		command.Stdout, command.Stderr = out, out
		if err := command.Run(); err != nil {
			return fmt.Errorf("uninstall DFS service: %w", err)
		}
	}
	if state.OwnsRepository {
		if err := os.RemoveAll(state.Repository); err != nil {
			return fmt.Errorf("remove setup repository: %w", err)
		}
	}
	_ = os.Remove(state.Mountpoint)
	_ = os.RemoveAll(filepath.Join(filepath.Dir(path), "pairing"))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func loadOrCreate(options Options) (*State, string, error) {
	repositoryPath, err := filepath.Abs(options.Repository)
	if err != nil {
		return nil, "", err
	}
	mountpoint, err := filepath.Abs(options.Mountpoint)
	if err != nil {
		return nil, "", err
	}
	path, err := StatePath(repositoryPath)
	if err != nil {
		return nil, "", err
	}
	if options.Resume {
		state, err := load(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", errors.New("no DFS setup transaction is recorded")
		}
		return state, path, err
	}
	if _, err := os.Stat(path); err == nil {
		return nil, "", errors.New("a DFS setup transaction already exists; use dfs setup --resume or --abort")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	invitation, err := peer.DecodeInvitation(options.Invitation)
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(repositoryPath); err == nil {
		return nil, "", fmt.Errorf("repository destination already exists: %s", repositoryPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	peerID, err := peer.NewPeerID()
	if err != nil {
		return nil, "", err
	}
	name := strings.TrimSpace(options.Name)
	if name == "" {
		name, err = os.Hostname()
		if err != nil {
			return nil, "", err
		}
	}
	state := &State{Version: 1, Phase: PhaseDiscovered, Invitation: strings.TrimSpace(options.Invitation), FileSystemID: invitation.FileSystemID,
		PeerID: peerID, Name: name, Repository: repositoryPath, Mountpoint: mountpoint, CacheLimit: options.CacheLimit,
		Timeout: int64(options.Timeout), OwnsRepository: true, UpdatedAt: time.Now().UTC()}
	if err := save(path, state); err != nil {
		return nil, "", err
	}
	state.Phase = PhaseApprovalRequested
	state.UpdatedAt = time.Now().UTC()
	return state, path, save(path, state)
}

func save(path string, state *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Version != 1 || indexPhase(state.Phase) < 0 || state.Repository == "" || state.Mountpoint == "" || state.PeerID == "" {
		return nil, errors.New("recorded DFS setup transaction is invalid")
	}
	return &state, nil
}

var phases = []Phase{PhaseDiscovered, PhaseApprovalRequested, PhaseApproved, PhaseRepositoryPrepared, PhaseMembershipReconciled, PhaseServiceInstalled, PhaseMounted, PhaseVerified}

func before(current, wanted Phase) bool {
	return indexPhase(current) < indexPhase(wanted)
}

func indexPhase(value Phase) int {
	for i, phase := range phases {
		if phase == value {
			return i
		}
	}
	return -1
}

func install(ctx context.Context, options Options, state *State) error {
	command := exec.CommandContext(ctx, state.Installer, state.Repository, state.Mountpoint, state.Binary)
	command.Stdout, command.Stderr = options.Out, options.Out
	if err := command.Run(); err != nil {
		return fmt.Errorf("install DFS service: %w", err)
	}
	return nil
}

func lock(statePath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(filepath.Dir(statePath), "setup.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another DFS setup process is already running")
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

func resolveInstaller(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	name := "install-cachyos.sh"
	if runtime.GOOS == "darwin" {
		name = "install-macos.sh"
	} else if runtime.GOOS != "linux" {
		return "", fmt.Errorf("automatic service installation is unsupported on %s", runtime.GOOS)
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
	return "", fmt.Errorf("cannot find scripts/%s; run dfs setup from the DFS source tree or pass --installer", name)
}
