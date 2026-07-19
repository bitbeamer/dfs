package mount

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthReportRoundTripAndCheck(t *testing.T) {
	repository := t.TempDir()
	mountpoint := t.TempDir()
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	report := HealthReport{
		Version: healthVersion, State: "ready", Healthy: true, PID: os.Getpid(),
		Hostname: hostname, Peer: "test", Repository: repository, Mountpoint: mountpoint,
		StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := writeHealth(HealthPath(repository), report); err != nil {
		t.Fatal(err)
	}
	checked, err := CheckHealth(repository)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Peer != "test" || checked.Mountpoint != mountpoint {
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
	report := HealthReport{
		Version: healthVersion, State: "stopped", PID: os.Getpid(),
		Repository: repository, Mountpoint: t.TempDir(), UpdatedAt: time.Now().UTC(),
	}
	if err := writeHealth(HealthPath(repository), report); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckHealth(repository); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped health error = %v", err)
	}

	report.State = "ready"
	report.Healthy = true
	report.UpdatedAt = time.Now().Add(-healthMaxAge - time.Second)
	if err := writeHealth(HealthPath(repository), report); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckHealth(repository); err == nil || !strings.Contains(err.Error(), "heartbeat is stale") {
		t.Fatalf("stale health error = %v", err)
	}
}

func TestReadHealthExplainsMissingReport(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if _, err := ReadHealth(repository); err == nil || !strings.Contains(err.Error(), "has not started") {
		t.Fatalf("missing health error = %v", err)
	}
}

func TestNotifySystemdSendsDatagram(t *testing.T) {
	temporary, err := os.CreateTemp("/tmp", "dfs-notify-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	socketPath := temporary.Name()
	if err := temporary.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	address := &net.UnixAddr{Name: socketPath, Net: "unixgram"}
	listener, err := net.ListenUnixgram("unixgram", address)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("NOTIFY_SOCKET", socketPath)
	notifySystemd("READY=1\nSTATUS=mounted")
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 128)
	count, _, err := listener.ReadFromUnix(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:count]); got != "READY=1\nSTATUS=mounted" {
		t.Fatalf("notification = %q", got)
	}
}
