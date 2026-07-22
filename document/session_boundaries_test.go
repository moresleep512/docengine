package document

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moresleep512/docengine/document/store"
	"github.com/moresleep512/docengine/recovery"
)

func TestOpenRejectsPathEncodingContentAndTransientStorageFailures(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing"), OpenOptions{}); err == nil {
		t.Fatal("expected missing-file error")
	}
	if _, err := Open("bad\x00path", OpenOptions{}); err == nil {
		t.Fatal("expected invalid-path error")
	}

	dir := t.TempDir()
	invalidUTF8 := filepath.Join(dir, "invalid.txt")
	if err := os.WriteFile(invalidUTF8, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(invalidUTF8, OpenOptions{}); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}

	path := filepath.Join(dir, "valid.txt")
	if err := os.WriteFile(path, []byte("valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, OpenOptions{SessionDir: filepath.Join(blocker, "session")}); err == nil {
		t.Fatal("expected session-directory error")
	}
	if _, err := Open(path, OpenOptions{SessionDir: filepath.Join(dir, "session"), RecoveryDir: filepath.Join(blocker, "recovery")}); err == nil {
		t.Fatal("expected recovery-directory error")
	}

	t.Setenv("TEMP", filepath.Join(dir, "temp"))
	t.Setenv("TMP", filepath.Join(dir, "temp"))
	defaulted, err := Open(path, OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if defaulted.recoveryDir == "" || defaulted.sessionDir == "" {
		t.Fatalf("default directories = (%q, %q)", defaulted.recoveryDir, defaulted.sessionDir)
	}
	if err := defaulted.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenMatchingJournalV2IsolationAndQuarantine(t *testing.T) {
	badPatternDir := filepath.Join(t.TempDir(), "[")
	if err := os.Mkdir(badPatternDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openMatchingJournal(badPatternDir, recovery.Fingerprint{}); err == nil {
		t.Fatal("expected bad glob pattern error")
	}
	parentFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openMatchingJournal(filepath.Join(parentFile, "recovery"), recovery.Fingerprint{}); err == nil {
		t.Fatal("expected recovery MkdirAll error")
	}

	dir := t.TempDir()
	fingerprint := recovery.Fingerprint{PathHash: [32]byte{7}}
	prefix := journalPrefix(fingerprint)
	legacy := filepath.Join(dir, prefix+".legacy.docengine-journal")
	if err := os.WriteFile(legacy, []byte("v1 is ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, replay, err := openMatchingJournal(dir, fingerprint)
	if err != nil || journal != nil || len(replay.Batches) != 0 {
		t.Fatalf("legacy openMatchingJournal = (%v, %+v, %v)", journal, replay, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy journal was modified: %v", err)
	}

	older := filepath.Join(dir, prefix+".older.docengine-journal-v2")
	newer := filepath.Join(dir, prefix+".newer.docengine-journal-v2")
	for _, path := range []string{older, newer} {
		created, _, createErr := recovery.Open(path, fingerprint)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if closeErr := created.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if err := os.WriteFile(newer, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(older, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}
	journal, replay, err = openMatchingJournal(dir, fingerprint)
	var recoveryErr *RecoveryOpenError
	if !errors.As(err, &recoveryErr) || journal != nil || len(replay.Batches) != 0 {
		t.Fatalf("ambiguous openMatchingJournal = (%v, %+v, %v)", journal, replay, err)
	}
	quarantined, err := filepath.Glob(filepath.Join(dir, "*.quarantine-ambiguous-*"))
	if err != nil || len(quarantined) != 2 {
		t.Fatalf("quarantined journals = %v, error = %v", quarantined, err)
	}
}

func TestOpenRejectsSemanticallyInvalidMatchingJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint := recoveryFingerprintForTest(t, path)
	recoveryDir := filepath.Join(dir, "recovery")
	journalPath := filepath.Join(recoveryDir, journalPrefix(fingerprint)+".invalid.docengine-journal-v2")
	journal, _, err := recovery.Open(journalPath, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.AppendBatch(1, 1, []recovery.ReplaceOperation{{Start: 99, DeleteLength: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session")}); err == nil {
		t.Fatal("Open accepted semantically invalid recovery frame")
	}
}

func TestSessionClosedEmptyAndCommitBoundaries(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := session.ApplyBatch(canceled, 0, nil)
	if err != nil || result.Revision != 0 || result.ByteLength != 3 || result.Dirty {
		t.Fatalf("empty batch = (%+v, %v)", result, err)
	}
	if _, err := session.CommitAtLeast(1); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("future commit error = %v", err)
	}
	if metadata, err := session.Save(); err != nil || metadata.Dirty {
		t.Fatalf("clean save = (%+v, %v)", metadata, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := session.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed ReadAt error = %v", err)
	}
	if _, _, err := session.Snapshot(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Snapshot error = %v", err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed ApplyBatch error = %v", err)
	}
	if _, err := session.Save(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Save error = %v", err)
	}
}

func TestHistoryQuotaEpochAndStorageErrors(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	defer session.Close()
	session.undo = []historyEntry{{}}
	session.redo = []historyEntry{{}}
	session.undoStore.quota = 0
	ref, err := session.historyText([]byte("too large"))
	if err != nil || ref != (textRef{}) || session.undoEpoch != 1 || len(session.undo) != 0 || len(session.redo) != 0 {
		t.Fatalf("quota history = (%+v, %v), epoch=%d undo=%d redo=%d", ref, err, session.undoEpoch, len(session.undo), len(session.redo))
	}

	session.undoStore.quota = 3
	if _, err := session.undoStore.append([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	ref, err = session.historyText([]byte("x"))
	if err != nil || ref.length != 1 || session.undoEpoch != 2 {
		t.Fatalf("quota reset history = (%+v, %v), epoch=%d", ref, err, session.undoEpoch)
	}

	if err := session.undoStore.file.Close(); err != nil {
		t.Fatal(err)
	}
	session.undoStore.quota = 0
	if _, err := session.historyText([]byte("x")); err == nil {
		t.Fatal("expected reset failure on closed underlying file")
	}
	session.undoStore.file = nil
}

func TestUndoRedoPropagateHistoryReadFailures(t *testing.T) {
	t.Run("undo", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		session.undo = []historyEntry{{inverse: []historyOperation{{insert: textRef{length: 1}}}}}
		if err := session.undoStore.close(); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Undo(); !errors.Is(err, ErrClosed) {
			t.Fatalf("Undo error = %v", err)
		}
	})
	t.Run("redo", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		session.redo = []historyEntry{{forward: []historyOperation{{insert: textRef{length: 1}}}}}
		if err := session.undoStore.close(); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Redo(); !errors.Is(err, ErrClosed) {
			t.Fatalf("Redo error = %v", err)
		}
	})
	t.Run("empty redo", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if _, err := session.Redo(); !errors.Is(err, ErrNothingToRedo) {
			t.Fatalf("Redo error = %v", err)
		}
	})
	t.Run("undo apply failure", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		if err := session.journal.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Undo(); !errors.Is(err, recovery.ErrClosed) {
			t.Fatalf("Undo error = %v", err)
		}
	})
	t.Run("redo apply failure", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Undo(); err != nil {
			t.Fatal(err)
		}
		if err := session.journal.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := session.Redo(); !errors.Is(err, recovery.ErrClosed) {
			t.Fatalf("Redo error = %v", err)
		}
	})
}

func TestApplyBatchPropagatesTreeReadAndJournalCreationFailures(t *testing.T) {
	t.Run("tree read", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		session.tree.SetSource(store.SourceBase, failingReaderAt{})
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 1}}); err == nil {
			t.Fatal("expected staged source-read failure")
		}
	})
	t.Run("journal creation", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		session.recoveryDir = filepath.Join(blocker, "recovery")
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); err == nil {
			t.Fatal("expected journal creation failure")
		}
	})
}

