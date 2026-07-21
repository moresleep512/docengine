package document

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moresleep512/docengine/document/store"
	"github.com/moresleep512/docengine/recovery"
)

func TestApplyBatchValidationFailuresAreAtomic(t *testing.T) {
	tests := []struct {
		name       string
		operations []ReplaceOperation
		wantError  error
	}{
		{
			name: "later range invalid",
			operations: []ReplaceOperation{
				{Start: 3, Insert: "!"},
				{Start: 99, Insert: "unreachable"},
			},
			wantError: store.ErrInvalidRange,
		},
		{
			name: "later insertion invalid UTF-8",
			operations: []ReplaceOperation{
				{Start: 3, Insert: "!"},
				{Start: 0, Insert: string([]byte{0xff})},
			},
		},
		{
			name: "later insertion oversized",
			operations: []ReplaceOperation{
				{Start: 3, Insert: "!"},
				{Start: 0, Insert: string(make([]byte, (1<<20)+1))},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, _, _ := openAtomicTestSession(t, "abc")
			defer session.Close()
			before := session.Metadata()
			if _, err := session.ApplyBatch(context.Background(), 0, test.operations); err == nil || test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("ApplyBatch error = %v, want %v", err, test.wantError)
			}
			assertSessionState(t, session, before, "abc")
			if _, err := session.Undo(); !errors.Is(err, ErrNothingToUndo) {
				t.Fatalf("Undo error = %v, want %v", err, ErrNothingToUndo)
			}
		})
	}
}

func TestApplyBatchCancellationIsAtomicAtEveryBoundary(t *testing.T) {
	for cancelAt := 1; cancelAt <= 4; cancelAt++ {
		t.Run(string(rune('0'+cancelAt)), func(t *testing.T) {
			session, _, _ := openAtomicTestSession(t, "abc")
			defer session.Close()
			ctx := &countingCancelContext{cancelAt: cancelAt}
			_, err := session.ApplyBatch(ctx, 0, []ReplaceOperation{
				{Start: 3, Insert: "d"},
				{Start: 4, Insert: "e"},
				{Start: 5, Insert: "f"},
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("ApplyBatch error = %v, want %v", err, context.Canceled)
			}
			assertSessionState(t, session, Metadata{ByteLength: 3}, "abc")
		})
	}
}

func TestApplyBatchUsesSequentialCoordinates(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	defer session.Close()
	result, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 1, Insert: "bc"},
		{Start: 2, DeleteLength: 1, Insert: "D"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 2 || result.ByteLength != 3 || !result.Dirty {
		t.Fatalf("result = %+v", result)
	}
	assertSessionText(t, session, "abD")
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	assertSessionText(t, session, "a")
	if _, err := session.Redo(); err != nil {
		t.Fatal(err)
	}
	assertSessionText(t, session, "abD")
}

func TestFailedBatchPreservesRedoHistory(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 2, []ReplaceOperation{{Start: 3, Insert: "!"}, {Start: 99}}); !errors.Is(err, store.ErrInvalidRange) {
		t.Fatalf("ApplyBatch error = %v", err)
	}
	if _, err := session.Redo(); err != nil {
		t.Fatalf("Redo after failed batch: %v", err)
	}
	assertSessionText(t, session, "xabc")
}

func TestJournalAppendFailureDoesNotPublishBatch(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 2, DeleteLength: 1}}); err != nil {
		t.Fatal(err)
	}
	before := session.Metadata()
	if err := session.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), before.Revision, []ReplaceOperation{{Start: 2, Insert: "z"}, {Start: 3, Insert: "!"}}); !errors.Is(err, recovery.ErrClosed) {
		t.Fatalf("ApplyBatch error = %v, want %v", err, recovery.ErrClosed)
	}
	assertSessionState(t, session, before, "ab")
	if len(session.pending) != 1 {
		t.Fatalf("pending operations = %d, want 1", len(session.pending))
	}
}

