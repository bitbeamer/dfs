package peer

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bitbeamer/dfs/internal/config"
	"github.com/bitbeamer/dfs/internal/repository"
)

const transportKeyFile = "peer-ssh-key"

type transportIdentity struct {
	PrivateKey string
	PublicKey  string
}

func ensureRepositoryTransport(ctx context.Context, repo *repository.Repository) (transportIdentity, error) {
	directory := filepath.Join(repo.Config.Repository, filepath.FromSlash(config.Directory))
	identity, err := ensureTransportIdentity(ctx, directory, repo.Config.PeerID)
	if err != nil {
		return transportIdentity{}, err
	}
	knownHosts := filepath.Join(directory, "known_hosts")
	if _, err := os.Stat(knownHosts); errors.Is(err, os.ErrNotExist) {
		knownHosts = ""
	} else if err != nil {
		return transportIdentity{}, fmt.Errorf("inspect pinned SSH host keys: %w", err)
	}
	sshCommand := transportSSHCommand(identity.PrivateKey, knownHosts)
	if err := repo.ConfigureSSHCommand(ctx, sshCommand); err != nil {
		return transportIdentity{}, fmt.Errorf("configure DFS transport key: %w", err)
	}
	return identity, nil
}

func ensureTransportIdentity(ctx context.Context, directory, peerID string) (transportIdentity, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return transportIdentity{}, fmt.Errorf("create DFS transport directory: %w", err)
	}
	privateKey := filepath.Join(directory, transportKeyFile)
	publicKeyPath := privateKey + ".pub"
	if _, err := os.Stat(privateKey); errors.Is(err, os.ErrNotExist) {
		command := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "dfs-"+peerID, "-f", privateKey)
		if output, commandErr := command.CombinedOutput(); commandErr != nil {
			return transportIdentity{}, fmt.Errorf("generate DFS transport key: %s", strings.TrimSpace(string(output)))
		}
	} else if err != nil {
		return transportIdentity{}, fmt.Errorf("inspect DFS transport key: %w", err)
	}
	if err := os.Chmod(privateKey, 0o600); err != nil {
		return transportIdentity{}, err
	}
	publicKey, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return transportIdentity{}, fmt.Errorf("read DFS transport public key: %w", err)
	}
	normalized, err := normalizePublicKey(string(publicKey))
	if err != nil {
		return transportIdentity{}, err
	}
	return transportIdentity{PrivateKey: privateKey, PublicKey: normalized}, nil
}

func normalizePublicKey(value string) (string, error) {
	fields := strings.Fields(value)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", errors.New("DFS pairing requires an Ed25519 SSH public key")
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(decoded) < 32 || len(decoded) > 1024 {
		return "", errors.New("invalid DFS SSH public key")
	}
	return fields[0] + " " + fields[1], nil
}

func authorizePeer(publicKey, repositoryPath, filesystemID, peerID string) (string, error) {
	publicKey, err := normalizePublicKey(publicKey)
	if err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate DFS executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", err
	}
	shortID := peerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	marker := "dfs-peer-" + shortID + "-" + filesystemID[:12]
	forcedCommand := shellQuote(executable) + " --repo " + shellQuote(repositoryPath) + " peer serve"
	line := `restrict,command="` + authorizedOptionEscape(forcedCommand) + `" ` + publicKey + " " + marker
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine local account for SSH authorization: %w", err)
	}
	directory := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create SSH configuration directory: %w", err)
	}
	path := filepath.Join(directory, "authorized_keys")
	if err := appendAuthorizedLine(path, marker, line); err != nil {
		return "", err
	}
	return marker, nil
}

