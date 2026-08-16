package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
)

func TestCoreRunsWithoutMountAndRejectsDuplicateInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Core Test\nemail = core@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := repository.Init(context.Background(), filepath.Join(home, "repository"), "core", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	second, err := repository.Open(repo.Config.Repository)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := packet.LocalAddr().(*net.UDPAddr).Port
	_ = packet.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() {
		done <- Run(repo, Options{Context: ctx, Logger: logger, PairingPort: port, DisableDiscovery: true})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if report, err := CheckHealth(repo.Config.Repository); err == nil && report.Mode == "core" && report.Mountpoint == "" {
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
	duplicateErr := Run(second, Options{Context: context.Background(), Logger: logger, PairingPort: port + 1, DisableDiscovery: true})
	if duplicateErr == nil || !strings.Contains(duplicateErr.Error(), "already running") {
		t.Fatalf("duplicate core error = %v", duplicateErr)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("core shutdown exceeded its bound")
	}
	report, err := ReadHealth(repo.Config.Repository)
	if err != nil || report.State != "stopped" {
		t.Fatalf("stopped core health = %+v, %v", report, err)
	}
}

func TestCoreInstancesAreRepositoryScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[user]\nname = Multi Test\nemail = multi@example.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	type running struct {
		repo   *repository.Repository
		cancel context.CancelFunc
		done   chan error
	}
	var instances []running
	for index := 0; index < 2; index++ {
		repo, err := repository.Init(context.Background(), filepath.Join(home, fmt.Sprintf("repository-%d", index)), fmt.Sprintf("peer-%d", index), int64(index+1)<<20)
		if err != nil {
			t.Fatal(err)
		}
		packet, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := packet.LocalAddr().(*net.UDPAddr).Port
		_ = packet.Close()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- Run(repo, Options{Context: ctx, Logger: logger, PairingPort: port, DisableDiscovery: true})
		}()
		instances = append(instances, running{repo: repo, cancel: cancel, done: done})
	}
	for _, instance := range instances {
		deadline := time.Now().Add(5 * time.Second)
		for {
			report, err := CheckHealth(instance.repo.Config.Repository)
			if err == nil && report.Repository == instance.repo.Config.Repository {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("instance %s did not become healthy: %v", instance.repo.Config.Name, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if HealthPath(instances[0].repo.Config.Repository) == HealthPath(instances[1].repo.Config.Repository) {
		t.Fatal("instance health paths collide")
	}
	for _, instance := range instances {
		instance.cancel()
		if err := <-instance.done; err != nil {
			t.Error(err)
		}
		_ = instance.repo.Close()
	}
}
