package cli

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestRawMountSignalDelivery(t *testing.T) {
	if os.Getenv("DFS_SIGNAL_HELPER") == "1" {
		signals := make(chan os.Signal, 2)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case received := <-signals:
			if received != syscall.SIGTERM {
				t.Fatalf("received signal %v, want SIGTERM", received)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("raw signal channel did not receive SIGTERM")
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestRawMountSignalDelivery$")
	command.Env = append(os.Environ(), "DFS_SIGNAL_HELPER=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("signal helper failed: %s: %v", output, err)
	}
}
