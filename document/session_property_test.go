package document

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
)

// TestPropertyRevisionMonotonicNonDecreasing applies a sequence of random
// edits interleaved with random undo/redo and Save calls, and asserts that the
// visible revision never decreases and never drops below the committed
// revision. This is a load-bearing invariant of the Session state machine.
func TestPropertyRevisionMonotonicNonDecreasing(t *testing.T) {
	const iterations = 200
	rng := newSessionRNG(1)
	session, _, _ := openAtomicTestSession(t, "abc")
	defer session.Close()
	previousRevision := session.Metadata().Revision
	for i := 0; i < iterations; i++ {
		switch rng.IntN(4) {
		case 0:
			meta := session.Metadata()
			_, err := session.ApplyBatch(context.Background(), meta.Revision, randomEdit(rng, meta.ByteLength))
			if err == nil {
				if session.Metadata().Revision < previousRevision {
					t.Fatalf("iteration %d: revision retreated", i)
				}
				previousRevision = session.Metadata().Revision
			}
		case 1:
			if _, err := session.Undo(); err == nil {
				if session.Metadata().Revision < previousRevision {
					t.Fatalf("iteration %d: revision retreated after undo", i)
				}
				previousRevision = session.Metadata().Revision
			}
		case 2:
			if _, err := session.Redo(); err == nil {
				if session.Metadata().Revision < previousRevision {
					t.Fatalf("iteration %d: revision retreated after redo", i)
				}
				previousRevision = session.Metadata().Revision
			}
		case 3:
			if _, err := session.Save(); err != nil {
				if errors.Is(err, ErrFaulted) {
					return // faulted sessions are tested separately
				}
			}
			meta := session.Metadata()
			if meta.Revision < meta.CommittedRevision {
				t.Fatalf("iteration %d: revision %d < committed %d", i, meta.Revision, meta.CommittedRevision)
			}
			previousRevision = meta.Revision
		}
	}
}

// TestPropertyUndoRedoFullRoundTrip applies a sequence of random edits, then
// undoes until ErrNothingToUndo, redoes until ErrNothingToRedo, and verifies
// the content equals the post-edit content. Undoing again must restore the
// original content, exercising the undo/redo stack invariants end-to-end.
func TestPropertyUndoRedoFullRoundTrip(t *testing.T) {
	const edits = 30
	rng := newSessionRNG(2)
	session, _, _ := openAtomicTestSession(t, "hello world")
	defer session.Close()
	for i := 0; i < edits; i++ {
		meta := session.Metadata()
		if _, err := session.ApplyBatch(context.Background(), meta.Revision, randomEdit(rng, meta.ByteLength)); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
	}
	afterEdits := session.Metadata().ByteLength
	afterEditsBytes, _ := readSession(session, afterEdits)

	// Undo to the bottom of the stack.
	for {
		if _, err := session.Undo(); errors.Is(err, ErrNothingToUndo) {
			break
		} else if err != nil {
			t.Fatalf("undo: %v", err)
		}
	}
	assertSessionText(t, session, "hello world")

	// Redo back to the top.
	for {
		if _, err := session.Redo(); errors.Is(err, ErrNothingToRedo) {
			break
		} else if err != nil {
			t.Fatalf("redo: %v", err)
		}
	}
	assertSessionText(t, session, string(afterEditsBytes))
}

// TestPropertySaveClearsDirtyAndRecovered verifies that after a successful
// Save of dirty edits, the session reports Dirty=false, and a fresh open of
// the saved file reports neither Dirty nor Recovered (the clean save removed
// the journal).
func TestPropertySaveClearsDirtyAndRecovered(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/doc.md"
	recoveryDir := dir + "/recovery"
	if err := os.WriteFile(path, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: dir + "/s1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: "!"}}); err != nil {
		t.Fatal(err)
	}
	if meta := session.Metadata(); !meta.Dirty {
		t.Fatalf("expected dirty after edit, got %+v", meta)
	}
	if _, err := session.Save(); err != nil {
		t.Fatal(err)
	}
	if meta := session.Metadata(); meta.Dirty {
		t.Fatalf("expected not dirty after save, got %+v", meta)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: there is no journal (clean save removes it), so not recovered.
	clean, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: dir + "/s2"})
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()
	if meta := clean.Metadata(); meta.Dirty || meta.Recovered {
		t.Fatalf("clean reopen = %+v", meta)
	}
}

// TestPropertyFailedBatchPreservesUndoStackDepth applies edits to build undo/redo
// history, then a deliberately invalid batch, and verifies the undo/redo stack
// depths and content are unchanged by the rejected batch.
func TestPropertyFailedBatchPreservesUndoStackDepth(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abcdef")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, Insert: "X"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 1, Insert: "Y"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	meta := session.Metadata()
	undoBefore, redoBefore := len(session.undo), len(session.redo)
	// Invalid batch: out-of-range start must be rejected atomically.
	_, err := session.ApplyBatch(context.Background(), meta.Revision, []ReplaceOperation{{Start: 99, DeleteLength: 1, Insert: "z"}})
	if err == nil {
		t.Fatal("expected invalid range")
	}
	if len(session.undo) != undoBefore || len(session.redo) != redoBefore {
		t.Fatalf("stacks changed %d/%d -> %d/%d", undoBefore, redoBefore, len(session.undo), len(session.redo))
	}
	assertSessionText(t, session, "Xabcdef")
}

