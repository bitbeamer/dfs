package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bitbeamer/dfs/internal/store"
)

func annexLinkTarget(path string) (string, bool) {
	target, annexed, _, _ := inspectAnnexLink(path)
	return target, annexed
}

func inspectAnnexLink(path string) (string, bool, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false, info, err
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", false, info, err
	}
	normalized := filepath.ToSlash(target)
	annexed := strings.Contains(normalized, "/.git/annex/objects/") || strings.HasPrefix(normalized, ".git/annex/objects/")
	return normalized, annexed, info, nil
}

func annexSize(target string) (int64, bool) {
	key := filepath.Base(filepath.FromSlash(target))
	start := strings.Index(key, "-s")
	if start < 0 {
		return 0, false
	}
	rest := key[start+2:]
	end := strings.Index(rest, "--")
	if end <= 0 {
		return 0, false
	}
	size, err := strconv.ParseInt(rest[:end], 10, 64)
	return size, err == nil
}

func (s *Service) OpenRead(ctx context.Context, path string) (handleResult ReadHandle, returnErr error) {
	started := time.Now()
	defer func() {
		mode := "failed"
		switch handleResult.(type) {
		case *localReadHandle:
			mode = "local"
		case *rangeReadHandle:
			mode = "remote-range"
		}
		s.repo.LogContentReadDebug("content open completed", "path", path, "mode", mode,
			"duration", time.Since(started), "error", returnErr)
	}()
	if err := ctx.Err(); err != nil {
		return nil, classify("open read", path, err)
	}
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	target, annexed, linkInfo, inspectErr := inspectAnnexLink(full)
	if inspectErr != nil && !errors.Is(inspectErr, os.ErrNotExist) {
		return nil, classify("open read", cleaned, inspectErr)
	}
	handle, openErr := os.Open(full)
	if openErr == nil {
		size := int64(0)
		if annexed {
			if parsed, ok := annexSize(target); ok {
				size = parsed
			} else if info, statErr := handle.Stat(); statErr == nil {
				size = info.Size()
			} else {
				_ = handle.Close()
				return nil, classify("open read", cleaned, statErr)
			}
		} else if linkInfo != nil && linkInfo.Mode()&os.ModeSymlink == 0 {
			size = linkInfo.Size()
		} else if info, statErr := handle.Stat(); statErr == nil {
			size = info.Size()
		} else {
			_ = handle.Close()
			return nil, classify("open read", cleaned, statErr)
		}
		if s.repo.Store != nil {
			s.repo.Touch(cleaned)
		}
		version := fileVersion(linkInfo)
		if annexed {
			version = "annex:" + target
		}
		return &localReadHandle{File: handle, size: size, direct: annexed, version: version}, nil
	}
	if !annexed || !errors.Is(openErr, os.ErrNotExist) {
		return nil, classify("open read", cleaned, openErr)
	}
	size, sizeKnown := annexSize(target)
	if sizeKnown && s.repo.CanStreamRanges() {
		readCtx, cancel := context.WithCancel(ctx)
		if s.repo.Store != nil {
			s.repo.Touch(cleaned)
		}
		return &rangeReadHandle{service: s, path: cleaned, key: filepath.Base(filepath.FromSlash(target)), size: size, ctx: readCtx, cancel: cancel, version: "annex:" + target}, nil
	}
	if err := s.repo.Fetch(ctx, cleaned, ""); err != nil {
		return nil, classify("hydrate", cleaned, err)
	}
	handle, err = os.Open(full)
	if err != nil {
		return nil, classify("open read", cleaned, err)
	}
	if !sizeKnown {
		info, statErr := handle.Stat()
		if statErr != nil {
			_ = handle.Close()
			return nil, classify("open read", cleaned, statErr)
		}
		size = info.Size()
	}
	if s.repo.Store != nil {
		s.repo.Touch(cleaned)
	}
	return &localReadHandle{File: handle, size: size, direct: true, version: "annex:" + target}, nil
}

