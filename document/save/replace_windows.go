//go:build windows

package save

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func replaceFile(from, to string) error {
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
	result, _, callErr := replaceFileW.Call(
		uintptr(unsafe.Pointer(toPtr)),
		uintptr(unsafe.Pointer(fromPtr)),
		0,
		1, // REPLACEFILE_WRITE_THROUGH
		0,
		0,
	)
	if result == 0 {
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return syscall.EINVAL
	}
	return nil
}
