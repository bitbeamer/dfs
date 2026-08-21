package managed

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
)

const (
	contentLivenessInterval = 2 * time.Second
	contentLivenessProbe    = 750 * time.Millisecond
	contentLivenessMaxAge   = 5 * time.Second
	contentLivenessLocalIO  = 25 * time.Millisecond
	contentPeerRefresh      = 30 * time.Second
)

type contentLivenessSnapshot struct {
	Version    int             `json:"version"`
	ObservedAt time.Time       `json:"observed_at"`
	Complete   bool            `json:"complete"`
	Peers      map[string]bool `json:"peers"`
}

// ContentLivenessMonitor keeps a lightweight, authenticated view of which
// accepted peers are reachable. It is owned by the core daemon and exposed to
// local frontend processes over a repository-specific Unix socket.
type ContentLivenessMonitor struct {
	repo      *repository.Repository
	listener  *net.UnixListener
	path      string
	interval  time.Duration
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	mu        sync.RWMutex
	snapshot  contentLivenessSnapshot
	peerIDs   []string
	refreshed time.Time
}

func contentLivenessPath(repositoryPath string) string {
	absolute, err := filepath.Abs(repositoryPath)
	if err != nil {
		absolute = repositoryPath
	}
	digest := sha256.Sum256([]byte(filepath.Clean(absolute)))
	return filepath.Join("/tmp", fmt.Sprintf("dfs-%d-%x-content.sock", os.Getuid(), digest[:8]))
}

func StartContentLivenessMonitor(parent context.Context, repo *repository.Repository) (*ContentLivenessMonitor, error) {
	path := contentLivenessPath(repo.Config.Repository)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("DFS content liveness endpoint %s is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	monitor := &ContentLivenessMonitor{repo: repo, listener: listener, path: path,
		interval: contentLivenessInterval, ctx: ctx, cancel: cancel,
		snapshot: contentLivenessSnapshot{Version: 1, Peers: make(map[string]bool)}}
	monitor.wg.Add(2)
	go monitor.serve()
	go monitor.observe()
	return monitor, nil
}

func (monitor *ContentLivenessMonitor) Close() error {
	monitor.cancel()
	err := monitor.listener.Close()
	monitor.wg.Wait()
	removeErr := os.Remove(monitor.path)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

func (monitor *ContentLivenessMonitor) serve() {
	defer monitor.wg.Done()
	for {
		connection, err := monitor.listener.AcceptUnix()
		if err != nil {
			return
		}
		monitor.mu.RLock()
		snapshot := monitor.snapshot
		snapshot.Peers = clonePeerLiveness(snapshot.Peers)
		monitor.mu.RUnlock()
		_ = connection.SetWriteDeadline(time.Now().Add(contentLivenessLocalIO))
		_ = json.NewEncoder(connection).Encode(snapshot)
		_ = connection.Close()
	}
}

func (monitor *ContentLivenessMonitor) observe() {
	defer monitor.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-monitor.ctx.Done():
			return
		case <-timer.C:
			monitor.probeOnce()
			timer.Reset(monitor.interval)
		}
	}
}

func (monitor *ContentLivenessMonitor) probeOnce() {
	if len(monitor.peerIDs) == 0 || time.Since(monitor.refreshed) >= contentPeerRefresh {
		ctx, cancel := context.WithTimeout(monitor.ctx, contentLivenessProbe)
		peerIDs, err := optimizedPeerIDs(ctx, monitor.repo, "interactive")
		cancel()
		if err == nil {
			monitor.peerIDs = peerIDs
			monitor.refreshed = time.Now()
		}
	}
	peerIDs := append([]string(nil), monitor.peerIDs...)
	states := make(map[string]bool, len(peerIDs))
	var statesMu sync.Mutex
	var probes sync.WaitGroup
	for _, peerID := range peerIDs {
		peerID := peerID
		probes.Add(1)
		go func() {
			defer probes.Done()
			ctx, cancel := context.WithTimeout(monitor.ctx, contentLivenessProbe)
			stream, _, response, err := openContentStream(ctx, monitor.repo, peerID, Request{Operation: "ping"})
			cancel()
			if stream != nil {
				_ = stream.Close()
			}
			if err == nil && response.AnnexUUID != "" {
				persistCtx, persistCancel := context.WithTimeout(monitor.ctx, time.Second)
				_ = monitor.repo.RecordPeerAnnexUUID(persistCtx, peerID, response.AnnexUUID)
				persistCancel()
			}
			statesMu.Lock()
			states[peerID] = err == nil
			statesMu.Unlock()
		}()
	}
	probes.Wait()
	if monitor.ctx.Err() != nil {
		return
	}
	monitor.mu.Lock()
	monitor.snapshot = contentLivenessSnapshot{Version: 1, ObservedAt: time.Now().UTC(),
		Complete: !monitor.refreshed.IsZero(), Peers: states}
	monitor.mu.Unlock()
}

func clonePeerLiveness(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for peerID, online := range source {
		result[peerID] = online
	}
	return result
}

// liveContentPeerIDs returns a complete current subset only when the local
// daemon has observed every requested accepted peer recently. Otherwise the
// caller must retain its bounded synchronous discovery path.
func liveContentPeerIDs(repositoryPath string, peerIDs []string) ([]string, bool) {
	connection, err := net.DialTimeout("unix", contentLivenessPath(repositoryPath), contentLivenessLocalIO)
	if err != nil {
		return nil, false
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(contentLivenessLocalIO))
	var snapshot contentLivenessSnapshot
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&snapshot); err != nil || snapshot.Version != 1 ||
		!snapshot.Complete || snapshot.ObservedAt.IsZero() || time.Since(snapshot.ObservedAt) > contentLivenessMaxAge {
		return nil, false
	}
	online := make([]string, 0, len(peerIDs))
	for _, peerID := range peerIDs {
		reachable, found := snapshot.Peers[peerID]
		if !found {
			return nil, false
		}
		if reachable {
			online = append(online, peerID)
		}
	}
	sort.Strings(online)
	return orderedIntersection(peerIDs, online), true
}
