package document

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moresleep512/docengine/document/store"
)

func TestSnapshotLeaseClosedBehaviorAndGenerationWait(t *testing.T) {
	tree, err := store.New(bytes.NewReader([]byte("abc")), 3)
	if err != nil {
		t.Fatal(err)
	}
	generation := newSourceGeneration(nil, nil)
	lease := generation.acquire(tree.Snapshot())
	if lease.Len() != 3 {
		t.Fatalf("Len = %d", lease.Len())
	}
	buffer := make([]byte, 3)
	if n, err := lease.ReadAt(buffer, 0); n != 3 || err != nil && !errors.Is(err, io.EOF) || string(buffer) != "abc" {
		t.Fatalf("ReadAt = (%d, %v, %q)", n, err, buffer)
	}
	var output bytes.Buffer
	if n, err := lease.WriteTo(&output); n != 3 || err != nil || output.String() != "abc" {
		t.Fatalf("WriteTo = (%d, %v, %q)", n, err, output.String())
	}
	done := make(chan error, 1)
	go func() { done <- generation.retireAndWait(false) }()
	select {
	case err := <-done:
		t.Fatalf("generation retired before lease release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second lease close: %v", err)
	}
	if _, err := lease.ReadAt(buffer, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed ReadAt error = %v", err)
	}
	if _, err := lease.WriteTo(io.Discard); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed WriteTo error = %v", err)
	}
	if err := generation.retireAndWait(false); err != nil {
		t.Fatalf("second retireAndWait: %v", err)
	}
	if err := generation.release(); err != nil {
		t.Fatalf("extra release: %v", err)
	}
}

func TestGenerationReportsJournalRemovalFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "non-empty")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	generation := newSourceGeneration(nil, nil)
	generation.journalPath = dir
	if err := generation.retireAndWait(true); err == nil {
		t.Fatal("expected non-empty journal-path removal error")
	}
}
