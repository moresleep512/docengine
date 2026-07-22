package document

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUndoStoreLifecycleQuotaAndErrors(t *testing.T) {
	store, err := openUndoStore("", DefaultUndoBytes)
	if err != nil || store != nil {
		t.Fatalf("empty open = (%v, %v)", store, err)
	}
	if _, err := store.append([]byte("x")); !errors.Is(err, errUndoQuota) {
		t.Fatalf("nil append error = %v", err)
	}
	if value, err := store.read(textRef{length: 1}); !errors.Is(err, errUndoQuota) || value != "" {
		t.Fatalf("nil read = (%q, %v)", value, err)
	}
	if err := store.reset(); err != nil {
		t.Fatalf("nil reset: %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("nil close: %v", err)
	}

	dir := t.TempDir()
	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	if ref, err := store.append(nil); err != nil || ref != (textRef{}) {
		t.Fatalf("empty append = (%+v, %v)", ref, err)
	}
	store.quota = 3
	ref, err := store.append([]byte("abc"))
	if err != nil || ref != (textRef{length: 3}) {
		t.Fatalf("append = (%+v, %v)", ref, err)
	}
	if _, err := store.append([]byte("d")); !errors.Is(err, errUndoQuota) {
		t.Fatalf("quota error = %v", err)
	}
	if value, err := store.read(textRef{}); err != nil || value != "" {
		t.Fatalf("empty read = (%q, %v)", value, err)
	}
	if value, err := store.read(ref); err != nil || value != "abc" {
		t.Fatalf("read = (%q, %v)", value, err)
	}
	if _, err := store.read(textRef{offset: 99, length: 1}); err == nil {
		t.Fatal("expected out-of-range read error")
	}
	if err := store.reset(); err != nil || store.size != 0 {
		t.Fatalf("reset = %v, size=%d", err, store.size)
	}
	path := store.path
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undo file remains after close: %v", err)
	}
	if _, err := store.append([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed append error = %v", err)
	}
	if err := store.reset(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed reset error = %v", err)
	}
	if err := store.close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestOpenUndoStoreFilesystemFailures(t *testing.T) {
	parentFile := filepath.Join(t.TempDir(), "parent")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openUndoStore(filepath.Join(parentFile, "session"), DefaultUndoBytes); err == nil {
		t.Fatal("expected MkdirAll error")
	}
	dir := t.TempDir()
	sentinel := errors.New("create temp")
	operations := systemUndoStoreOperations
	operations.createTemp = func(string, string) (*os.File, error) { return nil, sentinel }
	if _, err := openUndoStoreWith(dir, DefaultUndoBytes, operations); !errors.Is(err, sentinel) {
		t.Fatalf("CreateTemp error = %v", err)
	}
}

func TestUndoStoreCleanupErrors(t *testing.T) {
	dir := t.TempDir()
	store, err := openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("remove")
	store.remove = func(string) error { return sentinel }
	if err := store.close(); !errors.Is(err, sentinel) {
		t.Fatalf("remove error = %v", err)
	}

	store, err = openUndoStore(dir, DefaultUndoBytes)
	if err != nil {
		t.Fatal(err)
	}
	store.remove = func(string) error { return os.ErrNotExist }
	if err := store.close(); err != nil {
		t.Fatalf("missing cleanup = %v", err)
	}
}
