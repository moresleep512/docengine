package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FuzzSessionConcurrentSaveEdit randomized the one concurrency blind spot that
// the existing point-scenario tests only exercise with fixed edits: an edit
// applied while a Save is mid-flight (between snapshot capture and atomic
// replace), which forces the post-replace rebase path that copies pending ops
// into a new journal rooted at the saved content.
//
// The driver is deterministic (channel handshake, no sleeps): apply a pre-save
// edit, start Save with a commitHook that blocks at the "snapshot" stage,
// apply a fuzzed set of edits while Save is blocked, release Save, then verify
// the in-memory content, the disk content, and that a reopen recovers to the
// same content and that undoing the rebased batches restores the committed
// content. Run with -race to catch data races on the rebase path.
func FuzzSessionConcurrentSaveEdit(f *testing.F) {
	f.Add([]byte("alpha"), []byte{5, 0, 1, 0, 1, 2, 0, 0, 1})
	f.Add([]byte("a"), []byte{0, 1, 0, 0, 1, 0, 1, 0, 0, 1, 0, 1, 0, 1})
	f.Add([]byte("hello world"), []byte{1, 0, 2, 0, 3, 0, 0, 1})
	f.Fuzz(func(t *testing.T, base, program []byte) {
		const maxBaseBytes = 64
		base = sanitizeBase(base, maxBaseBytes)
		if len(base) == 0 {
			base = []byte("seed")
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "doc.md")
		recoveryDir := filepath.Join(dir, "recovery")
		if err := os.WriteFile(path, base, 0o600); err != nil {
			t.Fatal(err)
		}
		session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "s1")})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}

		// Apply a pre-save edit E0 to make the session dirty.
		pos := 0
		readByte := func() (byte, bool) {
			if pos >= len(program) {
				return 0, false
			}
			b := program[pos]
			pos++
			return b, true
		}
		nextEdit := func(current []byte) (start, deleteLength int64, insert []byte, ok bool) {
			sb, ok1 := readByte()
			db, ok2 := readByte()
			ib, ok3 := readByte()
			if !ok1 || !ok2 || !ok3 {
				return 0, 0, nil, false
			}
			start = int64(sb) % (int64(len(current)) + 1)
			maxDelete := int64(len(current)) - start
			deleteLength = int64(db) % (maxDelete + 1)
			insertLength := int64(ib) % 4
			insert = make([]byte, insertLength)
			for i := range insert {
				insert[i] = byte('a' + (int(ib)+i)%26)
			}
			return start, deleteLength, insert, true
		}
		applyEdit := func(current []byte) ([]byte, bool) {
			start, del, ins, ok := nextEdit(current)
			if !ok {
				return current, false
			}
			meta := session.Metadata()
			if _, err := session.ApplyBatch(context.Background(), meta.Revision, []ReplaceOperation{{
				Start: start, DeleteLength: del, Insert: string(ins),
			}}); err != nil {
				t.Fatalf("ApplyBatch: %v", err)
			}
			return spliceBytes(current, int(start), int(del), ins), true
		}

		// E0: a pre-save edit that makes the session dirty. A clean session
		// takes the Save fast path and never reaches the commitHook, which
		// would deadlock the <-started handshake; if there are not enough
		// program bytes for E0 there is nothing to exercise.
		reference := append([]byte(nil), base...)
		var applied bool
		reference, applied = applyEdit(reference)
		if !applied {
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			return
		}
		committedReference := append([]byte(nil), reference...)

		// Start Save, blocked at the snapshot stage.
		started, proceed := make(chan struct{}), make(chan struct{})
		session.commitHook = func(stage string) {
			if stage == "snapshot" {
				close(started)
				<-proceed
			}
		}
		saved := make(chan error, 1)
		go func() {
			_, saveErr := session.Save()
			saved <- saveErr
		}()
		<-started

		// Apply 1..3 concurrent edits E1 while Save is blocked. pendingApplied
		// counts real edits: if none land, the Save commits E0 and removes the
		// journal, so a clean reopen is Recovered=false (not a bug).
		pendingApplied := 0
		pendingEdits := 1
		if pos < len(program) {
			pendingEdits = 1 + int(program[pos])%3
		}
		for i := 0; i < pendingEdits; i++ {
			if next, ok := applyEdit(reference); ok {
				reference = next
				pendingApplied++
			}
		}

		close(proceed)
		session.commitHook = nil
		if err := <-saved; err != nil {
			t.Fatalf("Save: %v", err)
		}

		// The session content must reflect the committed snapshot + the rebased
		// pending edits.
		if !bytesEqual(sessionContent(t, session), reference) {
			t.Fatalf("post-save content mismatch\n got: %q\nwant: %q", sessionContent(t, session), reference)
		}
		meta := session.Metadata()
		if meta.CommittedRevision == 0 || meta.Dirty != (pendingApplied > 0) {
			t.Fatalf("post-save metadata = %+v, pendingApplied=%d", meta, pendingApplied)
		}
		if disk, err := os.ReadFile(path); err != nil || !bytesEqual(disk, committedReference) {
			t.Fatalf("disk after save = %q (err %v), want committed %q", disk, err, committedReference)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}

		// Reopen: the rebased journal must recover the in-memory content.
		reopened, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "s2")})
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer reopened.Close()
		if reopened.Metadata().Recovered != (pendingApplied > 0) {
			t.Fatalf("reopen recovered=%v, want %v (pendingApplied=%d): %+v",
				reopened.Metadata().Recovered, pendingApplied > 0, pendingApplied, reopened.Metadata())
		}
		if !bytesEqual(sessionContent(t, reopened), reference) {
			t.Fatalf("reopened content mismatch\n got: %q\nwant: %q", sessionContent(t, reopened), reference)
		}

		// Undoing every rebased batch must restore the committed snapshot.
		for {
			if _, err := reopened.Undo(); err != nil {
				if !errors.Is(err, ErrNothingToUndo) {
					t.Fatalf("reopen undo: %v", err)
				}
				break
			}
		}
		if !bytesEqual(sessionContent(t, reopened), committedReference) {
			t.Fatalf("after undo-all content = %q, want committed %q", sessionContent(t, reopened), committedReference)
		}
	})
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