func (s *Service) ContentVersion(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", classify("content version", path, err)
	}
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return "", err
	}
	target, annexed, info, inspectErr := inspectAnnexLink(full)
	if inspectErr != nil {
		return "", classify("content version", cleaned, inspectErr)
	}
	if annexed {
		return "annex:" + target, nil
	}
	return fileVersion(info), nil
}

func fileVersion(info os.FileInfo) string {
	if info == nil {
		return ""
	}
	attributes := attributesFromInfo(info)
	return fmt.Sprintf("%d:%d:%d", attributes.Inode, info.ModTime().UnixNano(), info.Size())
}

type localReadHandle struct {
	*os.File
	size    int64
	direct  bool
	version string
}

func (h *localReadHandle) Size() int64                     { return h.size }
func (h *localReadHandle) FileDescriptor() (uintptr, bool) { return h.Fd(), true }
func (h *localReadHandle) DirectIO() bool                  { return h.direct }
func (h *localReadHandle) Version() string                 { return h.version }

type rangeReadHandle struct {
	service *Service
	path    string
	key     string
	size    int64
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	version string
}

func (h *rangeReadHandle) Size() int64                     { return h.size }
func (h *rangeReadHandle) FileDescriptor() (uintptr, bool) { return 0, false }
func (h *rangeReadHandle) DirectIO() bool                  { return true }
func (h *rangeReadHandle) Version() string                 { return h.version }

func (h *rangeReadHandle) ReadAt(destination []byte, offset int64) (int, error) {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return 0, &Error{Code: CodeCanceled, Op: "read", Path: h.path, Err: os.ErrClosed}
	}
	if offset < 0 {
		return 0, &Error{Code: CodeInvalid, Op: "read", Path: h.path, Err: errors.New("negative offset")}
	}
	if offset >= h.size {
		return 0, io.EOF
	}
	if int64(len(destination)) > h.size-offset {
		destination = destination[:h.size-offset]
	}
	n, err := h.service.repo.ReadRange(h.ctx, h.path, h.key, h.size, offset, destination)
	if err != nil {
		return n, classify("read", h.path, err)
	}
	if n != len(destination) {
		return n, io.EOF
	}
	return n, nil
}

func (h *rangeReadHandle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		h.cancel()
	}
	return nil
}

