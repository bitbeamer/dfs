package mount

import (
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
)

const (
	healthVersion     = 1
	healthHeartbeat   = 30 * time.Second
	healthMaxAge      = 2 * time.Minute
	healthStatTimeout = 3 * time.Second
)

type HealthReport struct {
	Version    int       `json:"version"`
	State      string    `json:"state"`
	Healthy    bool      `json:"healthy"`
	PID        int       `json:"pid"`
	Hostname   string    `json:"hostname"`
	Peer       string    `json:"peer"`
	Repository string    `json:"repository"`
	Mountpoint string    `json:"mountpoint"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Error      string    `json:"error,omitempty"`
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

func newHealthReporter(peer, repository, mountpoint string, logger *slog.Logger) *healthReporter {
	hostname, _ := os.Hostname()
	now := time.Now().UTC()
	return &healthReporter{
		path: HealthPath(repository), logger: logger.With("component", "health"),
		report: HealthReport{
			Version: healthVersion, State: "starting", PID: os.Getpid(), Hostname: hostname,
			Peer: peer, Repository: repository, Mountpoint: mountpoint, StartedAt: now, UpdatedAt: now,
		},
	}
}

func (r *healthReporter) update(state string, healthy bool, err error) {
	r.mu.Lock()
	r.report.State = state
	r.report.Healthy = healthy
	r.report.UpdatedAt = time.Now().UTC()
	r.report.Error = ""
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

func writeHealth(path string, report HealthReport) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".health-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return syncDirectory(directory)
}

func ReadHealth(repository string) (HealthReport, error) {
	data, err := os.ReadFile(HealthPath(repository))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return HealthReport{}, errors.New("no mount health report; the service has not started")
		}
		return HealthReport{}, err
	}
	var report HealthReport
	if err := json.Unmarshal(data, &report); err != nil {
		return HealthReport{}, fmt.Errorf("decode mount health report: %w", err)
	}
	if report.Version != healthVersion {
		return report, fmt.Errorf("unsupported mount health report version %d", report.Version)
	}
	return report, nil
}

func CheckHealth(repository string) (HealthReport, error) {
	report, err := ReadHealth(repository)
	if err != nil {
		return report, err
	}
	if report.State != "ready" || !report.Healthy {
		if report.Error != "" {
			return report, fmt.Errorf("mount state is %s: %s", report.State, report.Error)
		}
		return report, fmt.Errorf("mount state is %s", report.State)
	}
	if time.Since(report.UpdatedAt) > healthMaxAge {
		return report, fmt.Errorf("mount heartbeat is stale (last update %s)", report.UpdatedAt.Format(time.RFC3339))
	}
	hostname, _ := os.Hostname()
	if report.Hostname == "" || report.Hostname != hostname {
		return report, fmt.Errorf("mount health belongs to host %q", report.Hostname)
	}
	if report.PID <= 0 || !processAlive(report.PID) {
		return report, fmt.Errorf("mount process %d is not running", report.PID)
	}
	statResult := make(chan error, 1)
	go func() {
		_, err := os.Stat(report.Mountpoint)
		statResult <- err
	}()
	select {
	case err := <-statResult:
		if err != nil {
			return report, fmt.Errorf("mountpoint %s is not accessible: %w", report.Mountpoint, err)
		}
	case <-time.After(healthStatTimeout):
		return report, fmt.Errorf("mountpoint %s did not respond within %s", report.Mountpoint, healthStatTimeout)
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