func TestUndoStoreFailureRollsBackJournalAndDoesNotPublishBatch(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	defer session.Close()
	if err := session.ensureJournalLocked(); err != nil {
		t.Fatal(err)
	}
	journalPath := session.journal.Path()
	beforeJournal, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.undoStore.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "z"}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("ApplyBatch error = %v, want %v", err, ErrClosed)
	}
	assertSessionState(t, session, Metadata{ByteLength: 3}, "abc")
	afterJournal, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterJournal.Size() != beforeJournal.Size() {
		t.Fatalf("journal grew from %d to %d after rollback", beforeJournal.Size(), afterJournal.Size())
	}
}

func TestUndoStoreFailureWhileRecordingDeletedTextIsAtomic(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	defer session.Close()
	if err := session.ensureJournalLocked(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(session.journal.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.undoStore.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 1}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("ApplyBatch error = %v, want %v", err, ErrClosed)
	}
	assertSessionState(t, session, Metadata{ByteLength: 3}, "abc")
	after, err := os.Stat(session.journal.Path())
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("journal grew from %d to %d", before.Size(), after.Size())
	}
}

func TestBatchLimitsAndRevisionOverflowAreAtomic(t *testing.T) {
	t.Run("maximum accepted", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "x")
		defer session.Close()
		operations := make([]ReplaceOperation, 256)
		result, err := session.ApplyBatch(context.Background(), 0, operations)
		if err != nil {
			t.Fatal(err)
		}
		if result.Revision != 256 || result.ByteLength != 1 {
			t.Fatalf("result = %+v", result)
		}
		assertSessionText(t, session, "x")
	})

	t.Run("over maximum rejected", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "x")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, make([]ReplaceOperation, 257)); err == nil {
			t.Fatal("expected oversized batch error")
		}
		assertSessionState(t, session, Metadata{ByteLength: 1}, "x")
	})

	t.Run("revision overflow rejected", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "x")
		defer session.Close()
		session.revision = math.MaxUint64
		before := session.Metadata()
		if _, err := session.ApplyBatch(context.Background(), math.MaxUint64, []ReplaceOperation{{Start: 1, Insert: "y"}}); !errors.Is(err, ErrRevisionOverflow) {
			t.Fatalf("ApplyBatch error = %v, want %v", err, ErrRevisionOverflow)
		}
		assertSessionState(t, session, before, "x")
	})
}

func TestTruncatedAtomicBatchRecoveryNeverExposesPrefix(t *testing.T) {
	session, _, recoveryDir := openAtomicTestSession(t, "abc")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 3, Insert: "d"},
		{Start: 4, Insert: "e"},
	}); err != nil {
		t.Fatal(err)
	}
	journalPath := session.journal.Path()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(journalPath, info.Size()-1); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(filepath.Join(filepath.Dir(recoveryDir), "doc.md"), OpenOptions{RecoveryDir: recoveryDir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertSessionState(t, reopened, Metadata{ByteLength: 3}, "abc")
	if reopened.Metadata().Recovered {
		t.Fatal("truncated batch incorrectly marked as recovered")
	}
	if repaired, err := os.Stat(journalPath); err != nil || repaired.Size() != 72 {
		t.Fatalf("repaired journal = (%v, %v), want 72 bytes", repaired, err)
	}
}

func TestEditRecoveryUndoRedoSaveAndCleanReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrated.md")
	originalDisk := "\xef\xbb\xbfalpha\r\nbeta"
	if err := os.WriteFile(path, []byte(originalDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	options := OpenOptions{RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session-1")}
	session, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	_, snapshot, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 0, DeleteLength: 5, Insert: "ALPHA"},
		{Start: 7, DeleteLength: 4, Insert: "engine"},
		{Start: 13, Insert: "!"},
	}); err != nil {
		t.Fatal(err)
	}
	assertReaderText(t, snapshot, snapshot.Len(), "alpha\r\nbeta")
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	assertSessionText(t, session, "ALPHA\r\nengine!")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	options.SessionDir = filepath.Join(dir, "session-2")
	recovered, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Metadata().Recovered || recovered.Metadata().Revision != 3 {
		t.Fatalf("recovered metadata = %+v", recovered.Metadata())
	}
	assertSessionText(t, recovered, "ALPHA\r\nengine!")
	if _, err := recovered.Undo(); err != nil {
		t.Fatal(err)
	}
	assertSessionText(t, recovered, "alpha\r\nbeta")
	if _, err := recovered.Redo(); err != nil {
		t.Fatal(err)
	}
	assertSessionText(t, recovered, "ALPHA\r\nengine!")
	if _, err := recovered.Save(); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	if disk, err := os.ReadFile(path); err != nil || string(disk) != "\xef\xbb\xbfALPHA\r\nengine!" {
		t.Fatalf("disk = %q, error = %v", disk, err)
	}

	clean, err := Open(path, OpenOptions{RecoveryDir: options.RecoveryDir, SessionDir: filepath.Join(dir, "session-3")})
	if err != nil {
		t.Fatal(err)
	}
	defer clean.Close()
	if metadata := clean.Metadata(); metadata.Dirty || metadata.Recovered || metadata.Revision != 0 || !metadata.HasBOM || metadata.EOL != EOLCRLF {
		t.Fatalf("clean metadata = %+v", metadata)
	}
	assertSessionText(t, clean, "ALPHA\r\nengine!")
}

