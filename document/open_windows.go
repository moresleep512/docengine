//go:build windows

package document

import (
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"strings"
)

// openBase includes FILE_SHARE_DELETE so a same-directory atomic replacement
// can commit while immutable snapshots are still reading the old file handle.
func openBase(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_RANDOM_ACCESS,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func resolvePath(path string) (string, error) {
	file, err := openBase(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return resolvePathWithCall(func(buffer []uint16) (uint32, error) {
		return windows.GetFinalPathNameByHandle(windows.Handle(file.Fd()), &buffer[0], uint32(len(buffer)), 0)
	}, 512)
}

func resolvePathWithCall(call func([]uint16) (uint32, error), initialSize int) (string, error) {
	buffer := make([]uint16, initialSize)
	for {
		n, callErr := call(buffer)
		if callErr != nil {
			return "", callErr
		}
		if int(n) < len(buffer) {
			resolved := windows.UTF16ToString(buffer[:n])
			if strings.HasPrefix(resolved, `\\?\UNC\`) {
				resolved = `\\` + strings.TrimPrefix(resolved, `\\?\UNC\`)
			} else {
				resolved = strings.TrimPrefix(resolved, `\\?\`)
			}
			return filepath.Clean(resolved), nil
		}
		buffer = make([]uint16, int(n)+1)
	}
}
