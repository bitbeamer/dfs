package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/managed"
	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/store"
	"golang.org/x/text/unicode/norm"
)

type Options struct {
	EventRetention int
	ManagedContent bool
}

type operationResult struct {
	fingerprint string
	err         error
}

// Service is the real repository-backed implementation of API. A Service is
// bound to exactly one logical filesystem, but its API never exposes where or
// how that filesystem is stored or transported.
type Service struct {
	repo              *repository.Repository
	root              string
	events            *eventLog
	operations        sync.Map
	operationMu       sync.Mutex
	pendingOperations map[string]string
	metadataMu        sync.RWMutex
	metadata          map[string]metadataCacheEntry
	stateMu           sync.Mutex
	stateLoaded       bool
	statePaths        map[string]bool
	locks             *lockTable
}

type metadataCacheEntry struct {
	value store.FileMetadata
	found bool
}

func New(repo *repository.Repository, options Options) *Service {
	if options.ManagedContent {
		repo.SetManagedFetcher(managed.FetchPath)
		repo.SetManagedRangeFetcher(managed.FetchRange)
		repo.SetManagedCloser(func() { managed.CloseContentSessions(repo.Config.Repository) })
	}
	return &Service{repo: repo, root: repo.Config.Repository, events: newEventLog(options.EventRetention), pendingOperations: make(map[string]string), metadata: make(map[string]metadataCacheEntry), statePaths: make(map[string]bool), locks: newLockTable()}
}

func (s *Service) Close() error {
	s.events.close()
	s.locks.close()
	return nil
}

func cleanPath(path string) (string, error) {
	if strings.ContainsRune(path, 0) || filepath.IsAbs(filepath.FromSlash(path)) {
		return "", &Error{Code: CodeInvalid, Op: "validate path", Path: path}
	}
	path = norm.NFC.String(filepath.ToSlash(filepath.Clean(filepath.FromSlash(path))))
	if path == "." {
		path = ""
	}
	path = strings.TrimPrefix(path, "./")
	for _, component := range strings.Split(path, "/") {
		if component == ".git" || component == ".." {
			return "", &Error{Code: CodePermission, Op: "validate path", Path: path}
		}
	}
	return path, nil
}

func (s *Service) resolve(path string) (string, string, error) {
	cleaned, err := cleanPath(path)
	if err != nil {
		return "", "", err
	}
	return cleaned, filepath.Join(s.root, filepath.FromSlash(cleaned)), nil
}

func (s *Service) Lookup(ctx context.Context, path string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, classify("lookup", path, err)
	}
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return Entry{}, err
	}
	attributes, err := s.attributes(full, cleaned)
	if err != nil {
		return Entry{}, classify("lookup", cleaned, err)
	}
	return Entry{Name: filepath.Base(cleaned), Path: cleaned, Attributes: attributes}, nil
}

func (s *Service) attributes(full, logical string) (Attributes, error) {
	annexTarget, annexed, linkInfo, err := inspectAnnexLink(full)
	if err != nil {
		return Attributes{}, err
	}
	info := linkInfo
	if linkInfo.Mode()&os.ModeSymlink == 0 || annexed {
		info, err = os.Stat(full)
		if err != nil {
			if !annexed || !errors.Is(err, os.ErrNotExist) {
				return Attributes{}, err
			}
			info = linkInfo
		}
	}
	attributes := attributesFromInfo(info)
	if annexed {
		attributes.Kind = KindFile
		attributes.Mode = attributes.Mode&^syscall.S_IFMT | syscall.S_IFREG | 0o644
		if size, ok := annexSize(annexTarget); ok {
			attributes.Size = size
			attributes.Blocks = (size + 511) / 512
		}
	}
	if s.repo.Store != nil {
		if metadata, found, metadataErr := s.fileMetadata(logical); metadataErr != nil {
			return Attributes{}, metadataErr
		} else if found {
			attributes.Mode = attributes.Mode&^0o7777 | metadata.Mode&0o7777
			attributes.UID, attributes.GID = metadata.UID, metadata.GID
			attributes.Accessed = time.Unix(0, metadata.AtimeNS)
			attributes.Modified = time.Unix(0, metadata.MtimeNS)
			attributes.Changed = time.Unix(0, metadata.CtimeNS)
		}
	}
	return attributes, nil
}

