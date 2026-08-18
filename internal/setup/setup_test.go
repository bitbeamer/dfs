package setup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
)

func TestSetupCreatesFirstFilesystemThroughManagedFlow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	repositoryPath := filepath.Join(home, "repository")
	mountpoint := filepath.Join(home, "mount")
	installer := filepath.Join(home, "install.sh")
	binary := filepath.Join(home, "dfs")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\nmkdir -p \"$4\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state, err := Run(context.Background(), Options{Create: true, NetworkName: "Home Files", Repository: repositoryPath,
		Mountpoint: mountpoint, Name: "ares", GitName: "DFS Test", GitEmail: "dfs@example.invalid", CacheLimit: 1 << 20, Installer: installer, Binary: binary, Out: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseVerified || state.FileSystemID == "" || state.NetworkName != "Home Files" {
		t.Fatalf("created setup state = %+v", state)
	}
	repo, err := repository.Open(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if repo.Config.NetworkName != "Home Files" || repo.Config.Name != "ares" || repo.Config.PeerID != state.PeerID {
		t.Fatalf("created repository config = %+v", repo.Config)
	}
	for key, wanted := range map[string]string{"user.name": "DFS Test", "user.email": "dfs@example.invalid"} {
		output, err := exec.Command("git", "-C", repositoryPath, "config", "--local", "--get", key).Output()
		if err != nil || strings.TrimSpace(string(output)) != wanted {
			t.Fatalf("local Git %s = %q, %v", key, output, err)
		}
	}
}

func TestSetupCreationRejectsExistingFilesystemSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	_, err := Run(context.Background(), Options{Create: true, FileSystemID: strings.Repeat("a", 40),
		Repository: filepath.Join(home, "repository"), Mountpoint: filepath.Join(home, "mount")})
	if err == nil || !strings.Contains(err.Error(), "cannot use") {
		t.Fatalf("creation selection error = %v", err)
	}
}

func TestSetupClusterVerificationPersistsIncompleteAcknowledgementsForResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repositoryPath := filepath.Join(home, "repository")
	repo, err := repository.InitWithIdentity(context.Background(), repositoryPath, "ares", 1<<20,
		repository.GitIdentity{Name: "DFS Test", Email: "dfs@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	peerID := repo.Config.PeerID
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(home, "state.json")
	state := &State{Version: 1, Phase: PhaseMounted, Repository: repositoryPath, Mountpoint: filepath.Join(home, "mount"), PeerID: peerID,
		// Leave enough time for membership reconciliation's local Git commands
		// before exercising the deliberately incomplete mesh timeout.
		VerificationTimeout: int64(time.Second), Timeout: int64(time.Millisecond)}
	checker := func(context.Context, *repository.Repository, time.Duration, time.Duration) (peer.MeshReport, error) {
		return peer.MeshReport{
			Peers:       []peer.MeshPeer{{PeerID: peerID, PeerName: "ares"}, {PeerID: "zeus-peer", PeerName: "zeus"}},
			Reports:     []peer.DiagnosticReport{{PeerID: peerID, ReconciliationStatus: "ready"}, {PeerID: "zeus-peer", ReconciliationStatus: "ready"}},
			Connections: []peer.MeshConnection{{FromPeerID: peerID, ToPeerID: "zeus-peer", Status: "OK"}, {FromPeerID: "zeus-peer", ToPeerID: peerID, Status: "NOT_CONFIGURED"}},
		}, nil
	}
	err = verifySetupCluster(context.Background(), statePath, state, Options{Out: os.Stderr, CheckCluster: checker})
	if err == nil || !strings.Contains(err.Error(), "retry with dfs setup --resume") {
		t.Fatalf("incomplete cluster verification error = %v", err)
	}
	persisted, err := load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != PhaseMounted || len(persisted.Acknowledgements) != 2 || persisted.Acknowledgements[1].Status != "INCOMPLETE" {
		t.Fatalf("persisted resumable cluster state = %+v", persisted)
	}
}

func TestSetupPersistsApprovalAndIdentityForResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	encoded, err := (peer.Invitation{Version: peer.ProtocolVersion, FileSystemID: strings.Repeat("a", 40),
		InvitationID: "invite", Secret: "secret", CertificateSHA256: strings.Repeat("b", 64)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath := filepath.Join(home, "repository")
	mountpoint := filepath.Join(home, "mount")
	refused := errors.New("not approved")
	var firstPeerID string
	_, err = Run(context.Background(), Options{Invitation: encoded, Repository: repositoryPath, Mountpoint: mountpoint,
		CacheLimit: 1, Timeout: time.Second, Approve: func(state *State) error {
			firstPeerID = state.PeerID
			return refused
		}})
	if !errors.Is(err, refused) {
		t.Fatalf("initial setup error = %v", err)
	}
	path, err := StatePath(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("setup state mode = %o", info.Mode().Perm())
	}
	var resumedPeerID string
	_, err = Run(context.Background(), Options{Repository: repositoryPath, Mountpoint: mountpoint, Resume: true,
		Approve: func(state *State) error {
			resumedPeerID = state.PeerID
			return refused
		}})
	if !errors.Is(err, refused) {
		t.Fatalf("resumed setup error = %v", err)
	}
	if firstPeerID == "" || resumedPeerID != firstPeerID {
		t.Fatalf("pairing identity changed across resume: %q != %q", resumedPeerID, firstPeerID)
	}
}

func TestSetupStateAndPairingDirectoriesAreRepositoryScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	first, err := StatePath(filepath.Join(home, "first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := StatePath(filepath.Join(home, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("repository-scoped setup paths collide: %s", first)
	}
	if pairingPath(first) == pairingPath(second) {
		t.Fatalf("repository-scoped pairing paths collide: %s", pairingPath(first))
	}
}

func TestChoosePairingPortRejectsInvalidValues(t *testing.T) {
	for _, port := range []int{-1, 65536} {
		if _, err := choosePairingPort(port); err == nil {
			t.Fatalf("choosePairingPort(%d) unexpectedly succeeded", port)
		}
	}
}
