package setup

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	Version             int                         `json:"version"`
	Phase               Phase                       `json:"phase"`
	Create              bool                        `json:"create,omitempty"`
	Invitation          string                      `json:"invitation,omitempty"`
	Approval            peer.JoinRequestCredentials `json:"approval,omitempty"`
	FileSystemID        string                      `json:"filesystem_id"`
	PeerID              string                      `json:"peer_id"`
	Name                string                      `json:"name"`
	GitName             string                      `json:"git_name,omitempty"`
	GitEmail            string                      `json:"git_email,omitempty"`
	Repository          string                      `json:"repository"`
	Mountpoint          string                      `json:"mountpoint"`
	CacheLimit          int64                       `json:"cache_limit_bytes"`
	Timeout             int64                       `json:"discovery_timeout_nanoseconds"`
	NetworkName         string                      `json:"network_name,omitempty"`
	OfferingPeer        string                      `json:"offering_peer,omitempty"`
	Installer           string                      `json:"installer,omitempty"`
	Binary              string                      `json:"binary,omitempty"`
	PairingPort         int                         `json:"pairing_port"`
	VerificationTimeout int64                       `json:"verification_timeout_nanoseconds,omitempty"`
	Acknowledgements    []peer.SetupAcknowledgement `json:"acknowledgements,omitempty"`
	ClusterVerifiedAt   time.Time                   `json:"cluster_verified_at,omitempty"`
	OwnsRepository      bool                        `json:"owns_repository"`
	UpdatedAt           time.Time                   `json:"updated_at"`
}

type Options struct {
	Invitation          string
	FileSystemID        string
	Create              bool
	NetworkName         string
	Repository          string
	Mountpoint          string
	Name                string
	GitName             string
	GitEmail            string
	CacheLimit          int64
	Timeout             time.Duration
	Resume              bool
	Installer           string
	Binary              string
	PairingPort         int
	VerificationTimeout time.Duration
	Out                 io.Writer
	Approve             func(*State) error
	CheckCluster        func(context.Context, *repository.Repository, time.Duration, time.Duration) (peer.MeshReport, error)
}

func StatePath(repositoryPath string) (string, error) {
	repositoryPath, err := filepath.Abs(repositoryPath)
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state")
	}
	digest := sha256.Sum256([]byte(filepath.Clean(repositoryPath)))
	return filepath.Join(root, "dfs", "setup", fmt.Sprintf("state-%x.json", digest[:8])), nil
}