func (s *Service) fileMetadata(path string) (store.FileMetadata, bool, error) {
	s.metadataMu.RLock()
	cached, ok := s.metadata[path]
	s.metadataMu.RUnlock()
	if ok {
		return cached.value, cached.found, nil
	}
	value, found, err := s.repo.Store.FileMetadata(path)
	if err == nil {
		s.metadataMu.Lock()
		s.metadata[path] = metadataCacheEntry{value: value, found: found}
		s.metadataMu.Unlock()
	}
	return value, found, err
}

func (s *Service) clearMetadataCache() {
	s.metadataMu.Lock()
	clear(s.metadata)
	s.metadataMu.Unlock()
}

func (s *Service) hasFileState(path string) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.stateLoaded {
		paths, err := s.repo.Store.FileStatePaths()
		if err != nil {
			return true
		}
		for _, candidate := range paths {
			s.statePaths[candidate] = true
		}
		s.stateLoaded = true
	}
	for candidate := range s.statePaths {
		if candidate == path || strings.HasPrefix(candidate, path+"/") {
			return true
		}
	}
	return false
}

func (s *Service) markFileState(path string) {
	s.stateMu.Lock()
	s.statePaths[path] = true
	s.stateMu.Unlock()
}

func (s *Service) removeFileState(path string) {
	s.stateMu.Lock()
	for candidate := range s.statePaths {
		if candidate == path || strings.HasPrefix(candidate, path+"/") {
			delete(s.statePaths, candidate)
		}
	}
	s.stateMu.Unlock()
}

func (s *Service) renameFileState(oldPath, newPath string) {
	s.stateMu.Lock()
	updates := make(map[string]bool)
	for candidate := range s.statePaths {
		if candidate == oldPath || strings.HasPrefix(candidate, oldPath+"/") {
			delete(s.statePaths, candidate)
			updates[newPath+strings.TrimPrefix(candidate, oldPath)] = true
		}
	}
	for candidate := range updates {
		s.statePaths[candidate] = true
	}
	s.stateMu.Unlock()
}

func attributesFromInfo(info os.FileInfo) Attributes {
	kind := KindFile
	switch {
	case info.IsDir():
		kind = KindDirectory
	case info.Mode()&os.ModeSymlink != 0:
		kind = KindSymlink
	}
	mode := uint32(info.Mode().Perm())
	switch kind {
	case KindDirectory:
		mode |= syscall.S_IFDIR
	case KindSymlink:
		mode |= syscall.S_IFLNK
	default:
		mode |= syscall.S_IFREG
	}
	attributes := Attributes{Kind: kind, Mode: mode, Size: info.Size(), Blocks: (info.Size() + 511) / 512,
		Modified: info.ModTime(), Changed: info.ModTime(), Accessed: info.ModTime()}
	applyPlatformAttributes(info, &attributes)
	return attributes
}

func (s *Service) ReadDirectory(ctx context.Context, path string, request PageRequest) (EntryPage, error) {
	if err := ctx.Err(); err != nil {
		return EntryPage{}, classify("read directory", path, err)
	}
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return EntryPage{}, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return EntryPage{}, classify("read directory", cleaned, err)
	}
	page := EntryPage{Entries: make([]DirectoryEntry, 0, len(entries))}
	for _, directoryEntry := range entries {
		if directoryEntry.Name() == ".git" || directoryEntry.Name() <= request.After {
			continue
		}
		if err := ctx.Err(); err != nil {
			return EntryPage{}, classify("read directory", cleaned, err)
		}
		name := norm.NFC.String(directoryEntry.Name())
		child := filepath.ToSlash(filepath.Join(cleaned, name))
		kind := KindFile
		mode := uint32(syscall.S_IFREG)
		entryType := directoryEntry.Type()
		switch {
		case entryType.IsDir():
			kind, mode = KindDirectory, syscall.S_IFDIR
		case entryType&os.ModeSymlink != 0:
			if _, annexed := annexLinkTarget(filepath.Join(full, directoryEntry.Name())); annexed {
				kind, mode = KindFile, syscall.S_IFREG
			} else {
				kind, mode = KindSymlink, syscall.S_IFLNK
			}
		}
		entry := DirectoryEntry{Name: name, Path: child, Kind: kind, Mode: mode}
		page.Entries = append(page.Entries, entry)
		if request.Limit > 0 && len(page.Entries) == request.Limit {
			page.Next = entry.Name
			break
		}
	}
	return page, nil
}

