//go:build !linux

package mount

import "log/slog"

type noopDirectoryChangeNotifier struct{}

func newDirectoryChangeNotifier(string, *slog.Logger) directoryChangeNotifier {
	return noopDirectoryChangeNotifier{}
}

func (noopDirectoryChangeNotifier) NotifyPath(string) {}
func (noopDirectoryChangeNotifier) Close()            {}
