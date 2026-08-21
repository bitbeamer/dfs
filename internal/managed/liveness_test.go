package managed

import (
	"encoding/json"
	"net"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestLiveContentPeerIDsUsesCompleteFreshLocalSnapshot(t *testing.T) {
	repositoryPath := t.TempDir()
	path := contentLivenessPath(repositoryPath)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := range 2 {
			connection, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return
			}
			var request contentLivenessRequest
			_ = json.NewDecoder(connection).Decode(&request)
			snapshot := contentLivenessSnapshot{Version: 1, ObservedAt: time.Now().UTC(), Complete: index == 0,
				PeerIDs: []string{"offline", "online"}, Peers: map[string]bool{"offline": false, "online": true},
				HoldersComplete: true, HolderPeerIDs: []string{"offline"}}
			_ = json.NewEncoder(connection).Encode(snapshot)
			_ = connection.Close()
		}
	}()

	if _, got, holders, known, holdersKnown := localContentPlan(repositoryPath, "key"); !known || !holdersKnown ||
		!reflect.DeepEqual(got, []string{"online"}) || !reflect.DeepEqual(holders, []string{"offline"}) {
		t.Fatalf("local plan = online %v, holders %v, known %v/%v", got, holders, known, holdersKnown)
	}
	if got, online, holders, known, holdersKnown := localContentPlan(repositoryPath, "key"); known || holdersKnown || got != nil || online != nil || holders != nil {
		t.Fatalf("incomplete snapshot = %v, %v, %v, %v, %v", got, online, holders, known, holdersKnown)
	}
	<-done
}