func (s *Service) ReadLink(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", classify("read link", path, err)
	}
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return "", err
	}
	if _, annexed := annexLinkTarget(full); annexed {
		return "", &Error{Code: CodeInvalid, Op: "read link", Path: cleaned, Err: errors.New("logical entry is a file")}
	}
	value, err := os.Readlink(full)
	return value, classify("read link", cleaned, err)
}

func (s *Service) MakeDirectory(ctx context.Context, path string, mode uint32, operationID string) error {
	return s.mutate(ctx, "mkdir", path, operationID, fmt.Sprintf("%o", mode), func(full, _ string) error {
		return os.Mkdir(full, os.FileMode(mode))
	})
}

func (s *Service) MakeNode(ctx context.Context, path string, mode, device uint32, operationID string) error {
	return s.mutate(ctx, "mknod", path, operationID, fmt.Sprintf("%o:%d", mode, device), func(full, _ string) error {
		return syscall.Mknod(full, mode, int(device))
	})
}

func (s *Service) Rename(ctx context.Context, oldPath, newPath, operationID string) error {
	oldClean, oldFull, err := s.resolve(oldPath)
	if err != nil {
		return err
	}
	newClean, newFull, err := s.resolve(newPath)
	if err != nil {
		return err
	}
	if oldClean == newClean {
		return nil
	}
	action := func() error {
		if err := ctx.Err(); err != nil {
			return classify("rename", oldClean, err)
		}
		err := s.repo.WithWorkTreeLock(func() error { return os.Rename(oldFull, newFull) })
		state := s.repo.Store != nil && (s.hasFileState(oldClean) || s.hasFileState(newClean))
		if err == nil && state {
			err = s.repo.Store.RenameFileState(oldClean, newClean)
		}
		if err != nil {
			return classify("rename", oldClean, err)
		}
		s.clearMetadataCache()
		if state {
			s.renameFileState(oldClean, newClean)
		}
		s.events.publish("rename", operationID, oldClean, newClean)
		return nil
	}
	if operationID == "" {
		return action()
	}
	fingerprint := "rename\x00" + oldClean + "\x00" + newClean
	return s.idempotent(operationID, fingerprint, action)
}

func (s *Service) Remove(ctx context.Context, path string, directory bool, operationID string) error {
	op := "remove"
	if directory {
		op = "remove directory"
	}
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return err
	}
	action := func() error {
		if err := ctx.Err(); err != nil {
			return classify(op, cleaned, err)
		}
		if err := s.repo.WithWorkTreeLock(func() error { return os.Remove(full) }); err != nil {
			return classify(op, cleaned, err)
		}
		state := s.repo.Store != nil && s.hasFileState(cleaned)
		if state {
			if err := s.repo.Store.RemoveFileState(cleaned); err != nil {
				return classify(op, cleaned, err)
			}
		}
		s.clearMetadataCache()
		if state {
			s.removeFileState(cleaned)
		}
		s.events.publish(op, operationID, cleaned)
		return nil
	}
	if operationID == "" {
		return action()
	}
	return s.idempotent(operationID, op+"\x00"+cleaned+"\x00"+strconv.FormatBool(directory), action)
}

func (s *Service) Link(ctx context.Context, oldPath, newPath, operationID string) error {
	oldClean, oldFull, err := s.resolve(oldPath)
	if err != nil {
		return err
	}
	return s.mutate(ctx, "link", newPath, operationID, oldClean, func(newFull, _ string) error {
		return os.Link(oldFull, newFull)
	})
}

func (s *Service) Symlink(ctx context.Context, target, linkPath, operationID string) error {
	return s.mutate(ctx, "symlink", linkPath, operationID, target, func(full, _ string) error {
		return os.Symlink(target, full)
	})
}