func (s *Service) BeginWrite(ctx context.Context, request WriteRequest) (WriteTransaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify("begin write", request.Path, err)
	}
	cleaned, destination, err := s.resolve(request.Path)
	if err != nil {
		return nil, err
	}
	fingerprint := fmt.Sprintf("write\x00%s\x00%d\x00%t\x00%t\x00%t", cleaned, request.Mode, request.Create, request.Exclusive, request.Truncate)
	if request.OperationID != "" {
		s.operationMu.Lock()
		if previous, found := s.operations.Load(request.OperationID); found {
			result := previous.(operationResult)
			s.operationMu.Unlock()
			if result.fingerprint != fingerprint {
				return nil, &Error{Code: CodeConflict, Op: "begin write", Path: cleaned, Err: errors.New("operation ID belongs to a different request")}
			}
			return &replayWrite{result: result.err}, nil
		}
		s.operationMu.Unlock()
	}
	info, statErr := os.Stat(destination)
	exists := statErr == nil
	if errors.Is(statErr, os.ErrNotExist) {
		if linkInfo, linkErr := os.Lstat(destination); linkErr == nil {
			info, statErr, exists = linkInfo, nil, true
		}
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, classify("begin write", cleaned, statErr)
	}
	if !exists && !request.Create {
		return nil, &Error{Code: CodeNotFound, Op: "begin write", Path: cleaned, Err: os.ErrNotExist}
	}
	if exists && request.Create && request.Exclusive {
		return nil, &Error{Code: CodeAlreadyExists, Op: "begin write", Path: cleaned, Err: os.ErrExist}
	}
	if request.OperationID != "" {
		s.operationMu.Lock()
		if previous, found := s.operations.Load(request.OperationID); found {
			result := previous.(operationResult)
			s.operationMu.Unlock()
			if result.fingerprint != fingerprint {
				return nil, &Error{Code: CodeConflict, Op: "begin write", Path: cleaned, Err: errors.New("operation ID belongs to a different request")}
			}
			return &replayWrite{result: result.err}, nil
		}
		if pending, found := s.pendingOperations[request.OperationID]; found {
			s.operationMu.Unlock()
			message := "operation is already in progress"
			if pending != fingerprint {
				message = "operation ID belongs to a different request"
			}
			return nil, &Error{Code: CodeConflict, Op: "begin write", Path: cleaned, Err: errors.New(message)}
		}
		s.pendingOperations[request.OperationID] = fingerprint
		s.operationMu.Unlock()
	}
	privateDirectory := filepath.Join(s.root, ".git", "dfs", "transactions")
	if err := os.MkdirAll(privateDirectory, 0o700); err != nil {
		s.releasePending(request.OperationID, fingerprint)
		return nil, classify("begin write", cleaned, err)
	}
	staging, err := os.CreateTemp(privateDirectory, "write-*")
	if err != nil {
		s.releasePending(request.OperationID, fingerprint)
		return nil, classify("begin write", cleaned, err)
	}
	cleanup := func(err error) (WriteTransaction, error) {
		_ = staging.Close()
		_ = os.Remove(staging.Name())
		s.releasePending(request.OperationID, fingerprint)
		return nil, classify("begin write", cleaned, err)
	}
	mode := os.FileMode(request.Mode)
	if mode == 0 {
		mode = 0o644
		if exists {
			mode = info.Mode().Perm() | 0o200
		}
	}
	if err := staging.Chmod(mode.Perm()); err != nil {
		return cleanup(err)
	}
	if exists && !request.Truncate {
		source, err := s.OpenRead(ctx, cleaned)
		if err != nil {
			return cleanup(err)
		}
		_, copyErr := copyReaderAt(ctx, staging, source, source.Size())
		closeErr := source.Close()
		if copyErr != nil {
			return cleanup(copyErr)
		}
		if closeErr != nil {
			return cleanup(closeErr)
		}
		if _, err := staging.Seek(0, io.SeekStart); err != nil {
			return cleanup(err)
		}
	}
	return &writeTransaction{service: s, file: staging, destination: destination, path: cleaned,
		operationID: request.OperationID, fingerprint: fingerprint}, nil
}

func copyReaderAt(ctx context.Context, destination io.Writer, source io.ReaderAt, size int64) (int64, error) {
	const blockSize = 1 << 20
	buffer := make([]byte, blockSize)
	var offset, written int64
	for offset < size {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		want := int64(len(buffer))
		if size-offset < want {
			want = size - offset
		}
		n, readErr := source.ReadAt(buffer[:want], offset)
		if n > 0 {
			count, writeErr := destination.Write(buffer[:n])
			written += int64(count)
			if writeErr != nil {
				return written, writeErr
			}
			if count != n {
				return written, io.ErrShortWrite
			}
			offset += int64(n)
		}
		if readErr != nil && !(errors.Is(readErr, io.EOF) && offset == size) {
			return written, readErr
		}
		if n == 0 {
			return written, io.ErrUnexpectedEOF
		}
	}
	return written, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	err = directory.Sync()
	if runtime.GOOS == "darwin" && errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}

type writeTransaction struct {
	service     *Service
	file        *os.File
	destination string
	path        string
	operationID string
	fingerprint string
	mu          sync.Mutex
	finished    bool
}

