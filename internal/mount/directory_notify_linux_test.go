//go:build linux

package mount

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

type recordingSignalEmitter struct {
	mu      sync.Mutex
	signals []string
	written chan struct{}
}

func (e *recordingSignalEmitter) Emit(_ dbus.ObjectPath, name string, values ...any) error {
	e.mu.Lock()
	e.signals = append(e.signals, name+" "+values[0].(string))
	e.mu.Unlock()
	e.written <- struct{}{}
	return nil
}

func (*recordingSignalEmitter) Close() error { return nil }

func TestKDEDirectoryNotificationsRefreshEveryAffectedAncestor(t *testing.T) {
	emitter := &recordingSignalEmitter{written: make(chan struct{}, 3)}
	notifier := &kdeDirectoryChangeNotifier{
		connection: emitter,
		mountpoint: "/mnt/dfs storage",
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:    make(map[string]struct{}),
	}
	defer notifier.Close()

	notifier.NotifyPath("projects/new/file.txt")
	for range 3 {
		select {
		case <-emitter.written:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for KDE directory notifications")
		}
	}

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	want := []string{
		"org.kde.KDirNotify.FilesAdded file:///mnt/dfs%20storage",
		"org.kde.KDirNotify.FilesAdded file:///mnt/dfs%20storage/projects",
		"org.kde.KDirNotify.FilesAdded file:///mnt/dfs%20storage/projects/new",
	}
	if len(emitter.signals) != len(want) {
		t.Fatalf("signals = %#v, want %#v", emitter.signals, want)
	}
	for index := range want {
		if emitter.signals[index] != want[index] {
			t.Fatalf("signals = %#v, want %#v", emitter.signals, want)
		}
	}
}
