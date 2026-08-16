//go:build linux

package mount

import (
	"log/slog"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const directoryNotifyDelay = 50 * time.Millisecond

type kdeDirectoryChangeNotifier struct {
	connection dbusSignalEmitter
	mountpoint string
	logger     *slog.Logger

	mu      sync.Mutex
	pending map[string]struct{}
	timer   *time.Timer
	closed  bool
}

type dbusSignalEmitter interface {
	Emit(path dbus.ObjectPath, name string, values ...any) error
	Close() error
}

func newDirectoryChangeNotifier(mountpoint string, logger *slog.Logger) directoryChangeNotifier {
	connection, err := dbus.ConnectSessionBus()
	if err != nil {
		logger.Debug("desktop directory notifications unavailable", "error", err)
		return noopDirectoryChangeNotifier{}
	}
	return &kdeDirectoryChangeNotifier{
		connection: connection,
		mountpoint: filepath.Clean(mountpoint),
		logger:     logger,
		pending:    make(map[string]struct{}),
	}
}

func (n *kdeDirectoryChangeNotifier) NotifyPath(path string) {
	relative := strings.Trim(filepath.Clean(filepath.FromSlash(path)), string(filepath.Separator))
	directory := filepath.Dir(filepath.Join(n.mountpoint, relative))

	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	// Notify every containing directory up to the mount root. If a previously
	// unknown branch arrived, Dolphin may be displaying any one of its known
	// ancestors rather than the changed path's immediate parent.
	for {
		n.pending[directory] = struct{}{}
		if directory == n.mountpoint {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory || !pathWithin(parent, n.mountpoint) {
			n.pending[n.mountpoint] = struct{}{}
			break
		}
		directory = parent
	}
	if n.timer == nil {
		n.timer = time.AfterFunc(directoryNotifyDelay, n.flush)
	} else {
		n.timer.Reset(directoryNotifyDelay)
	}
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (n *kdeDirectoryChangeNotifier) flush() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	directories := make([]string, 0, len(n.pending))
	for directory := range n.pending {
		directories = append(directories, directory)
	}
	clear(n.pending)
	n.timer = nil
	n.mu.Unlock()

	sort.Strings(directories)
	for _, directory := range directories {
		directoryURL := (&url.URL{Scheme: "file", Path: directory}).String()
		if err := n.connection.Emit(
			dbus.ObjectPath("/KDirNotify"),
			"org.kde.KDirNotify.FilesAdded",
			directoryURL,
		); err != nil && n.logger != nil {
			n.logger.Debug("desktop directory notification failed", "directory", directory, "error", err)
		}
	}
}

func (n *kdeDirectoryChangeNotifier) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	if n.timer != nil {
		n.timer.Stop()
	}
	n.mu.Unlock()
	n.connection.Close()
}

type noopDirectoryChangeNotifier struct{}

func (noopDirectoryChangeNotifier) NotifyPath(string) {}
func (noopDirectoryChangeNotifier) Close()            {}
