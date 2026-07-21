//go:build windows

package document

import (
	"errors"
	"testing"
	"unicode/utf16"
)

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

func TestResolvePathWithCallUNCResizeAndFailure(t *testing.T) {
	calls := 0
	resolved, err := resolvePathWithCall(func(buffer []uint16) (uint32, error) {
		calls++
		value := utf16.Encode([]rune(`\\?\UNC\server\share\doc`))
		if len(buffer) <= len(value) {
			return uint32(len(value)), nil
		}
		copy(buffer, value)
		return uint32(len(value)), nil
	}, 1)
	if err != nil || resolved != `\\server\share\doc` || calls != 2 {
		t.Fatalf("resolve = (%q, %v), calls=%d", resolved, err, calls)
	}
	sentinel := errors.New("injected")
	if _, err := resolvePathWithCall(func([]uint16) (uint32, error) { return 0, sentinel }, 1); !errors.Is(err, sentinel) {
		t.Fatalf("call error = %v", err)
	}
}
