package wakeup

import (
	"os"
	"testing"
	"time"
)

func TestRepositoryEventRoundTrip(t *testing.T) {
	repository := t.TempDir()
	listener, err := Listen(repository)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if info, err := os.Stat(Path(repository)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("event socket = %v, %v", info, err)
	}
	if err := Notify(repository, "managed Git receive"); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	go func() {
		reason, _ := listener.Receive()
		received <- reason
	}()
	select {
	case reason := <-received:
		if reason != "managed Git receive" {
			t.Fatalf("event = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("repository event was not delivered")
	}
}