func (t *writeTransaction) ReadAt(data []byte, offset int64) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return 0, &Error{Code: CodeConflict, Op: "read write transaction", Path: t.path, Err: errors.New("transaction is finished")}
	}
	n, err := t.file.ReadAt(data, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		err = classify("read write transaction", t.path, err)
	}
	return n, err
}

func (t *writeTransaction) WriteAt(data []byte, offset int64) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return 0, &Error{Code: CodeConflict, Op: "write", Path: t.path, Err: errors.New("transaction is finished")}
	}
	if offset < 0 {
		return 0, &Error{Code: CodeInvalid, Op: "write", Path: t.path, Err: errors.New("negative offset")}
	}
	n, err := t.file.WriteAt(data, offset)
	return n, classify("write", t.path, err)
}

func (t *writeTransaction) Truncate(size int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return &Error{Code: CodeConflict, Op: "truncate", Path: t.path, Err: errors.New("transaction is finished")}
	}
	if size < 0 {
		return &Error{Code: CodeInvalid, Op: "truncate", Path: t.path, Err: errors.New("negative size")}
	}
	return classify("truncate", t.path, t.file.Truncate(size))
}

func (t *writeTransaction) Allocate(offset, size int64, mode uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return &Error{Code: CodeConflict, Op: "allocate", Path: t.path, Err: errors.New("transaction is finished")}
	}
	if offset < 0 || size < 0 {
		return &Error{Code: CodeInvalid, Op: "allocate", Path: t.path, Err: errors.New("negative range")}
	}
	if mode != 0 {
		return &Error{Code: CodeNotSupported, Op: "allocate", Path: t.path, Err: errors.New("allocation mode is not supported")}
	}
	info, err := t.file.Stat()
	if err != nil {
		return classify("allocate", t.path, err)
	}
	if end := offset + size; end > info.Size() {
		err = t.file.Truncate(end)
	}
	return classify("allocate", t.path, err)
}

func (t *writeTransaction) SetAttributes(changes AttributeChanges) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return &Error{Code: CodeConflict, Op: "set write attributes", Path: t.path, Err: errors.New("transaction is finished")}
	}
	if changes.Size != nil {
		if *changes.Size < 0 {
			return &Error{Code: CodeInvalid, Op: "set write attributes", Path: t.path, Err: errors.New("negative size")}
		}
		if err := t.file.Truncate(*changes.Size); err != nil {
			return classify("set write attributes", t.path, err)
		}
	}
	if changes.Mode != nil {
		if err := t.file.Chmod(os.FileMode(*changes.Mode)); err != nil {
			return classify("set write attributes", t.path, err)
		}
	}
	if changes.UID != nil || changes.GID != nil {
		uid, gid := -1, -1
		if changes.UID != nil {
			uid = int(*changes.UID)
		}
		if changes.GID != nil {
			gid = int(*changes.GID)
		}
		if err := t.file.Chown(uid, gid); err != nil {
			return classify("set write attributes", t.path, err)
		}
	}
	if changes.Accessed != nil || changes.Modified != nil {
		info, err := t.file.Stat()
		if err != nil {
			return classify("set write attributes", t.path, err)
		}
		accessed, modified := info.ModTime(), info.ModTime()
		if changes.Accessed != nil {
			accessed = *changes.Accessed
		}
		if changes.Modified != nil {
			modified = *changes.Modified
		}
		if err := os.Chtimes(t.file.Name(), accessed, modified); err != nil {
			return classify("set write attributes", t.path, err)
		}
	}
	return nil
}

func (t *writeTransaction) Rename(path string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return &Error{Code: CodeConflict, Op: "rename write", Path: t.path, Err: errors.New("transaction is finished")}
	}
	cleaned, destination, err := t.service.resolve(path)
	if err != nil {
		return err
	}
	t.path, t.destination = cleaned, destination
	return nil
}

