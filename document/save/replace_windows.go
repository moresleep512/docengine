//go:build windows

package save

import (
	"errors"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

const replaceFileWriteThrough = 1

const (
	replaceFileAttempts     = 8
	replaceFileInitialDelay = time.Millisecond
)

type replaceCall func(replaced, replacement, backup *uint16, flags uint32, exclude, reserved uintptr) (uintptr, error)

func replaceFile(from, to string) error {
	return replaceFileWithCall(from, to, func(toPtr, fromPtr, _ *uint16, flags uint32, exclude, reserved uintptr) (uintptr, error) {
		result, _, callErr := replaceFileW.Call(
			uintptr(unsafe.Pointer(toPtr)),
			uintptr(unsafe.Pointer(fromPtr)),
			0,
			uintptr(flags),
			exclude,
			reserved,
		)
		return result, callErr
	})
}

func replaceFileWithCall(from, to string, call replaceCall) error {
	return replaceFileWithCallAndWait(from, to, call, time.Sleep)
}

func replaceFileWithCallAndWait(from, to string, call replaceCall, wait func(time.Duration)) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	// ReplaceFileW preserves replacement semantics and works while readers hold
	// the original with FILE_SHARE_DELETE. The first argument is the file being
	// replaced; the second is the fully-written temporary replacement.
	delay := replaceFileInitialDelay
	for attempt := 0; ; attempt++ {
		result, callErr := call(toPtr, fromPtr, nil, replaceFileWriteThrough, 0, 0)
		if result != 0 {
			return nil
		}
		if callErr == syscall.Errno(0) {
			callErr = syscall.EINVAL
		}
		if attempt == replaceFileAttempts-1 || !isRetryableReplaceError(callErr) {
			return callErr
		}
		wait(delay)
		delay *= 2
	}
}

func isRetryableReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_UNABLE_TO_REMOVE_REPLACED) ||
		errors.Is(err, windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT) ||
		errors.Is(err, windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2)
}

func syncParent(string) error { return nil }
