//go:build windows

package save

import (
	"errors"
	"syscall"
	"testing"
)

func TestReplaceFileWithCallValidationAndResults(t *testing.T) {
	called := false
	call := func(*uint16, *uint16) (uintptr, error) { called = true; return 1, nil }
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
	if err := replaceFileWithCall("from", "to", func(*uint16, *uint16) (uintptr, error) { return 0, sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("call error = %v", err)
	}
	if err := replaceFileWithCall("from", "to", func(*uint16, *uint16) (uintptr, error) { return 0, syscall.Errno(0) }); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("zero call error = %v", err)
	}
}
