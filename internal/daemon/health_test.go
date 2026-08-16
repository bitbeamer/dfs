package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/peer"
)

func TestHealthReportRoundTripAndCheck(t *testing.T) {
	repository := t.TempDir()
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	report := HealthReport{Version: healthVersion, Mode: "core", State: "ready", Healthy: true, PID: os.Getpid(),
		Hostname: hostname, Peer: "test", Repository: repository, Mountpoint: "/configured/mount",
		StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Operational: &peer.DiagnosticReport{Version: 2, PeerName: "test", ReconciliationStatus: "ready"}}
	if err := writeHealth(HealthPath(repository), report); err != nil {
		t.Fatal(err)
	}
	checked, err := CheckHealth(repository)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Peer != "test" || checked.Operational == nil {
		t.Fatalf("checked health = %+v", checked)
	}
	info, err := os.Stat(HealthPath(repository))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("health mode = %o, want 600", info.Mode().Perm())
	}
}

func TestHealthCheckRejectsStoppedAndStaleReports(t *testing.T) {
	repository := t.TempDir()
	report := HealthReport{Version: healthVersion, Mode: "core", State: "stopped", PID: os.Getpid(), Repository: repository, UpdatedAt: time.Now().UTC()}
	if err := writeHealth(HealthPath(repository), report); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckHealth(repository); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped health error = %v", err)
	}
	report.State, report.Healthy, report.UpdatedAt = "ready", true, time.Now().Add(-healthMaxAge-time.Second)
	if err := writeHealth(HealthPath(repository), report); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckHealth(repository); err == nil || !strings.Contains(err.Error(), "heartbeat is stale") {
		t.Fatalf("stale health error = %v", err)
	}
}

func TestReadHealthExplainsMissingReport(t *testing.T) {
	if _, err := ReadHealth(filepath.Join(t.TempDir(), "repository")); err == nil || !strings.Contains(err.Error(), "has not started") {
		t.Fatalf("missing health error = %v", err)
	}
}

func TestNotifySystemdSendsDatagram(t *testing.T) {
	temporary, err := os.CreateTemp("/tmp", "dfs-notify-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := temporary.Name()
	_ = temporary.Close()
	_ = os.Remove(socketPath)
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", socketPath)
	notifySystemd("READY=1\nSTATUS=core")
	_ = listener.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 128)
	count, _, err := listener.ReadFromUnix(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:count]); got != "READY=1\nSTATUS=core" {
		t.Fatalf("notification = %q", got)
	}
}
