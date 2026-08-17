// Package core defines DFS's stable, in-process frontend API and its reference
// repository-backed implementation. The API deliberately contains no FUSE,
// Git, git-annex, transport, repository-path, or private-state types.
package core

import (
	"context"
	"io"
	"time"
)

// APIVersion changes only when an incompatible semantic change is unavoidable.
// Additive changes preserve compatibility within the same major version.
const APIVersion = 1

// API is the complete in-process surface available to DFS frontends. Methods
// are safe for concurrent use unless a handle documents otherwise.
type API interface {
	Namespace
	Content
	Policy
	History
	Health
	Events
}

type Namespace interface {
	Lookup(context.Context, string) (Entry, error)
	ReadDirectory(context.Context, string, PageRequest) (EntryPage, error)
	ReadLink(context.Context, string) (string, error)
	BeginWrite(context.Context, WriteRequest) (WriteTransaction, error)
	MakeDirectory(context.Context, string, uint32, string) error
	MakeNode(context.Context, string, uint32, uint32, string) error
	Rename(context.Context, string, string, string) error
	Remove(context.Context, string, bool, string) error
	Link(context.Context, string, string, string) error
	Symlink(context.Context, string, string, string) error
	SetAttributes(context.Context, string, AttributeChanges, string) (Attributes, error)
	GetXattr(context.Context, string, string) ([]byte, error)
	ListXattrs(context.Context, string) ([]string, error)
	SetXattr(context.Context, string, string, []byte, XattrFlags, string) error
	RemoveXattr(context.Context, string, string, string) error
	GetLock(context.Context, string, uint64, FileLock) (FileLock, error)
	SetLock(context.Context, string, uint64, FileLock, bool) error
}

type Content interface {
	// OpenRead returns a stable snapshot of one file version. ReadAt follows
	// io.ReaderAt partial-read semantics and is caller-paced, providing natural
	// backpressure. Closing it cancels an in-flight remote range request.
	OpenRead(context.Context, string) (ReadHandle, error)
	ContentVersion(context.Context, string) (string, error)
	Evict(context.Context, string) error
}

type Policy interface {
	SetPin(context.Context, string, PinScope, bool) error
	Pins(context.Context) ([]Pin, error)
}

type History interface {
	History(context.Context, string) ([]Revision, error)
}

type Health interface {
	Health(context.Context) (HealthSnapshot, error)
	Capacity(context.Context) (FilesystemCapacity, error)
}

type Events interface {
	// Subscribe resumes strictly after the supplied cursor. Cursor zero starts
	// at the oldest retained event. Next blocks without blocking publishers and
	// returns ErrEventGone if the requested cursor has fallen out of retention.
	Subscribe(context.Context, Cursor) (Subscription, error)
}

type Kind uint8

const (
	KindUnknown Kind = iota
	KindFile
	KindDirectory
	KindSymlink
)

type Attributes struct {
	Kind       Kind
	Mode       uint32
	UID        uint32
	GID        uint32
	Size       int64
	Inode      uint64
	Blocks     int64
	Accessed   time.Time
	Modified   time.Time
	Changed    time.Time
	Generation uint64
}

type Entry struct {
	Name       string
	Path       string
	Attributes Attributes
}

type DirectoryEntry struct {
	Name  string
	Path  string
	Kind  Kind
	Mode  uint32
	Inode uint64
}

type PageRequest struct {
	// After is the last name returned by the preceding page. Empty starts at
	// the beginning. Limit <= 0 requests all remaining entries.
	After string
	Limit int
}

type EntryPage struct {
	Entries []DirectoryEntry
	Next    string
}

type ReadHandle interface {
	io.ReaderAt
	io.Closer
	Size() int64
	// FileDescriptor exposes an optional direct local-content descriptor for
	// zero-copy-capable in-process frontends. Callers do not own the descriptor.
	FileDescriptor() (uintptr, bool)
	// DirectIO requests that a mounted frontend bypass kernel page caching for
	// content whose backing identity may change independently of its pathname.
	DirectIO() bool
	Version() string
}

type WriteRequest struct {
	Path        string
	OperationID string
	Mode        uint32
	Create      bool
	Exclusive   bool
	Truncate    bool
}

type WriteTransaction interface {
	io.ReaderAt
	io.WriterAt
	Truncate(int64) error
	Allocate(int64, int64, uint32) error
	SetAttributes(AttributeChanges) error
	Rename(string) error
	// Commit atomically publishes all successful writes. A retry with the same
	// non-empty operation ID is idempotent. Commit after Abort is invalid.
	Commit(context.Context) error
	Abort(context.Context) error
	Close() error
}

type AttributeChanges struct {
	Mode     *uint32
	UID      *uint32
	GID      *uint32
	Size     *int64
	Accessed *time.Time
	Modified *time.Time
}

type XattrFlags uint8

const (
	XattrCreate XattrFlags = 1 << iota
	XattrReplace
)

type LockKind uint8

const (
	LockUnlocked LockKind = iota
	LockRead
	LockWrite
)

type FileLock struct {
	Start uint64
	End   uint64
	Kind  LockKind
	PID   uint32
}

type PinScope string

const (
	PinLocal   PinScope = "local"
	PinCluster PinScope = "cluster"
)

type Pin struct {
	Path  string
	Scope PinScope
}

type Revision struct {
	ID      string
	Time    time.Time
	Author  string
	Summary string
}

type HealthSnapshot struct {
	LogicalFiles       int64
	LogicalBytes       int64
	ContentFiles       int64
	ContentBytes       int64
	CacheBytes         int64
	CacheLimitBytes    int64
	MissingPinnedFiles int64
	DiskAvailableBytes int64
	Pins               []PinHealth
}

// FilesystemCapacity describes the backing storage available to a frontend.
// Values are physical storage counters, not logical namespace size or DFS
// cache quotas.
type FilesystemCapacity struct {
	Blocks      uint64
	FreeBlocks  uint64
	AvailBlocks uint64
	Files       uint64
	FreeFiles   uint64
	BlockSize   uint32
	NameLength  uint32
}

type PinHealth struct {
	Pin
	Kind         Kind
	Status       string
	LogicalFiles int64
	LogicalBytes int64
	MissingFiles int64
	MissingBytes int64
}

type Cursor uint64

type Event struct {
	Cursor      Cursor
	OperationID string
	Kind        string
	Paths       []string
	At          time.Time
}

type Subscription interface {
	Next(context.Context) (Event, error)
	Close() error
}

type ErrorCode string

const (
	CodeCanceled      ErrorCode = "canceled"
	CodeNotFound      ErrorCode = "not_found"
	CodeAlreadyExists ErrorCode = "already_exists"
	CodePermission    ErrorCode = "permission_denied"
	CodeInvalid       ErrorCode = "invalid_argument"
	CodeConflict      ErrorCode = "conflict"
	CodeUnavailable   ErrorCode = "unavailable"
	CodeNoSpace       ErrorCode = "no_space"
	CodeNotSupported  ErrorCode = "not_supported"
	CodeEventGone     ErrorCode = "event_gone"
	CodeInternal      ErrorCode = "internal"
)

type Error struct {
	Code ErrorCode
	Op   string
	Path string
	Err  error
}

var _ API = (*Service)(nil)