func Run(ctx context.Context, options Options) (*State, error) {
	if options.Out == nil {
		options.Out = io.Discard
	}
	path, err := StatePath(options.Repository)
	if err != nil {
		return nil, err
	}
	if err := migrateLegacyState(options.Repository, path); err != nil {
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
	if state.PairingPort == 0 && before(state.Phase, PhaseServiceInstalled) {
		state.PairingPort, err = choosePairingPort(options.PairingPort)
		if err != nil {
			return nil, err
		}
		if err := save(path, state); err != nil {
			return nil, err
		}
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
		if !state.Create && state.Invitation == "" {
			if err := awaitApproval(ctx, path, state, options.Out); err != nil {
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
			if !state.Create {
				if _, completeErr := peer.CompletePairing(ctx, state.Repository); completeErr != nil && !strings.Contains(completeErr.Error(), "no incomplete") {
					return nil, fmt.Errorf("resume reciprocal pairing: %w", completeErr)
				}
			}
			repo, openErr := repository.Open(state.Repository)
			if openErr != nil {
				return nil, openErr
			}
			state.NetworkName = repo.Config.NetworkName
			_ = repo.Close()
		} else if state.Create {
			if state.OwnsRepository {
				if removeErr := os.RemoveAll(state.Repository); removeErr != nil {
					return nil, fmt.Errorf("clear interrupted repository creation: %w", removeErr)
				}
			}
			repo, initErr := repository.InitWithIdentity(ctx, state.Repository, state.Name, state.CacheLimit,
				repository.GitIdentity{Name: state.GitName, Email: state.GitEmail})
			if initErr != nil {
				return nil, fmt.Errorf("initialize DFS repository: %w", initErr)
			}
			if err := repo.SetNetworkName(state.NetworkName); err != nil {
				_ = repo.Close()
				return nil, fmt.Errorf("set DFS filesystem name: %w", err)
			}
			state.FileSystemID, err = repo.FileSystemID(ctx)
			if err != nil {
				_ = repo.Close()
				return nil, err
			}
			state.PeerID = repo.Config.PeerID
			if closeErr := repo.Close(); closeErr != nil {
				return nil, closeErr
			}
		} else {
			if state.OwnsRepository {
				if removeErr := os.RemoveAll(state.Repository); removeErr != nil {
					return nil, fmt.Errorf("clear interrupted repository clone: %w", removeErr)
				}
			}
			result, joinErr := peer.PairAndJoinWithOptions(ctx, state.Invitation, state.Repository, state.Name, state.CacheLimit, time.Duration(state.Timeout), true, peer.PairOptions{
				PeerID: state.PeerID, StateDirectory: pairingPath(path), PairingPort: state.PairingPort, GitName: state.GitName, GitEmail: state.GitEmail,
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
		repo, openErr := repository.Open(state.Repository)
		if openErr != nil {
			return nil, openErr
		}
		reconcileErr := peer.ReconcileMembership(ctx, repo)
		closeErr := repo.Close()
		if reconcileErr != nil {
			return nil, fmt.Errorf("reconcile DFS membership: %w", reconcileErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
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
		if err := verifySetupCluster(ctx, path, state, options); err != nil {
			return nil, err
		}
		if err := advance(PhaseVerified); err != nil {
			return nil, err
		}
	}
	_ = os.RemoveAll(pairingPath(path))
	return state, nil
}

func Abort(ctx context.Context, repositoryPath, installer string, out io.Writer) error {
	path, err := StatePath(repositoryPath)
	if err != nil {
		return err
	}
	if err := migrateLegacyState(repositoryPath, path); err != nil {
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
		command := exec.CommandContext(ctx, resolved, "--uninstall", state.Repository)
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
	_ = os.RemoveAll(pairingPath(path))
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
		if err != nil {
			return nil, "", err
		}
		if before(state.Phase, PhaseRepositoryPrepared) && state.GitName == "" && state.GitEmail == "" {
			state.GitName = strings.TrimSpace(options.GitName)
			state.GitEmail = strings.TrimSpace(options.GitEmail)
			if err := save(path, state); err != nil {
				return nil, "", err
			}
		}
		return state, path, nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil, "", errors.New("a DFS setup transaction already exists; use dfs setup resume or dfs setup abort")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	filesystemID := strings.TrimSpace(options.FileSystemID)
	if options.Create && (filesystemID != "" || strings.TrimSpace(options.Invitation) != "") {
		return nil, "", errors.New("creating a DFS filesystem cannot use an invitation or existing filesystem ID")
	}
	if strings.TrimSpace(options.Invitation) != "" {
		invitation, decodeErr := peer.DecodeInvitation(options.Invitation)
		if decodeErr != nil {
			return nil, "", decodeErr
		}
		filesystemID = invitation.FileSystemID
	}
	if !options.Create && len(filesystemID) < 16 {
		return nil, "", errors.New("a discovered DFS filesystem must be selected")
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
	networkName := strings.TrimSpace(options.NetworkName)
	if options.Create && networkName == "" {
		networkName = filepath.Base(mountpoint)
	}
	pairingPort, err := choosePairingPort(options.PairingPort)
	if err != nil {
		return nil, "", err
	}
	verificationTimeout := options.VerificationTimeout
	if verificationTimeout <= 0 {
		verificationTimeout = time.Minute
	}
	state := &State{Version: 1, Phase: PhaseDiscovered, Create: options.Create, Invitation: strings.TrimSpace(options.Invitation), FileSystemID: filesystemID,
		PeerID: peerID, Name: name, GitName: strings.TrimSpace(options.GitName), GitEmail: strings.TrimSpace(options.GitEmail), Repository: repositoryPath, Mountpoint: mountpoint, CacheLimit: options.CacheLimit,
		Timeout: int64(options.Timeout), NetworkName: networkName, PairingPort: pairingPort, VerificationTimeout: int64(verificationTimeout), OwnsRepository: true, UpdatedAt: time.Now().UTC()}
	if err := save(path, state); err != nil {
		return nil, "", err
	}
	state.Phase = PhaseApprovalRequested
	state.UpdatedAt = time.Now().UTC()
	return state, path, save(path, state)
}

func verifySetupCluster(ctx context.Context, statePath string, state *State, options Options) error {
	timeout := time.Duration(state.VerificationTimeout)
	if timeout <= 0 {
		timeout = time.Minute
	}
	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	repo, err := repository.Open(state.Repository)
	if err != nil {
		return err
	}
	defer repo.Close()
	checker := options.CheckCluster
	if checker == nil {
		checker = peer.CheckMesh
	}
	discoveryTimeout := time.Duration(state.Timeout)
	if discoveryTimeout <= 0 || discoveryTimeout > 2*time.Second {
		discoveryTimeout = 2 * time.Second
	}
	var last []peer.SetupAcknowledgement
	for {
		if err := peer.ReconcileMembership(verifyCtx, repo); err != nil {
			return fmt.Errorf("reconcile DFS membership before cluster verification: %w", err)
		}
		report, checkErr := checker(verifyCtx, repo, discoveryTimeout, 5*time.Second)
		if checkErr == nil {
			acknowledgements, ready := peer.EvaluateSetupAcknowledgements(report)
			localReady := false
			for _, acknowledgement := range acknowledgements {
				if acknowledgement.PeerID == state.PeerID && acknowledgement.Status == "READY" {
					localReady = true
					break
				}
			}
			ready = ready && localReady
			state.Acknowledgements = acknowledgements
			state.UpdatedAt = time.Now().UTC()
			if ready {
				state.ClusterVerifiedAt = state.UpdatedAt
			}
			if err := save(statePath, state); err != nil {
				return err
			}
			last = acknowledgements
			if ready {
				printSetupAcknowledgements(options.Out, acknowledgements)
				return nil
			}
		}
		select {
		case <-verifyCtx.Done():
			if len(last) > 0 {
				printSetupAcknowledgements(options.Out, last)
			}
			if checkErr != nil {
				return fmt.Errorf("verify directed DFS cluster: %w", checkErr)
			}
			return errors.New("online DFS members have not acknowledged every directed cluster connection; retry with dfs setup resume")
		case <-time.After(2 * time.Second):
			fmt.Fprintln(options.Out, "Waiting for online DFS members to acknowledge the new cluster topology...")
		}
	}
}

func printSetupAcknowledgements(out io.Writer, acknowledgements []peer.SetupAcknowledgement) {
	for _, acknowledgement := range acknowledgements {
		if acknowledgement.Detail == "" {
			fmt.Fprintf(out, "Cluster member %s: %s\n", acknowledgement.PeerName, acknowledgement.Status)
		} else {
			fmt.Fprintf(out, "Cluster member %s: %s (%s)\n", acknowledgement.PeerName, acknowledgement.Status, acknowledgement.Detail)
		}
	}
}

func awaitApproval(ctx context.Context, statePath string, state *State, out io.Writer) error {
	for {
		if !state.Approval.ExpiresAt.IsZero() && !state.Approval.ExpiresAt.After(time.Now()) {
			return errors.New("DFS join request expired; abort setup and start a new request")
		}
		discoveryTimeout := time.Duration(state.Timeout)
		if discoveryTimeout <= 0 {
			discoveryTimeout = 3 * time.Second
		}
		discoveryCtx, cancel := context.WithTimeout(ctx, discoveryTimeout+time.Second)
		offers, err := peer.Discover(discoveryCtx, discoveryTimeout)
		cancel()
		if err != nil {
			return fmt.Errorf("discover selected DFS filesystem: %w", err)
		}
		var selected peer.Network
		for _, network := range peer.GroupOffers(offers) {
			if network.FileSystemID == state.FileSystemID {
				selected = network
				break
			}
		}
		if len(selected.Offers) == 0 {
			return errors.New("selected DFS filesystem is no longer discoverable")
		}
		if state.Approval.RequestID == "" {
			credentials, err := peer.SubmitJoinRequest(ctx, selected, state.PeerID, state.Name, pairingPath(statePath), state.PairingPort, 15*time.Minute)
			if err != nil {
				return err
			}
			state.Approval = credentials
			state.UpdatedAt = time.Now().UTC()
			if err := save(statePath, state); err != nil {
				return err
			}
			printJoinApprovalInstruction(out, credentials.RequestID)
		}
		invitation, approved, err := peer.PollJoinApproval(ctx, selected, state.Approval)
		if err == nil && approved {
			encoded, encodeErr := invitation.Encode()
			if encodeErr != nil {
				return encodeErr
			}
			state.Invitation = encoded
			return save(statePath, state)
		}
		if err != nil {
			fmt.Fprintf(out, "Waiting for DFS join approval: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func printJoinApprovalInstruction(out io.Writer, requestID string) {
	fmt.Fprintf(out, "Join request %s is pending. On any existing peer run: dfs peer approve %s\n", requestID, requestID)
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
	command := exec.CommandContext(ctx, state.Installer, "--pair-port", fmt.Sprint(state.PairingPort), state.Repository, state.Mountpoint, state.Binary)
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
	file, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
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

func pairingPath(statePath string) string {
	return strings.TrimSuffix(statePath, filepath.Ext(statePath)) + "-pairing"
}

func migrateLegacyState(repositoryPath, target string) error {
	legacy := filepath.Join(filepath.Dir(target), "state.json")
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	state, err := load(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy DFS setup transaction: %w", err)
	}
	wanted, err := filepath.Abs(repositoryPath)
	if err != nil {
		return err
	}
	recorded, err := filepath.Abs(state.Repository)
	if err != nil || filepath.Clean(recorded) != filepath.Clean(wanted) {
		return nil
	}
	if err := os.Rename(legacy, target); err != nil {
		return fmt.Errorf("migrate DFS setup transaction: %w", err)
	}
	legacyPairing := filepath.Join(filepath.Dir(target), "pairing")
	if _, err := os.Stat(legacyPairing); err == nil {
		_ = os.Rename(legacyPairing, pairingPath(target))
	}
	return nil
}

func choosePairingPort(requested int) (int, error) {
	if requested != 0 {
		if requested < 1 || requested > 65535 {
			return 0, fmt.Errorf("pairing port must be between 1 and 65535")
		}
		if !pairingPortAvailable(requested) {
			return 0, fmt.Errorf("pairing port %d is already in use; choose another with --pair-port", requested)
		}
		return requested, nil
	}
	for port := peer.DefaultPairingPort; port <= peer.DefaultPairingPort+100; port++ {
		if pairingPortAvailable(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free DFS pairing port is available between %d and %d", peer.DefaultPairingPort, peer.DefaultPairingPort+100)
}

func pairingPortAvailable(port int) bool {
	address := fmt.Sprintf(":%d", port)
	udpListener, err := net.ListenPacket("udp", address)
	if err != nil {
		return false
	}
	_ = udpListener.Close()
	return true
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
