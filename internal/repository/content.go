package repository

import (
	"bufio"
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ContentFile describes one logical namespace file and where its content is
// recorded as available.
type ContentFile struct {
	Path    string   `json:"path"`
	Size    int64    `json:"size_bytes"`
	Key     string   `json:"key,omitempty"`
	Git     bool     `json:"git,omitempty"`     // content lives in Git metadata; present on every synced peer
	Local   bool     `json:"local"`             // content is present in this repository
	Peers   []string `json:"peers,omitempty"`   // accepted peer IDs recorded as holders
	Storage []string `json:"storage,omitempty"` // durable storage names recorded as holders
}

// ContentFiles lists the logical namespace with per-file content residency.
// With peerIDs or storages set, holders are resolved from the synced git-annex
// location log; that metadata is advisory and can lag actual peer content
// until the next metadata sync. Pass nil for both to list local residency only.
func (r *Repository) ContentFiles(ctx context.Context, peerIDs []string, storages []StorageRemote) ([]ContentFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	output, err := r.runner.Run(ctx, "git", "annex", "find", "--anything", "--json")
	if err != nil {
		return nil, err
	}
	annexed, err := parseContentFind(output)
	if err != nil {
		return nil, err
	}
	localOutput, err := r.runner.Run(ctx, "git", "annex", "find", "--in=here", "--json")
	if err != nil {
		return nil, err
	}
	localFiles, err := parseContentFind(localOutput)
	if err != nil {
		return nil, err
	}
	local := make(map[string]bool, len(localFiles))
	for _, file := range localFiles {
		local[file.Path] = true
	}
	var keyHolders map[string]contentHolders
	if len(peerIDs) > 0 || len(storages) > 0 {
		keyHolders, err = r.contentHoldersLocked(ctx, peerIDs, storages)
		if err != nil {
			return nil, err
		}
	}
	annexedPaths := make(map[string]bool, len(annexed))
	files := make([]ContentFile, 0, len(annexed))
	for _, record := range annexed {
		annexedPaths[record.Path] = true
		file := ContentFile{Path: record.Path, Size: record.Size, Key: record.Key, Local: local[record.Path]}
		if holders, found := keyHolders[record.Key]; found {
			file.Peers = holders.peers
			file.Storage = holders.storage
		}
		files = append(files, file)
	}
	tree, err := r.runner.Run(ctx, "git", "ls-tree", "-r", "-z", "--format=%(objecttype)%x09%(objectsize)%x09%(path)", "HEAD")
	if err != nil {
		return nil, err
	}
	for _, entry := range strings.Split(tree, "\x00") {
		if entry == "" {
			continue
		}
		fields := strings.SplitN(entry, "\t", 3)
		if len(fields) != 3 || fields[0] != "blob" {
			continue
		}
		path := filepath.ToSlash(fields[2])
		if annexedPaths[path] {
			continue
		}
		size, _ := strconv.ParseInt(fields[1], 10, 64)
		files = append(files, ContentFile{Path: path, Size: size, Git: true, Local: true})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

type contentFindRecord struct {
	Path string
	Key  string
	Size int64
}

func parseContentFind(output string) ([]contentFindRecord, error) {
	type record struct {
		File     string `json:"file"`
		Key      string `json:"key"`
		ByteSize string `json:"bytesize"`
	}
	var result []contentFindRecord
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var value record
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(value.ByteSize, 10, 64)
		if err != nil {
			return nil, err
		}
		result = append(result, contentFindRecord{Path: filepath.ToSlash(value.File), Key: value.Key, Size: size})
	}
	return result, scanner.Err()
}

type contentHolders struct {
	peers   []string
	storage []string
}

// contentHoldersLocked maps annex keys to the accepted peers and durable
// storages whose UUIDs are recorded as holders in the local location log.
func (r *Repository) contentHoldersLocked(ctx context.Context, peerIDs []string, storages []StorageRemote) (map[string]contentHolders, error) {
	uuidPeers := make(map[string]string, len(peerIDs))
	for _, peerID := range peerIDs {
		shortID := peerID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		value, err := r.runner.Run(ctx, "git", "config", "--get", "remote.dfs-peer-"+shortID+".annex-uuid")
		if err == nil && strings.TrimSpace(value) != "" {
			uuidPeers[strings.TrimSpace(value)] = peerID
		}
	}
	uuidStorage := make(map[string]string, len(storages))
	for _, storage := range storages {
		if storage.UUID != "" {
			uuidStorage[storage.UUID] = storage.Name
		}
	}
	output, commandErr := r.runner.Run(ctx, "git", "annex", "whereis", "--all", "--json")
	if commandErr != nil && strings.TrimSpace(output) == "" {
		return nil, commandErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type location struct {
		UUID string `json:"uuid"`
	}
	type record struct {
		Key     string     `json:"key"`
		Whereis []location `json:"whereis"`
	}
	holders := make(map[string]contentHolders)
	decoded := 0
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	for scanner.Scan() {
		var value record
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return nil, err
		}
		decoded++
		if value.Key == "" {
			continue
		}
		peers := make(map[string]bool)
		storage := make(map[string]bool)
		for _, item := range value.Whereis {
			if peerID, found := uuidPeers[item.UUID]; found {
				peers[peerID] = true
			}
			if name, found := uuidStorage[item.UUID]; found {
				storage[name] = true
			}
		}
		if len(peers) == 0 && len(storage) == 0 {
			continue
		}
		entry := contentHolders{peers: make([]string, 0, len(peers)), storage: make([]string, 0, len(storage))}
		for peerID := range peers {
			entry.peers = append(entry.peers, peerID)
		}
		for name := range storage {
			entry.storage = append(entry.storage, name)
		}
		sort.Strings(entry.peers)
		sort.Strings(entry.storage)
		holders[value.Key] = entry
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if commandErr != nil && decoded == 0 {
		return nil, commandErr
	}
	return holders, nil
}
