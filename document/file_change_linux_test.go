//go:build linux

package document

import (
	"os"
	"testing"
)

type opaqueSystemFileInfo struct{ os.FileInfo }

func (opaqueSystemFileInfo) Sys() any { return nil }

func TestCaptureFileChangeWithoutSystemMetadata(t *testing.T) {
	info, err := os.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := captureFileChange(nil, opaqueSystemFileInfo{FileInfo: info})
	if err != nil || stamp != (fileChangeStamp{}) {
		t.Fatalf("captureFileChange = (%+v, %v)", stamp, err)
	}
}
