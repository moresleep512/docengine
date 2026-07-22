//go:build windows

package document

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsFileBasicInformation struct {
	creationTime, lastAccessTime, lastWriteTime, changeTime int64
	fileAttributes                                          uint32
	padding                                                 uint32
}

type windowsFileInformationQuery func(windows.Handle, uint32, *byte, uint32) error

func captureFileChange(file readStatFile, _ os.FileInfo) (fileChangeStamp, error) {
	descriptor, ok := file.(interface{ Fd() uintptr })
	if !ok {
		return fileChangeStamp{}, nil
	}
	return queryWindowsFileChange(windows.Handle(descriptor.Fd()), windows.GetFileInformationByHandleEx)
}

func queryWindowsFileChange(handle windows.Handle, query windowsFileInformationQuery) (fileChangeStamp, error) {
	var information windowsFileBasicInformation
	if err := query(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information))); err != nil {
		return fileChangeStamp{}, err
	}
	return fileChangeStamp{first: information.changeTime, available: true}, nil
}
