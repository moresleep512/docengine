package document

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestSessionCompactReclaimsPiecesUndoAndCheckpointsJournal(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abcdef")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "X"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 3, DeleteLength: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 3, []ReplaceOperation{{Start: 0, Insert: "Q"}}); err != nil {
		t.Fatal(err)
	}
	journalPath := session.journal.Path()
	beforeRevision, beforeContent := session.Metadata().Revision, compactSessionContent(t, session)
	result, err := session.Compact(context.Background(), CompactOptions{CheckpointJournal: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.JournalCheckpointed || result.Metadata.Revision != beforeRevision || result.Metadata.Dirty ||
		result.UndoBytesAfter >= result.UndoBytesBefore || result.Pieces.AfterPieces > result.Pieces.BeforePieces {
		t.Fatalf("compaction = %+v", result)
	}
	if got := compactSessionContent(t, session); got != beforeContent {
		t.Fatalf("content changed: %q -> %q", beforeContent, got)
	}
	if len(session.pending) != 0 || session.journal != nil {
		t.Fatalf("journal not checkpointed: pending=%d journal=%v", len(session.pending), session.journal)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired journal remains: %v", err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatalf("Undo after compaction = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactPreservesRedoAndReportsCleanupError(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	result, err := session.Compact(context.Background(), CompactOptions{})
	if err != nil || result.JournalCheckpointed || result.Metadata.Revision != 2 {
		t.Fatalf("compact with redo = (%+v, %v)", result, err)
	}
	if _, err := session.Redo(); err != nil {
		t.Fatalf("Redo after compaction = %v", err)
	}

	removeErr := errors.New("remove retired undo store")
	session.undoStore.remove = func(string) error { return removeErr }
	result, err = session.Compact(context.Background(), CompactOptions{})
	if !errors.Is(err, removeErr) || result.UndoBytesAfter == 0 {
		t.Fatalf("cleanup failure = (%+v, %v)", result, err)
	}
	session.undoStore.remove = os.Remove
	if _, err := session.Undo(); err != nil {
		t.Fatalf("Undo after reported cleanup failure = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactValidationCancellationClosedAndFaulted(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.Compact(nil, CompactOptions{}); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil context = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.Compact(canceled, CompactOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context = %v", err)
	}
	fault := errors.New("fault")
	session.mu.Lock()
	session.fault = fault
	session.mu.Unlock()
	for _, options := range []CompactOptions{{}, {CheckpointJournal: true}} {
		if _, err := session.Compact(context.Background(), options); !errors.Is(err, ErrFaulted) || !errors.Is(err, fault) {
			t.Fatalf("faulted compact %+v = %v", options, err)
		}
	}
	session.mu.Lock()
	session.fault = nil
	session.mu.Unlock()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	for _, options := range []CompactOptions{{}, {CheckpointJournal: true}} {
		if _, err := session.Compact(context.Background(), options); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed compact %+v = %v", options, err)
		}
	}
}

func TestSessionCompactChecksCancellationAfterJournalCheckpoint(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			cancel()
		}
	}
	result, err := session.Compact(ctx, CompactOptions{CheckpointJournal: true})
	if !errors.Is(err, context.Canceled) || !result.JournalCheckpointed || result.Metadata.CommittedRevision != 1 {
		t.Fatalf("cancel after checkpoint = (%+v, %v)", result, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactReturnsJournalCheckpointFailure(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("checkpoint stat")
	session.operations.stat = func(string) (os.FileInfo, error) { return nil, sentinel }
	result, err := session.Compact(context.Background(), CompactOptions{CheckpointJournal: true})
	if !errors.Is(err, sentinel) || result.JournalCheckpointed {
		t.Fatalf("checkpoint failure = (%+v, %v)", result, err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactCheckpointPreservesConcurrentEditAndOldSnapshot(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	oldJournalPath := session.journal.Path()
	revision, oldSnapshot, err := session.Snapshot()
	if err != nil || revision != 1 {
		t.Fatalf("Snapshot = (%d, %v)", revision, err)
	}
	captured, proceed := make(chan struct{}), make(chan struct{})
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			close(captured)
			<-proceed
		}
	}
	type compactResult struct {
		result CompactionResult
		err    error
	}
	completed := make(chan compactResult, 1)
	go func() {
		result, compactErr := session.Compact(context.Background(), CompactOptions{CheckpointJournal: true})
		completed <- compactResult{result: result, err: compactErr}
	}()
	<-captured
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "y"}}); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	compact := <-completed
	session.commitHook = nil
	if compact.err != nil {
		t.Fatal(compact.err)
	}
	if !compact.result.JournalCheckpointed || compact.result.Metadata.Revision != 2 ||
		compact.result.Metadata.CommittedRevision != 1 || !compact.result.Metadata.Dirty ||
		compact.result.Pieces.AfterPieces > compact.result.Pieces.BeforePieces {
		t.Fatalf("compaction = %+v", compact.result)
	}
	if got := compactSessionContent(t, session); got != "axy" {
		t.Fatalf("current content = %q", got)
	}
	if disk, err := os.ReadFile(path); err != nil || string(disk) != "ax" {
		t.Fatalf("checkpoint content = %q, %v", disk, err)
	}
	if session.journal == nil || session.journal.Path() == oldJournalPath || len(session.pending) != 1 {
		t.Fatalf("rebased journal = %v, pending=%d", session.journal, len(session.pending))
	}
	var snapshotContent bytes.Buffer
	if n, err := oldSnapshot.WriteTo(&snapshotContent); err != nil || n != 2 || snapshotContent.String() != "ax" {
		t.Fatalf("old Snapshot = (%d, %v, %q)", n, err, snapshotContent.String())
	}
	if _, err := os.Stat(oldJournalPath); err != nil {
		t.Fatalf("old journal retired before Snapshot close: %v", err)
	}
	if err := oldSnapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldJournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old journal remains after Snapshot close: %v", err)
	}
	if _, err := oldSnapshot.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Snapshot ReadAt = %v", err)
	}
	if _, err := session.Undo(); err != nil || compactSessionContent(t, session) != "ax" {
		t.Fatalf("Undo after concurrent compaction = %v", err)
	}
	if _, err := session.Redo(); err != nil || compactSessionContent(t, session) != "axy" {
		t.Fatalf("Redo after concurrent compaction = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryReferenceCollectionAndRemapping(t *testing.T) {
	zero := textRef{}
	first := textRef{offset: 2, length: 3}
	second := textRef{offset: 7, length: 4}
	entries := []historyEntry{{
		forward: []historyOperation{{insert: zero}, {insert: first}},
		inverse: []historyOperation{{insert: second}},
	}}
	refs := collectHistoryRefs(nil, entries)
	if len(refs) != 2 || refs[0] != first || refs[1] != second {
		t.Fatalf("refs = %+v", refs)
	}
	remapHistoryRefs(entries, map[textRef]textRef{
		first:  {offset: 0, length: 3},
		second: {offset: 3, length: 4},
	})
	if entries[0].forward[0].insert != zero || entries[0].forward[1].insert.offset != 0 || entries[0].inverse[0].insert.offset != 3 {
		t.Fatalf("remapped entries = %+v", entries)
	}
}

func compactSessionContent(t testing.TB, session *Session) string {
	t.Helper()
	content, err := readSession(session, session.Metadata().ByteLength)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
