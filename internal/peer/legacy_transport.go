package peer

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// removeLegacyAuthorizations deletes only forced-command entries bearing the
// exact marker written by older DFS releases. User-managed entries are kept.
func removeLegacyAuthorizations(filesystemID string) error {
	if len(filesystemID) < 12 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".ssh", "authorized_keys")
	return rewriteLegacyAuthorizations(path, func(marker string) bool {
		return strings.HasPrefix(marker, "dfs-peer-") && strings.HasSuffix(marker, "-"+filesystemID[:12])
	})
}

func removeAuthorizedMarker(marker string) error {
	if marker == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return rewriteLegacyAuthorizations(filepath.Join(home, ".ssh", "authorized_keys"), func(candidate string) bool {
		return candidate == marker
	})
}

func rewriteLegacyAuthorizations(path string, remove func(string) bool) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var lines []string
	changed := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		marker := ""
		if len(fields) > 0 {
			marker = fields[len(fields)-1]
		}
		if remove(marker) {
			changed = true
			continue
		}
		lines = append(lines, line)
	}
	closeErr := file.Close()
	if err := scanner.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if !changed {
		return nil
	}
	data := []byte(strings.Join(lines, "\n"))
	if len(data) > 0 {
		data = append(data, '\n')
	}
	temporary := path + ".dfs-new"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
