package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

func (e *Error) Error() string {
	location := e.Path
	if location != "" {
		location = " " + location
	}
	if e.Err == nil {
		return fmt.Sprintf("%s%s: %s", e.Op, location, e.Code)
	}
	return fmt.Sprintf("%s%s: %s: %v", e.Op, location, e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func IsErrorCode(err error, code ErrorCode) bool {
	var target *Error
	return errors.As(err, &target) && target.Code == code
}

func classify(op, path string, err error) error {
	if err == nil {
		return nil
	}
	if IsErrorCode(err, CodeInternal) || isCoreError(err) {
		return err
	}
	code := CodeInternal
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = CodeCanceled
	case errors.Is(err, fs.ErrNotExist):
		code = CodeNotFound
	case errors.Is(err, fs.ErrExist):
		code = CodeAlreadyExists
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		code = CodePermission
	case errors.Is(err, syscall.ENOSPC):
		code = CodeNoSpace
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.ENOSYS):
		code = CodeNotSupported
	}
	return &Error{Code: code, Op: op, Path: path, Err: err}
}

func isCoreError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}
