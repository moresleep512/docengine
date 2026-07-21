//go:build windows

package save

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestReplaceFileWithCallValidationAndResults(t *testing.T) {
	called := false
	call := func(_, _, backup *uint16, flags uint32, exclude, reserved uintptr) (uintptr, error) {
		called = true
		if backup != nil || flags != replaceFileWriteThrough || exclude != 0 || reserved != 0 {
			t.Fatalf("ReplaceFileW arguments = (backup=%v flags=%d exclude=%d reserved=%d)", backup, flags, exclude, reserved)
		}
		return 1, nil
	}
	if err := replaceFileWithCall("bad\x00from", "to", call); err == nil || called {
		t.Fatalf("invalid from = %v, called=%v", err, called)
	}
	if err := replaceFileWithCall("from", "bad\x00to", call); err == nil || called {
		t.Fatalf("invalid to = %v, called=%v", err, called)
	}
	if err := replaceFileWithCall("from", "to", call); err != nil || !called {
		t.Fatalf("success = %v, called=%v", err, called)
	}
	sentinel := errors.New("replace failed")
	if err := replaceFileWithCall("from", "to", func(*uint16, *uint16, *uint16, uint32, uintptr, uintptr) (uintptr, error) { return 0, sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("call error = %v", err)
	}
	if err := replaceFileWithCall("from", "to", func(*uint16, *uint16, *uint16, uint32, uintptr, uintptr) (uintptr, error) { return 0, syscall.Errno(0) }); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("zero call error = %v", err)
	}
}

func TestReplaceFileRetriesOnlyTransientWindowsErrors(t *testing.T) {
	retryable := []error{
		windows.ERROR_SHARING_VIOLATION,
		windows.ERROR_LOCK_VIOLATION,
		windows.ERROR_UNABLE_TO_REMOVE_REPLACED,
		windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT,
		windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2,
	}
	for _, err := range retryable {
		if !isRetryableReplaceError(err) {
			t.Fatalf("%v is not retryable", err)
		}
	}
	if isRetryableReplaceError(syscall.EACCES) || isRetryableReplaceError(errors.New("permanent")) {
		t.Fatal("permanent error marked retryable")
	}

	calls := 0
	var waits []time.Duration
	err := replaceFileWithCallAndWait("from", "to", func(*uint16, *uint16, *uint16, uint32, uintptr, uintptr) (uintptr, error) {
		calls++
		if calls < 3 {
			return 0, windows.ERROR_UNABLE_TO_REMOVE_REPLACED
		}
		return 1, nil
	}, func(delay time.Duration) { waits = append(waits, delay) })
	if err != nil || calls != 3 || len(waits) != 2 || waits[0] != time.Millisecond || waits[1] != 2*time.Millisecond {
		t.Fatalf("retry success = (err=%v calls=%d waits=%v)", err, calls, waits)
	}

	calls, waits = 0, nil
	err = replaceFileWithCallAndWait("from", "to", func(*uint16, *uint16, *uint16, uint32, uintptr, uintptr) (uintptr, error) {
		calls++
		return 0, windows.ERROR_LOCK_VIOLATION
	}, func(delay time.Duration) { waits = append(waits, delay) })
	if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) || calls != replaceFileAttempts || len(waits) != replaceFileAttempts-1 {
		t.Fatalf("retry exhaustion = (err=%v calls=%d waits=%v)", err, calls, waits)
	}
}
