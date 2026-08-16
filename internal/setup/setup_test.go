package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/peer"
)

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
