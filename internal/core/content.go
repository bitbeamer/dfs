package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func annexLinkTarget(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(path)
	if err != nil {
		return "", false
	}
	normalized := filepath.ToSlash(target)
	return normalized, strings.Contains(normalized, "/.git/annex/objects/") || strings.HasPrefix(normalized, ".git/annex/objects/")
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

func (s *Service) OpenRead(ctx context.Context, path string) (ReadHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify("open read", path, err)
	}
	cleaned, full, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	target, annexed := annexLinkTarget(full)
	handle, openErr := os.Open(full)
	if openErr == nil {
		info, statErr := handle.Stat()
		if statErr != nil {
			_ = handle.Close()
			return nil, classify("open read", cleaned, statErr)
		}
		if s.repo.Store != nil {
			s.repo.Touch(cleaned)
		}
		return &localReadHandle{File: handle, size: info.Size()}, nil
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
		return &rangeReadHandle{service: s, path: cleaned, key: filepath.Base(filepath.FromSlash(target)), size: size, ctx: readCtx, cancel: cancel}, nil
	}
	if err := s.repo.Fetch(ctx, cleaned, ""); err != nil {
		return nil, classify("hydrate", cleaned, err)
	}
	handle, err = os.Open(full)
	if err != nil {
		return nil, classify("open read", cleaned, err)
	}
	info, err := handle.Stat()
	if err != nil {
		_ = handle.Close()
		return nil, classify("open read", cleaned, err)
	}
	if s.repo.Store != nil {
		s.repo.Touch(cleaned)
	}
	return &localReadHandle{File: handle, size: info.Size()}, nil
}

type localReadHandle struct {
	*os.File
	size int64
}

func (h *localReadHandle) Size() int64                     { return h.size }
func (h *localReadHandle) FileDescriptor() (uintptr, bool) { return h.Fd(), true }

type rangeReadHandle struct {
	service *Service
	path    string
	key     string
	size    int64
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
}

func (h *rangeReadHandle) Size() int64                     { return h.size }
func (h *rangeReadHandle) FileDescriptor() (uintptr, bool) { return 0, false }

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
		if err := os.MkdirAll(filepath.Dir(t.destination), 0o755); err != nil {
			return err
		}
		return os.Rename(t.file.Name(), t.destination)
	})
	if err != nil {
		_ = os.Remove(t.file.Name())
		err = classify("commit", t.path, err)
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

func (t *replayWrite) WriteAt(data []byte, _ int64) (int, error) { return len(data), nil }
func (t *replayWrite) Truncate(int64) error                      { return nil }
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