func TestReplayV2BatchBoundariesAndFailures(t *testing.T) {
	t.Run("zero first revision", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		err := session.replay(recovery.ReplayResult{Batches: []recovery.Batch{{Group: 1, Operations: []recovery.Operation{{}}}}})
		if err == nil {
			t.Fatal("zero revision accepted")
		}
	})
	t.Run("non-contiguous batches", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		err := session.replay(recovery.ReplayResult{Batches: []recovery.Batch{
			{FirstRevision: 1, Group: 1, Operations: []recovery.Operation{{}}},
			{FirstRevision: 3, Group: 3, Operations: []recovery.Operation{{}}},
		}})
		if err == nil {
			t.Fatal("non-contiguous batches accepted")
		}
	})
	t.Run("valid groups", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		first, err := session.journal.AppendBatch(1, 1, []recovery.ReplaceOperation{{Inserted: []byte("A")}})
		if err != nil {
			t.Fatal(err)
		}
		second, err := session.journal.AppendBatch(2, 2, []recovery.ReplaceOperation{{Start: 1, Inserted: []byte("B")}})
		if err != nil {
			t.Fatal(err)
		}
		err = session.replay(recovery.ReplayResult{Batches: []recovery.Batch{
			{FirstRevision: 1, Group: 1, Operations: []recovery.Operation{{InsertLength: 1, PayloadOffset: first.PayloadOffsets[0]}}},
			{FirstRevision: 2, Group: 2, Operations: []recovery.Operation{{Start: 1, InsertLength: 1, PayloadOffset: second.PayloadOffsets[0]}}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		assertSessionText(t, session, "ABabc")
		if session.revision != 2 || !session.dirty || !session.recovered || len(session.undo) != 2 {
			t.Fatalf("replay state: revision=%d dirty=%v recovered=%v undo=%d", session.revision, session.dirty, session.recovered, len(session.undo))
		}
	})
	t.Run("journal read", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		_ = session.journal.Close()
		err := session.replay(singleReplayOperation(recovery.Operation{InsertLength: 1}))
		if !errors.Is(err, recovery.ErrClosed) {
			t.Fatalf("replay error = %v", err)
		}
	})
	t.Run("history write", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		result, err := session.journal.AppendBatch(1, 1, []recovery.ReplaceOperation{{Inserted: []byte("x")}})
		if err != nil {
			t.Fatal(err)
		}
		_ = session.undoStore.close()
		err = session.replay(singleReplayOperation(recovery.Operation{InsertLength: 1, PayloadOffset: result.PayloadOffsets[0]}))
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("replay history error = %v", err)
		}
	})
	t.Run("inverse history write", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		_ = session.undoStore.close()
		err := session.replay(singleReplayOperation(recovery.Operation{DeleteLength: 1}))
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("replay inverse-history error = %v", err)
		}
	})
	t.Run("replacement source range", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		if err := session.replay(singleReplayOperation(recovery.Operation{InsertLength: 1, PayloadOffset: 1 << 30})); err == nil {
			t.Fatal("invalid payload offset accepted")
		}
	})
	t.Run("short read", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		session.operations.readRecovery = func(*recovery.Journal, []byte, int64) (int, error) { return 0, nil }
		if err := session.replay(singleReplayOperation(recovery.Operation{InsertLength: 1})); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("replay error = %v", err)
		}
	})
	t.Run("missing tree journal source", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		result, err := session.journal.AppendBatch(1, 1, []recovery.ReplaceOperation{{Inserted: []byte("x")}})
		if err != nil {
			t.Fatal(err)
		}
		session.tree.SetSource(store.SourceJournal, nil)
		err = session.replay(singleReplayOperation(recovery.Operation{InsertLength: 1, PayloadOffset: result.PayloadOffsets[0]}))
		if !errors.Is(err, store.ErrUnknownSource) {
			t.Fatalf("replay error = %v", err)
		}
	})
	t.Run("invalid inserted UTF-8", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if err := session.ensureJournalLocked(); err != nil {
			t.Fatal(err)
		}
		result, err := session.journal.AppendBatch(1, 1, []recovery.ReplaceOperation{{Inserted: []byte{0xff}}})
		if err != nil {
			t.Fatal(err)
		}
		err = session.replay(singleReplayOperation(recovery.Operation{InsertLength: 1, PayloadOffset: result.PayloadOffsets[0]}))
		if !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("replay error = %v", err)
		}
	})
	t.Run("non-boundary edit", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "é")
		defer session.Close()
		err := session.replay(singleReplayOperation(recovery.Operation{Start: 1}))
		if !errors.Is(err, ErrInvalidUTF8Boundary) {
			t.Fatalf("replay error = %v", err)
		}
	})
}

