// Package wakeup delivers repository-specific events to a running DFS mount.
package wakeup

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type Listener struct {
	connection *net.UnixConn
	path       string
}

func Path(repository string) string {
	absolute, err := filepath.Abs(repository)
	if err != nil {
		absolute = repository
	}
	digest := sha256.Sum256([]byte(filepath.Clean(absolute)))
	// Darwin limits Unix-domain socket paths to roughly 100 bytes. A fixed,
	// short runtime location also supports repositories nested under long paths.
	return filepath.Join("/tmp", fmt.Sprintf("dfs-%d-%x-events.sock", os.Getuid(), digest[:8]))
}

func Listen(repository string) (*Listener, error) {
	path := Path(repository)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("DFS event endpoint %s is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	connection, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = connection.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &Listener{connection: connection, path: path}, nil
}

func (l *Listener) Receive() (string, error) {
	buffer := make([]byte, 256)
	count, _, err := l.connection.ReadFromUnix(buffer)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(buffer[:count])), nil
}

func (l *Listener) Close() error {
	err := l.connection.Close()
	removeErr := os.Remove(l.path)
	if err != nil {
		return err
	}
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return nil
}

func Notify(repository, reason string) error {
	connection, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: Path(repository), Net: "unixgram"})
	if err != nil {
		return err
	}
	defer connection.Close()
	_, err = connection.Write([]byte(reason))
	return err
}
