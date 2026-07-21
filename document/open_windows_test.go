//go:build windows

package document

import "testing"

func TestOpenBaseRejectsInvalidAndMissingPaths(t *testing.T) {
	if _, err := openBase("bad\x00path"); err == nil {
		t.Fatal("expected UTF-16 path conversion error")
	}
	if _, err := openBase(t.TempDir() + "\\missing"); err == nil {
		t.Fatal("expected CreateFile error")
	}
}

func TestOpenRejectsWindowsCharacterDevice(t *testing.T) {
	if _, err := Open(`\\.\NUL`, OpenOptions{}); err == nil {
		t.Fatal("expected non-regular-file error")
	}
}