func appendAuthorizedLine(path, marker, line string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read SSH authorized keys: %w", err)
	}
	for _, current := range strings.Split(string(existing), "\n") {
		if strings.HasSuffix(strings.TrimSpace(current), " "+marker) {
			return nil
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open SSH authorized keys: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure SSH authorized keys: %w", err)
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		if _, err := io.WriteString(file, "\n"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(file, line+"\n"); err != nil {
		return fmt.Errorf("authorize paired DFS peer: %w", err)
	}
	return file.Sync()
}

func removeAuthorizedMarker(marker string) error {
	if marker == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".ssh", "authorized_keys")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	var lines []string
	removed := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasSuffix(strings.TrimSpace(line), " "+marker) {
			removed = true
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !removed {
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

func RevokePeerAuthorization(remoteName string) error {
	if !strings.HasPrefix(remoteName, "dfs-peer-") {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".ssh", "authorized_keys")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept []string
	changed := false
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		fields := strings.Fields(line)
		marker := ""
		if len(fields) > 0 {
			marker = fields[len(fields)-1]
		}
		if strings.HasPrefix(marker, remoteName+"-") {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return nil
	}
	output := strings.Join(kept, "\n")
	if output != "" {
		output += "\n"
	}
	temporary := path + ".dfs-new"
	if err := os.WriteFile(temporary, []byte(output), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func transportSSHCommand(privateKey, knownHosts string) string {
	parts := []string{
		"ssh", "-i", shellQuote(privateKey), "-o", "IdentitiesOnly=no", "-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1",
	}
	if knownHosts != "" {
		parts = append(parts, "-o", "UserKnownHostsFile="+shellQuote(knownHosts), "-o", "StrictHostKeyChecking=yes")
	}
	return strings.Join(parts, " ")
}

func installKnownHosts(path, cloneURL string, keys []string) error {
	if !strings.HasPrefix(cloneURL, "ssh://") {
		return nil
	}
	parsed, err := url.Parse(cloneURL)
	if err != nil || parsed.Hostname() == "" {
		return errors.New("invalid paired SSH clone URL")
	}
	host := parsed.Hostname()
	if port := parsed.Port(); port != "" && port != "22" {
		host = "[" + host + "]:" + port
	}
	if len(keys) == 0 {
		return errors.New("pairing peer supplied no SSH host keys")
	}
	var lines []string
	for _, key := range keys {
		fields := strings.Fields(key)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") {
			continue
		}
		lines = append(lines, host+" "+fields[0]+" "+fields[1])
	}
	if len(lines) == 0 {
		return errors.New("pairing peer supplied no valid SSH host keys")
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	seen := make(map[string]bool)
	var combined []string
	for _, line := range append(strings.Split(strings.TrimSpace(string(existing)), "\n"), lines...) {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		combined = append(combined, line)
	}
	return os.WriteFile(path, []byte(strings.Join(combined, "\n")+"\n"), 0o600)
}

func localSSHHostKeys() []string {
	paths, _ := filepath.Glob("/etc/ssh/ssh_host_*_key.pub")
	var result []string
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fields := strings.Fields(string(data))
		if len(fields) >= 2 && (strings.HasPrefix(fields[0], "ssh-") || strings.HasPrefix(fields[0], "ecdsa-")) {
			result = append(result, fields[0]+" "+fields[1])
		}
	}
	return result
}

func ServeSSH(repositoryPath string) error {
	original := strings.TrimSpace(os.Getenv("SSH_ORIGINAL_COMMAND"))
	if original == "" {
		return errors.New("DFS peer transport can only be invoked by SSH")
	}
	if original == diagnosticCommand {
		return serveDiagnostic(repositoryPath, os.Stdout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	annexShell, err := executablePath("git-annex-shell", "/opt/homebrew/bin", "/usr/local/bin")
	if err != nil {
		return fmt.Errorf("locate git-annex-shell: %w", err)
	}
	command := exec.CommandContext(ctx, annexShell, "-c", original)
	command.Env = append(os.Environ(), "GIT_ANNEX_SHELL_DIRECTORY="+repositoryPath)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func executablePath(name string, fallbackDirectories ...string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}
	for _, directory := range fallbackDirectories {
		candidate := filepath.Join(directory, name)
		info, statErr := os.Stat(candidate)
		if statErr == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func authorizedOptionEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
