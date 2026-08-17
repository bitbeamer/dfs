package optimization

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/membership"
)

const Version = 1

const FileName = "optimization.json"

type Measurement struct {
	PeerID                string  `json:"peer_id"`
	PeerName              string  `json:"peer_name"`
	Status                string  `json:"status"`
	Samples               int     `json:"samples"`
	Failures              int     `json:"failures"`
	LatencyMedianMS       float64 `json:"latency_median_ms,omitempty"`
	LatencyP95MS          float64 `json:"latency_p95_ms,omitempty"`
	TTFBMedianMS          float64 `json:"ttfb_median_ms,omitempty"`
	TTFBP95MS             float64 `json:"ttfb_p95_ms,omitempty"`
	InteractiveMedianMbps float64 `json:"interactive_median_mbps,omitempty"`
	InteractiveP10Mbps    float64 `json:"interactive_p10_mbps,omitempty"`
	BulkMedianMbps        float64 `json:"bulk_median_mbps,omitempty"`
	BulkP10Mbps           float64 `json:"bulk_p10_mbps,omitempty"`
	LastError             string  `json:"last_error,omitempty"`
}

type RankedSource struct {
	PeerID   string `json:"peer_id"`
	PeerName string `json:"peer_name"`
	Status   string `json:"status"`
}

type State struct {
	Version               int            `json:"version"`
	PeerID                string         `json:"peer_id"`
	OptimizedAt           time.Time      `json:"optimized_at"`
	MembershipFingerprint string         `json:"membership_fingerprint"`
	Stale                 bool           `json:"stale,omitempty"`
	Measurements          []Measurement  `json:"measurements"`
	Interactive           []RankedSource `json:"interactive"`
	Bulk                  []RankedSource `json:"bulk"`
}

type Member struct {
	PeerID   string
	PeerName string
	Endpoint string
}

func Path(repositoryPath string) string {
	return filepath.Join(repositoryPath, filepath.FromSlash(config.Directory), FileName)
}

func Load(repositoryPath string) (State, error) {
	data, err := os.ReadFile(Path(repositoryPath))
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode DFS optimization state: %w", err)
	}
	if state.Version != Version || state.PeerID == "" || state.OptimizedAt.IsZero() {
		return State{}, errors.New("invalid DFS optimization state")
	}
	return state, nil
}

func Save(repositoryPath string, state State) error {
	state.Version = Version
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory := filepath.Dir(Path(repositoryPath))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".optimization-*")
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
	if err := os.Rename(temporaryPath, Path(repositoryPath)); err != nil {
		return err
	}
	opened, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.Sync()
}

func CurrentMembers(repositoryPath, filesystemID, localPeerID string) ([]Member, error) {
	records, err := membership.Accepted(repositoryPath, filesystemID, localPeerID)
	if err != nil {
		return nil, err
	}
	members := make([]Member, 0, len(records))
	for _, record := range records {
		members = append(members, Member{PeerID: record.Payload.PeerID, PeerName: record.Payload.Name, Endpoint: record.Payload.QUICEndpoint})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PeerID < members[j].PeerID })
	return members, nil
}

func Fingerprint(members []Member) string {
	values := make([]string, 0, len(members))
	for _, member := range members {
		values = append(values, member.PeerID+"\x00"+member.Endpoint)
	}
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])
}

func LoadCurrent(repositoryPath, filesystemID, localPeerID string) (State, error) {
	state, err := Load(repositoryPath)
	if err != nil {
		return State{}, err
	}
	members, err := CurrentMembers(repositoryPath, filesystemID, localPeerID)
	if err != nil {
		return State{}, err
	}
	state.Stale = state.MembershipFingerprint != Fingerprint(members)
	return state, nil
}

func OrderedPeerIDs(state State, profile string, members []Member, localPeerID string) []string {
	ranking := state.Interactive
	if profile == "bulk" {
		ranking = state.Bulk
	}
	current := make(map[string]bool, len(members))
	for _, member := range members {
		if member.PeerID != localPeerID {
			current[member.PeerID] = true
		}
	}
	seen := make(map[string]bool, len(current))
	ordered := make([]string, 0, len(current))
	for _, source := range ranking {
		if current[source.PeerID] && !seen[source.PeerID] {
			seen[source.PeerID] = true
			ordered = append(ordered, source.PeerID)
		}
	}
	var missing []string
	for peerID := range current {
		if !seen[peerID] {
			missing = append(missing, peerID)
		}
	}
	sort.Strings(missing)
	return append(ordered, missing...)
}