func TestConcurrentBatchDuringSaveRemainsOneRecoverableUndoUnit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent-batch.md")
	recoveryDir := filepath.Join(dir, "recovery")
	if err := os.WriteFile(path, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session-1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 5, Insert: "A"}}); err != nil {
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
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{
		{Start: 1, Insert: "B"},
		{Start: 2, Insert: "C"},
	}); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	session.commitHook = nil
	if metadata := session.Metadata(); metadata.Revision != 3 || metadata.CommittedRevision != 1 || !metadata.Dirty {
		t.Fatalf("metadata after save = %+v", metadata)
	}
	assertSessionText(t, session, "ABC")
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session-2")})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertSessionText(t, reopened, "ABC")
	if metadata := reopened.Metadata(); metadata.Revision != 3 || metadata.CommittedRevision != 1 || !metadata.Recovered {
		t.Fatalf("reopened metadata = %+v", metadata)
	}
	if _, err := reopened.Undo(); err != nil {
		t.Fatal(err)
	}
	assertSessionText(t, reopened, "A")
}

type countingCancelContext struct {
	checks   int
	cancelAt int
}

func (c *countingCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *countingCancelContext) Done() <-chan struct{}       { return nil }
func (c *countingCancelContext) Value(any) any               { return nil }
func (c *countingCancelContext) Err() error {
	c.checks++
	if c.checks >= c.cancelAt {
		return context.Canceled
	}
	return nil
}

func openAtomicTestSession(t testing.TB, content string) (*Session, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	recoveryDir := filepath.Join(dir, "recovery")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session")})
	if err != nil {
		t.Fatal(err)
	}
	return session, path, recoveryDir
}

func assertSessionState(t testing.TB, session *Session, want Metadata, text string) {
	t.Helper()
	got := session.Metadata()
	if got.Revision != want.Revision || got.CommittedRevision != want.CommittedRevision || got.ByteLength != want.ByteLength || got.Dirty != want.Dirty || got.Recovered != want.Recovered {
		t.Fatalf("metadata = %+v, want revision=%d committed=%d length=%d dirty=%v recovered=%v", got, want.Revision, want.CommittedRevision, want.ByteLength, want.Dirty, want.Recovered)
	}
	assertSessionText(t, session, text)
}

func assertSessionText(t testing.TB, session *Session, want string) {
	t.Helper()
	content, err := readSession(session, session.Metadata().ByteLength)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != want {
		t.Fatalf("session text = %q, want %q", content, want)
	}
}

func assertReaderText(t testing.TB, reader io.ReaderAt, length int64, want string) {
	t.Helper()
	buffer := make([]byte, length)
	n, err := reader.ReadAt(buffer, 0)
	if err != nil && !(errors.Is(err, io.EOF) && n == len(buffer)) {
		t.Fatal(err)
	}
	if string(buffer[:n]) != want {
		t.Fatalf("reader text = %q, want %q", buffer[:n], want)
	}
}