func (s *Service) mutate(ctx context.Context, kind, path, operationID, extra string, operation func(string, string) error) error {
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return err
	}
	action := func() error {
		if err := ctx.Err(); err != nil {
			return classify(kind, cleaned, err)
		}
		err := s.repo.WithWorkTreeLock(func() error { return operation(full, cleaned) })
		if err != nil {
			return classify(kind, cleaned, err)
		}
		s.clearMetadataCache()
		s.events.publish(kind, operationID, cleaned)
		return nil
	}
	if operationID == "" {
		return action()
	}
	fingerprint := kind + "\x00" + cleaned + "\x00" + extra
	return s.idempotent(operationID, fingerprint, action)
}

func (s *Service) idempotent(operationID, fingerprint string, action func() error) error {
	if operationID == "" {
		return action()
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if previous, found := s.operations.Load(operationID); found {
		result := previous.(operationResult)
		if result.fingerprint != fingerprint {
			return &Error{Code: CodeConflict, Op: "reuse operation ID", Err: errors.New("operation ID belongs to a different request")}
		}
		return result.err
	}
	err := action()
	s.operations.Store(operationID, operationResult{fingerprint: fingerprint, err: err})
	return err
}

func (s *Service) GetXattr(ctx context.Context, path, name string) ([]byte, error) {
	cleaned, _, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, classify("get xattr", cleaned, err)
	}
	value, err := s.repo.Store.XAttr(cleaned, name)
	if errors.Is(err, store.ErrXAttrNotFound) {
		return nil, &Error{Code: CodeNotFound, Op: "get xattr", Path: cleaned, Err: err}
	}
	return value, classify("get xattr", cleaned, err)
}

func (s *Service) ListXattrs(ctx context.Context, path string) ([]string, error) {
	cleaned, _, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, classify("list xattrs", cleaned, err)
	}
	names, err := s.repo.Store.ListXAttrs(cleaned)
	return names, classify("list xattrs", cleaned, err)
}

func (s *Service) SetXattr(ctx context.Context, path, name string, value []byte, flags XattrFlags, operationID string) error {
	return s.mutate(ctx, "set xattr", path, operationID, name+"\x00"+string(value), func(_ string, cleaned string) error {
		err := s.repo.Store.SetXAttr(cleaned, name, value, int(flags))
		switch {
		case errors.Is(err, store.ErrXAttrExists):
			return &Error{Code: CodeAlreadyExists, Op: "set xattr", Path: cleaned, Err: err}
		case errors.Is(err, store.ErrXAttrNotFound):
			return &Error{Code: CodeNotFound, Op: "set xattr", Path: cleaned, Err: err}
		default:
			if err == nil {
				s.markFileState(cleaned)
			}
			return err
		}
	})
}

func (s *Service) RemoveXattr(ctx context.Context, path, name, operationID string) error {
	return s.mutate(ctx, "remove xattr", path, operationID, name, func(_ string, cleaned string) error {
		err := s.repo.Store.RemoveXAttr(cleaned, name)
		if errors.Is(err, store.ErrXAttrNotFound) {
			return &Error{Code: CodeNotFound, Op: "remove xattr", Path: cleaned, Err: err}
		}
		return err
	})
}

