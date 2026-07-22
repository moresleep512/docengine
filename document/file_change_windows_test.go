//go:build windows

package document

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestQueryWindowsFileChangeSuccessAndFailure(t *testing.T) {
	stamp, err := queryWindowsFileChange(0, func(_ windows.Handle, class uint32, buffer *byte, size uint32) error {
		if class != windows.FileBasicInfo || size != uint32(unsafe.Sizeof(windowsFileBasicInformation{})) {
			t.Fatalf("query = (class %d, size %d)", class, size)
		}
		(*windowsFileBasicInformation)(unsafe.Pointer(buffer)).changeTime = 42
		return nil
	})
	if err != nil || stamp != (fileChangeStamp{first: 42, available: true}) {
		t.Fatalf("queryWindowsFileChange = (%+v, %v)", stamp, err)
	}
	sentinel := errors.New("file basic information")
	if _, err := queryWindowsFileChange(0, func(windows.Handle, uint32, *byte, uint32) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("queryWindowsFileChange error = %v", err)
	}
}
