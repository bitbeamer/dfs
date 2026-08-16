package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/peer"
	"github.com/bitbeamer/dfs/internal/repository"
)

const (
	healthVersion   = 2
	healthHeartbeat = 30 * time.Second
	healthMaxAge    = 2 * time.Minute
)

type HealthReport struct {
	Version          int                    `json:"version"`
	Mode             string                 `json:"mode"`
	State            string                 `json:"state"`
	Healthy          bool                   `json:"healthy"`
	PID              int                    `json:"pid"`
	Hostname         string                 `json:"hostname"`
	Peer             string                 `json:"peer"`
	Repository       string                 `json:"repository"`
	Mountpoint       string                 `json:"mountpoint,omitempty"`
	StartedAt        time.Time              `json:"started_at"`
	UpdatedAt        time.Time              `json:"updated_at"`
	Error            string                 `json:"error,omitempty"`
	Operational      *peer.DiagnosticReport `json:"operational,omitempty"`
	OperationalError string                 `json:"operational_error,omitempty"`
}

type healthReporter struct {
	mu     sync.Mutex
	path   string
	logger *slog.Logger
	report HealthReport
}

func HealthPath(repository string) string {
	return filepath.Join(repository, filepath.FromSlash(config.Directory), "health.json")
}

func newHealthReporter(peerName, repository, mountpoint string, logger *slog.Logger) *healthReporter {
	hostname, _ := os.Hostname()
	now := time.Now().UTC()
	return &healthReporter{path: HealthPath(repository), logger: logger.With("component", "health"), report: HealthReport{
		Version: healthVersion, Mode: "core", State: "starting", PID: os.Getpid(), Hostname: hostname,
		Peer: peerName, Repository: repository, Mountpoint: mountpoint, StartedAt: now, UpdatedAt: now,
	}}
}

func (r *healthReporter) update(state string, healthy bool, err error) {
	r.mu.Lock()
	r.report.State, r.report.Healthy, r.report.UpdatedAt, r.report.Error = state, healthy, time.Now().UTC(), ""
	if err != nil {
		r.report.Error = err.Error()
	}
	report := r.report
	r.mu.Unlock()
	if writeErr := writeHealth(r.path, report); writeErr != nil {
		r.logger.Warn("health report update failed", "state", state, "error", writeErr)
	}
}

func (r *healthReporter) heartbeat(stop <-chan struct{}) {
	ticker := time.NewTicker(healthHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.update("ready", true, nil)
			notifySystemd("WATCHDOG=1")
		case <-stop:
			return
		}
	}
}

func (r *healthReporter) observe(stop <-chan struct{}, repo *repository.Repository, interval time.Duration) {
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			report, err := peer.Diagnose(ctx, repo, 10*time.Second)
			cancel()
			r.mu.Lock()
			if err != nil {
				r.report.OperationalError = err.Error()
			} else {
				r.report.Operational, r.report.OperationalError = &report, ""
			}
			snapshot := r.report
			r.mu.Unlock()
			if writeErr := writeHealth(r.path, snapshot); writeErr != nil {
				r.logger.Warn("operational health update failed", "error", writeErr)
			}
			timer.Reset(interval)
		case <-stop:
			return
		}
	}
}

func writeHealth(path string, report HealthReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".health-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func ReadHealth(repository string) (HealthReport, error) {
	data, err := os.ReadFile(HealthPath(repository))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return HealthReport{}, errors.New("no core health report; the daemon has not started")
		}
		return HealthReport{}, err
	}
	var report HealthReport
	if err := json.Unmarshal(data, &report); err != nil {
		return report, fmt.Errorf("decode core health report: %w", err)
	}
	if report.Version != healthVersion {
		return report, fmt.Errorf("unsupported core health report version %d", report.Version)
	}
	return report, nil
}

func CheckHealth(repository string) (HealthReport, error) {
	report, err := ReadHealth(repository)
	if err != nil {
		return report, err
	}
	if report.Mode != "core" {
		return report, fmt.Errorf("health report is not owned by the independent core daemon")
	}
	if report.State != "ready" || !report.Healthy {
		if report.Error != "" {
			return report, fmt.Errorf("core state is %s: %s", report.State, report.Error)
		}
		return report, fmt.Errorf("core state is %s", report.State)
	}
	if time.Since(report.UpdatedAt) > healthMaxAge {
		return report, fmt.Errorf("core heartbeat is stale (last update %s)", report.UpdatedAt.Format(time.RFC3339))
	}
	hostname, _ := os.Hostname()
	if report.Hostname == "" || report.Hostname != hostname {
		return report, fmt.Errorf("core health belongs to host %q", report.Hostname)
	}
	if report.PID <= 0 || !processAlive(report.PID) {
		return report, fmt.Errorf("core process %d is not running", report.PID)
	}
	return report, nil
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func notifySystemd(message string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + strings.TrimPrefix(socket, "@")
	}
	connection, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return
	}
	defer connection.Close()
	_, _ = connection.Write([]byte(message))
}