func (s *Service) SetAttributes(ctx context.Context, path string, changes AttributeChanges, operationID string) (Attributes, error) {
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return Attributes{}, err
	}
	fingerprint := fmt.Sprintf("attrs\x00%s\x00%v", cleaned, changes)
	err = s.idempotent(operationID, fingerprint, func() error {
		if err := ctx.Err(); err != nil {
			return classify("set attributes", cleaned, err)
		}
		_, annexed := annexLinkTarget(full)
		if changes.Size != nil && annexed {
			transaction, beginErr := s.BeginWrite(ctx, WriteRequest{Path: cleaned})
			if beginErr != nil {
				return beginErr
			}
			if resizeErr := transaction.Truncate(*changes.Size); resizeErr != nil {
				_ = transaction.Abort(context.Background())
				return resizeErr
			}
			if commitErr := transaction.Commit(ctx); commitErr != nil {
				return commitErr
			}
		}
		mutationErr := s.repo.WithWorkTreeLock(func() error {
			if changes.Size != nil && !annexed {
				if err := os.Truncate(full, *changes.Size); err != nil {
					return err
				}
			}
			if changes.Mode != nil && !annexed {
				if err := os.Chmod(full, os.FileMode(*changes.Mode)); err != nil {
					return err
				}
			}
			if (changes.UID != nil || changes.GID != nil) && !annexed {
				uid, gid := -1, -1
				if changes.UID != nil {
					uid = int(*changes.UID)
				}
				if changes.GID != nil {
					gid = int(*changes.GID)
				}
				if err := os.Chown(full, uid, gid); err != nil {
					return err
				}
			}
			if changes.Accessed != nil || changes.Modified != nil {
				info, statErr := os.Stat(full)
				if statErr != nil {
					return statErr
				}
				accessed, modified := info.ModTime(), info.ModTime()
				if changes.Accessed != nil {
					accessed = *changes.Accessed
				}
				if changes.Modified != nil {
					modified = *changes.Modified
				}
				if err := os.Chtimes(full, accessed, modified); err != nil {
					return err
				}
			}
			if s.repo.Store != nil {
				attributes, attrErr := s.attributes(full, cleaned)
				if attrErr != nil {
					return attrErr
				}
				if changes.Mode != nil {
					attributes.Mode = attributes.Mode&^0o7777 | *changes.Mode&0o7777
				}
				if changes.UID != nil {
					attributes.UID = *changes.UID
				}
				if changes.GID != nil {
					attributes.GID = *changes.GID
				}
				if changes.Accessed != nil {
					attributes.Accessed = *changes.Accessed
				}
				if changes.Modified != nil {
					attributes.Modified = *changes.Modified
				}
				metadata := store.FileMetadata{Mode: attributes.Mode, UID: attributes.UID, GID: attributes.GID,
					AtimeNS: attributes.Accessed.UnixNano(), MtimeNS: attributes.Modified.UnixNano(), CtimeNS: time.Now().UnixNano()}
				if err := s.repo.Store.SaveFileMetadata(cleaned, metadata); err != nil {
					return err
				}
				s.metadataMu.Lock()
				s.metadata[cleaned] = metadataCacheEntry{value: metadata, found: true}
				s.metadataMu.Unlock()
				s.markFileState(cleaned)
				return nil
			}
			return nil
		})
		if mutationErr == nil {
			s.events.publish("set attributes", operationID, cleaned)
		}
		return mutationErr
	})
	if err != nil {
		return Attributes{}, classify("set attributes", cleaned, err)
	}
	entry, err := s.Lookup(ctx, cleaned)
	return entry.Attributes, err
}

func (s *Service) SetPin(ctx context.Context, path string, scope PinScope, pinned bool) error {
	cleaned, _, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return classify("set pin", cleaned, err)
	}
	switch scope {
	case PinLocal:
		if pinned {
			err = s.repo.Pin(ctx, cleaned)
		} else {
			err = s.repo.Unpin(cleaned)
		}
	case PinCluster:
		err = s.repo.SetClusterPin(ctx, cleaned, pinned)
	default:
		return &Error{Code: CodeInvalid, Op: "set pin", Path: cleaned, Err: fmt.Errorf("unknown scope %q", scope)}
	}
	if err == nil {
		s.events.publish("pin", "", cleaned)
	}
	return classify("set pin", cleaned, err)
}

func (s *Service) Pins(ctx context.Context) ([]Pin, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify("list pins", "", err)
	}
	records, err := s.repo.Store.PinRecords()
	if err != nil {
		return nil, classify("list pins", "", err)
	}
	result := make([]Pin, 0, len(records))
	for _, record := range records {
		result = append(result, Pin{Path: record.Path, Scope: PinScope(record.Scope)})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path == result[j].Path {
			return result[i].Scope < result[j].Scope
		}
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func (s *Service) Evict(ctx context.Context, path string) error {
	cleaned, _, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err = s.repo.Evict(ctx, cleaned); err == nil {
		s.events.publish("evict", "", cleaned)
	}
	return classify("evict", cleaned, err)
}

func (s *Service) Subscribe(ctx context.Context, after Cursor) (Subscription, error) {
	return s.events.subscribe(ctx, after)
}
