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
	snapshot := contentLivenessSnapshot{Version: 1, ObservedAt: time.Now().UTC(), Complete: true,
		Peers: map[string]bool{"offline": false, "online": true}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 2 {
			connection, acceptErr := listener.AcceptUnix()
			if acceptErr != nil {
				return
			}
			_ = json.NewEncoder(connection).Encode(snapshot)
			_ = connection.Close()
		}
	}()

	if got, known := liveContentPeerIDs(repositoryPath, []string{"offline", "online"}); !known || !reflect.DeepEqual(got, []string{"online"}) {
		t.Fatalf("live peers = %v, %v, want [online], true", got, known)
	}
	if got, known := liveContentPeerIDs(repositoryPath, []string{"online", "new-member"}); known || got != nil {
		t.Fatalf("incomplete membership snapshot = %v, %v, want nil, false", got, known)
	}
	<-done
}
