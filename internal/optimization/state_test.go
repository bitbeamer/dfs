package optimization

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestSaveLoadAndStableOrdering(t *testing.T) {
	repositoryPath := t.TempDir()
	state := State{PeerID: "local", OptimizedAt: time.Now().UTC(), MembershipFingerprint: "fingerprint",
		Interactive: []RankedSource{{PeerID: "fast", PeerName: "Fast", Status: "MEASURED"}, {PeerID: "offline", PeerName: "Offline", Status: "OFFLINE"}},
		Bulk:        []RankedSource{{PeerID: "offline", PeerName: "Offline", Status: "OFFLINE"}, {PeerID: "fast", PeerName: "Fast", Status: "MEASURED"}}}
	if err := Save(repositoryPath, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Version != Version || loaded.PeerID != state.PeerID {
		t.Fatalf("loaded state = %#v", loaded)
	}
	info, err := os.Stat(Path(repositoryPath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %v", info.Mode().Perm())
	}
	members := []Member{{PeerID: "local"}, {PeerID: "new"}, {PeerID: "offline"}, {PeerID: "fast"}}
	if got, want := OrderedPeerIDs(loaded, "interactive", members, "local"), []string{"fast", "offline", "new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("interactive order = %v, want %v", got, want)
	}
	if got, want := OrderedPeerIDs(loaded, "bulk", members, "local"), []string{"offline", "fast", "new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bulk order = %v, want %v", got, want)
	}
}

func TestLoadMissingState(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load error = %v, want not-exist", err)
	}
}

func TestSaveReplacesPriorRun(t *testing.T) {
	repositoryPath := t.TempDir()
	first := State{PeerID: "local", OptimizedAt: time.Now().Add(-time.Hour).UTC(), Measurements: []Measurement{{PeerID: "old"}}}
	second := State{PeerID: "local", OptimizedAt: time.Now().UTC(), Measurements: []Measurement{{PeerID: "new"}}}
	if err := Save(repositoryPath, first); err != nil {
		t.Fatal(err)
	}
	if err := Save(repositoryPath, second); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Measurements) != 1 || loaded.Measurements[0].PeerID != "new" {
		t.Fatalf("replacement state = %#v", loaded)
	}
}

func TestLoadCurrentRejectsAnotherPeersState(t *testing.T) {
	repositoryPath := t.TempDir()
	if err := Save(repositoryPath, State{PeerID: "other", OptimizedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCurrent(repositoryPath, "filesystem", "local"); err == nil {
		t.Fatal("accepted another peer's optimization state")
	}
}

func TestFingerprintIsOrderIndependentAndEndpointSensitive(t *testing.T) {
	left := []Member{{PeerID: "a", Endpoint: "quic://a:1"}, {PeerID: "b", Endpoint: "quic://b:1"}}
	right := []Member{left[1], left[0]}
	if Fingerprint(left) != Fingerprint(right) {
		t.Fatal("membership fingerprint depends on record order")
	}
	right[0].Endpoint = "quic://b:2"
	if Fingerprint(left) == Fingerprint(right) {
		t.Fatal("membership fingerprint ignores endpoint changes")
	}
}