func (t *writeTransaction) Commit(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return &Error{Code: CodeConflict, Op: "commit", Path: t.path, Err: errors.New("transaction is finished")}
	}
	if err := ctx.Err(); err != nil {
		return classify("commit", t.path, err)
	}
	if err := t.file.Sync(); err != nil {
		return classify("commit", t.path, err)
	}
	if err := t.file.Close(); err != nil {
		return classify("commit", t.path, err)
	}
	t.finished = true
	err := t.service.repo.WithWorkTreeLock(func() error {
		if err := os.Rename(t.file.Name(), t.destination); err != nil {
			return err
		}
		return syncDirectory(filepath.Dir(t.destination))
	})
	if err != nil {
		publishErr := err
		if quarantineErr := t.quarantine(); quarantineErr != nil {
			publishErr = errors.Join(publishErr, fmt.Errorf("quarantine staged write: %w", quarantineErr))
		}
		err = classify("commit", t.path, publishErr)
	} else if t.service.repo.Store != nil {
		if info, statErr := os.Stat(t.destination); statErr != nil {
			err = classify("commit metadata", t.path, statErr)
		} else {
			attributes := attributesFromInfo(info)
			metadata := store.FileMetadata{Mode: attributes.Mode, UID: attributes.UID, GID: attributes.GID,
				AtimeNS: attributes.Accessed.UnixNano(), MtimeNS: attributes.Modified.UnixNano(), CtimeNS: attributes.Changed.UnixNano()}
			if saveErr := t.service.repo.Store.SaveFileMetadata(t.path, metadata); saveErr != nil {
				err = classify("commit metadata", t.path, saveErr)
			} else {
				t.service.metadataMu.Lock()
				t.service.metadata[t.path] = metadataCacheEntry{value: metadata, found: true}
				t.service.metadataMu.Unlock()
				t.service.markFileState(t.path)
			}
		}
	}
	if t.operationID != "" {
		t.service.operationMu.Lock()
		delete(t.service.pendingOperations, t.operationID)
		t.service.operations.Store(t.operationID, operationResult{fingerprint: t.fingerprint, err: err})
		t.service.operationMu.Unlock()
	}
	if err == nil {
		t.service.events.publish("write", t.operationID, t.path)
	}
	return err
}

func (t *writeTransaction) quarantine() error {
	directory := filepath.Join(t.service.root, ".git", "dfs", "recovery", time.Now().UTC().Format("20060102T150405.000000000Z"), "writes")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	destination := filepath.Join(directory, filepath.Base(t.file.Name()))
	if err := os.Rename(t.file.Name(), destination); err != nil {
		return err
	}
	if err := syncDirectory(directory); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(t.file.Name()))
}

func (t *writeTransaction) Abort(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}
	t.finished = true
	err := errors.Join(t.file.Close(), os.Remove(t.file.Name()))
	t.service.releasePending(t.operationID, t.fingerprint)
	return classify("abort", t.path, err)
}

func (t *writeTransaction) Close() error { return t.Abort(context.Background()) }

type replayWrite struct{ result error }

func (t *replayWrite) ReadAt([]byte, int64) (int, error)         { return 0, io.EOF }
func (t *replayWrite) WriteAt(data []byte, _ int64) (int, error) { return len(data), nil }
func (t *replayWrite) Truncate(int64) error                      { return nil }
func (t *replayWrite) Allocate(int64, int64, uint32) error       { return nil }
func (t *replayWrite) SetAttributes(AttributeChanges) error      { return nil }
func (t *replayWrite) Rename(string) error                       { return nil }
func (t *replayWrite) Commit(context.Context) error              { return t.result }
func (t *replayWrite) Abort(context.Context) error               { return nil }
func (t *replayWrite) Close() error                              { return nil }

func (s *Service) releasePending(operationID, fingerprint string) {
	if operationID == "" {
		return
	}
	s.operationMu.Lock()
	if s.pendingOperations[operationID] == fingerprint {
		delete(s.pendingOperations, operationID)
	}
	s.operationMu.Unlock()
}