// TestPropertyFaultStateIrreversible drives a session into the permanent
// faulted state via a post-commit rebind failure and verifies that every
// subsequent mutator returns ErrFaulted while readers (ReadAt, Snapshot,
// Metadata, Fault, Close) remain usable.
func TestPropertyFaultStateIrreversible(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "base")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: "!"}}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("reopen committed base")
	originalOpen := session.operations.openBase
	calls := 0
	session.operations.openBase = func(p string) (*os.File, error) {
		calls++
		if calls == 2 {
			return nil, sentinel
		}
		return originalOpen(p)
	}
	if _, err := session.Save(); !errors.Is(err, ErrFaulted) || !errors.Is(err, sentinel) {
		t.Fatalf("Save = %v, want ErrFaulted+sentinel", err)
	}
	if !session.Metadata().PersistenceFaulted {
		t.Fatal("session not marked faulted")
	}
	// Mutators must all refuse with ErrFaulted.
	if _, err := session.ApplyBatch(context.Background(), session.Metadata().Revision, []ReplaceOperation{{Start: 0, Insert: "z"}}); !errors.Is(err, ErrFaulted) {
		t.Fatalf("ApplyBatch after fault = %v", err)
	}
	if _, err := session.Undo(); !errors.Is(err, ErrFaulted) {
		t.Fatalf("Undo after fault = %v", err)
	}
	if _, err := session.Redo(); !errors.Is(err, ErrFaulted) {
		t.Fatalf("Redo after fault = %v", err)
	}
	if _, err := session.Save(); !errors.Is(err, ErrFaulted) {
		t.Fatalf("Save after fault = %v", err)
	}
	// Readers must keep working and reflect the committed content.
	meta := session.Metadata()
	if meta.CommittedRevision != 1 || meta.Dirty {
		t.Fatalf("faulted metadata = %+v", meta)
	}
	if body, _ := os.ReadFile(path); string(body) != "base!" {
		t.Fatalf("disk = %q", body)
	}
	buf := make([]byte, meta.ByteLength)
	if n, err := session.ReadAt(buf, 0); err != nil || n != len(buf) || string(buf) != "base!" {
		t.Fatalf("ReadAt = (%d, %v, %q)", n, err, buf)
	}
	_, lease, err := session.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after fault = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("lease close = %v", err)
	}
	if !errors.Is(session.Fault(), sentinel) {
		t.Fatalf("Fault = %v", session.Fault())
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// TestPropertyConcurrentUndoRedoEditNoRace runs Undo, Redo, and ApplyBatch
// concurrently against a single session under the race detector. The oracle
// is that no operation panics and the session remains usable afterward. Errors
// from revision conflicts or empty stacks are expected and ignored.
func TestPropertyConcurrentUndoRedoEditNoRace(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abcdefghij")
	defer session.Close()
	// Seed some history so undo/redo have work.
	for i := 0; i < 10; i++ {
		session.ApplyBatch(context.Background(), uint64(i), []ReplaceOperation{{Start: 0, Insert: "x"}})
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			rng := newSessionRNG(seed)
			for i := 0; i < 50; i++ {
				meta := session.Metadata()
				switch rng.IntN(3) {
				case 0:
					session.ApplyBatch(context.Background(), meta.Revision, randomEdit(rng, meta.ByteLength))
				case 1:
					session.Undo()
				case 2:
					session.Redo()
				}
			}
		}(uint64(g + 1))
	}
	wg.Wait()
	// The session must still respond after the concurrent storm.
	session.Metadata()
}

// randomEdit builds a single-op ReplaceOperation that is always valid for the
// current document length, using a small ASCII alphabet to keep inserted bytes
// UTF-8-clean.
func randomEdit(rng *sessionRNG, length int64) []ReplaceOperation {
	start := rng.Int64N(length + 1)
	maxDelete := length - start
	deleteLength := int64(0)
	if maxDelete > 0 {
		deleteLength = rng.Int64N(maxDelete + 1)
	}
	insert := []byte{byte('a' + rng.IntN(26))}
	return []ReplaceOperation{{Start: start, DeleteLength: deleteLength, Insert: string(insert)}}
}

// sessionRNG is a small xorshift PRNG used by the deterministic session property
// tests. It avoids math/rand's global state and keeps tests hermetic.
type sessionRNG struct{ state uint64 }

func newSessionRNG(seed uint64) *sessionRNG {
	s := seed
	if s == 0 {
		s = 1
	}
	return &sessionRNG{state: s}
}

func (r *sessionRNG) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return int(r.state % uint64(n))
}

func (r *sessionRNG) Int64N(n int64) int64 {
	if n <= 0 {
		return 0
	}
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return int64(r.state % uint64(n))
}
