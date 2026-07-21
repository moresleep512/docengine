package save

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// TestPropertyAtomicPreservesContentForRandomSizes writes a random content of
// many lengths and a random BOM-like prefix through Atomic and verifies the
// on-disk bytes are exactly prefix+content and no save temp files remain. The
// file mode is intentionally not asserted: on Windows the executable bit is
// never preserved, so the existing tests avoid the assertion as well.
func TestPropertyAtomicPreservesContentForRandomSizes(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 7))
	for iteration := 0; iteration < 60; iteration++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		content := make([]byte, rng.IntN(4096))
		for i := range content {
			content[i] = byte(rng.IntN(256))
		}
		prefix := make([]byte, rng.IntN(8))
		for i := range prefix {
			prefix[i] = byte(rng.IntN(256))
		}
		writeContent := func(writer io.Writer) (int64, error) {
			n, err := writer.Write(content)
			return int64(n), err
		}
		total, err := Atomic(path, 0o600, prefix, writeContent)
		if err != nil {
			t.Fatalf("iteration %d: Atomic = %v", iteration, err)
		}
		if total != int64(len(prefix)+len(content)) {
			t.Fatalf("iteration %d: total = %d, want %d", iteration, total, len(prefix)+len(content))
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]byte(nil), prefix...), content...)
		if !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: content mismatch", iteration)
		}
		assertNoSaveTemps(t, dir)
	}
}

// TestPropertyRepeatedAtomicReplacesCleanly verifies that a sequence of
// successful Atomic calls to the same path always reflects the latest content
// and never leaves behind save temp files.
func TestPropertyRepeatedAtomicReplacesCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(17, 19))
	for iteration := 0; iteration < 30; iteration++ {
		content := []byte{byte('a' + rng.IntN(26)), byte('A' + rng.IntN(26))}
		if _, err := Atomic(path, 0o600, nil, func(w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		}); err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("iteration %d: got %q, want %q", iteration, got, content)
		}
		assertNoSaveTemps(t, dir)
	}
}

// TestPropertySyncParentIdempotent verifies that calling SyncParent repeatedly
// after a successful Atomic does not corrupt the content or change the file
// size, across many content lengths.
func TestPropertySyncParentIdempotent(t *testing.T) {
	rng := rand.New(rand.NewPCG(23, 29))
	for iteration := 0; iteration < 40; iteration++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		// Pre-create the target: on Windows ReplaceFileW requires the file being
		// replaced to already exist.
		if err := os.WriteFile(path, []byte("seed"), 0o600); err != nil {
			t.Fatal(err)
		}
		content := make([]byte, rng.IntN(1024))
		for i := range content {
			content[i] = byte(rng.IntN(256))
		}
		if _, err := Atomic(path, 0o600, nil, func(w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		}); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sizeBefore := info.Size()
		for sync := 0; sync < 3; sync++ {
			if err := SyncParent(path); err != nil {
				t.Fatalf("iteration %d sync %d: %v", iteration, sync, err)
			}
			again, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, content) {
				t.Fatalf("iteration %d sync %d corrupted content", iteration, sync)
			}
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if after.Size() != sizeBefore {
			t.Fatalf("iteration %d: size changed %d -> %d", iteration, sizeBefore, after.Size())
		}
	}
}

// TestPropertyDurabilityErrorAlwaysCommitted verifies that every DurabilityError,
// regardless of the wrapped cause, reports Committed() == true and is
// recognized by errors.As. This is the load-bearing contract that callers rely
// on to avoid treating a committed write as a failure.
func TestPropertyDurabilityErrorAlwaysCommitted(t *testing.T) {
	causes := []error{
		errors.New("directory sync failed"),
		errors.New("power loss mid-rename"),
		os.ErrPermission,
		io.ErrShortWrite,
		nil,
	}
	for index, cause := range causes {
		err := &DurabilityError{Path: "doc", Err: cause}
		if !err.Committed() {
			t.Fatalf("DurabilityError %d is not committed", index)
		}
		var target *DurabilityError
		if !errors.As(err, &target) || target != err {
			t.Fatalf("DurabilityError %d errors.As failed", index)
		}
		if cause != nil && !errors.Is(err, cause) {
			t.Fatalf("DurabilityError %d does not unwrap to its cause", index)
		}
	}
}

// TestPropertyAtomicCheckedConflictPreservesOriginal verifies that a conflict
// detected by the beforeReplace check leaves the original file byte-identical
// and removes the temporary file, across many original-content shapes.
func TestPropertyAtomicCheckedConflictPreservesOriginal(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 37))
	conflict := errors.New("concurrent modification")
	for iteration := 0; iteration < 40; iteration++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		original := make([]byte, rng.IntN(512))
		for i := range original {
			original[i] = byte(rng.IntN(256))
		}
		if err := os.WriteFile(path, original, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := AtomicChecked(path, 0o600, []byte("p"), func(w io.Writer) (int64, error) {
			n, wErr := w.Write([]byte("new"))
			return int64(n), wErr
		}, func() error { return conflict })
		if !errors.Is(err, conflict) {
			t.Fatalf("iteration %d: error = %v, want %v", iteration, err, conflict)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, original) {
			t.Fatalf("iteration %d: original mutated on conflict", iteration)
		}
		assertNoSaveTemps(t, dir)
	}
}