func singleReplayOperation(operation recovery.Operation) recovery.ReplayResult {
	return recovery.ReplayResult{Batches: []recovery.Batch{{FirstRevision: 1, Group: 1, Operations: []recovery.Operation{operation}}}}
}

func TestConcurrentSaveReportsNewJournalCreationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 1, Insert: "A"}}); err != nil {
		t.Fatal(err)
	}
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
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 1, Insert: "B"}}); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.recoveryDir = filepath.Join(blocker, "recovery")
	session.mu.Unlock()
	close(proceed)
	if err := <-saved; err == nil {
		t.Fatal("expected rebased-journal creation error")
	}
	session.commitHook = nil
}

func TestCommitMissingFileAndSyncTicker(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		session, path, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		session.commitHook = func(string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := session.Save(); err == nil {
			t.Fatal("expected missing-file identity error")
		}
	})
	t.Run("snapshot source closes before write", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		session.commitHook = func(string) {
			if err := session.journal.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := session.Save(); !errors.Is(err, recovery.ErrClosed) {
			t.Fatalf("Save error = %v", err)
		}
	})
	t.Run("periodic sync", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "abc")
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReadTreeRangeAndEOLBoundaries(t *testing.T) {
	tree, err := store.New(byteReaderAt("abc"), 3)
	if err != nil {
		t.Fatal(err)
	}
	tree.SetSource(store.SourceBase, failingReaderAt{})
	if _, err := readTreeRange(tree, 0, 1); err == nil {
		t.Fatal("expected tree source-read error")
	}
	if err := validateUTF8ReplacementBoundaries(tree, 1, 0); err == nil {
		t.Fatal("expected boundary source-read error")
	}
	if got := detectEOL([]byte("a\r\nb\nc")); got != EOLMixed {
		t.Fatalf("mixed EOL = %q", got)
	}
}

type failingReaderAt struct{}

func (failingReaderAt) ReadAt([]byte, int64) (int, error) { return 0, errors.New("source read failed") }

type byteReaderAt string

func (value byteReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset >= int64(len(value)) {
		return 0, io.EOF
	}
	n := copy(buffer, value[offset:])
	if n < len(buffer) {
		return n, io.EOF
	}
	return n, nil
}
