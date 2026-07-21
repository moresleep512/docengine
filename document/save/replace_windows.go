//go:build windows

package save

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

const replaceFileWriteThrough = 1

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
	result, callErr := call(toPtr, fromPtr, nil, replaceFileWriteThrough, 0, 0)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return syscall.EINVAL
	}
	return nil
}

func syncParent(string) error { return nil }
