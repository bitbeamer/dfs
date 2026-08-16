package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	dfscore "github.com/bitbeamer/dfs/internal/daemon"
	"github.com/bitbeamer/dfs/internal/repository"
)

func startTestCore(t *testing.T, repositoryPath, mountpoint string) {
	t.Helper()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := packet.LocalAddr().(*net.UDPAddr).Port
	_ = packet.Close()
	repo, err := repository.Open(repositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- dfscore.Run(repo, dfscore.Options{Context: ctx, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			PairingPort: port, Mountpoint: mountpoint, DisableDiscovery: true})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := dfscore.CheckHealth(repositoryPath); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("core startup: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("core did not become healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("core shutdown: %v", err)
			}
		case <-time.After(25 * time.Second):
			t.Error("core did not stop")
		}
		_ = repo.Close()
	})
}
