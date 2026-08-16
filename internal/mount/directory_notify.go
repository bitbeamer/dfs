package mount

// directoryChangeNotifier tells desktop file managers that directory contents
// changed outside a local filesystem request. FUSE entry invalidation keeps
// kernel lookups correct, but does not itself produce the desktop notification
// that directory models such as Dolphin's use to refresh their listings.
type directoryChangeNotifier interface {
	NotifyPath(path string)
	Close()
}
