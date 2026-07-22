package document

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moresleep512/docengine/document/coordinate"
)

func TestSessionCoordinateIndexAndChangeMaps(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "aé\nz")
	defer session.Close()

	original, err := session.CoordinateIndex(context.Background(), coordinate.Options{CheckpointBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if stats := original.Stats(); stats.Revision != 0 || stats.ByteLength != 5 || stats.RuneCount != 4 || stats.LineCount != 2 {
		t.Fatalf("original stats = %+v", stats)
	}
	position, err := original.ByteToPosition(context.Background(), 4)
	if err != nil || position.Line != 1 || position.Column != 0 || position.RuneOffset != 3 {
		t.Fatalf("original position = (%+v, %v)", position, err)
	}

	result, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 1, Insert: "🙂"},
		{Start: 5, DeleteLength: 2, Insert: "界"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 2 || result.ByteLength != 10 || result.Changes.BeforeRevision() != 0 || result.Changes.AfterRevision() != 2 || result.Changes.BeforeLength() != 5 || result.Changes.AfterLength() != 10 || result.Changes.Len() != 2 {
		t.Fatalf("apply result = %+v", result)
	}
	before, err := result.Changes.Transform(coordinate.Anchor{Offset: 1, Affinity: coordinate.AffinityBefore})
	if err != nil || before.Offset != 1 {
		t.Fatalf("before anchor = (%+v, %v)", before, err)
	}
	after, err := result.Changes.Transform(coordinate.Anchor{Offset: 1, Affinity: coordinate.AffinityAfter})
	if err != nil || after.Offset != 8 {
		t.Fatalf("after anchor = (%+v, %v)", after, err)
	}

	current, err := session.CoordinateIndex(context.Background(), coordinate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats := current.Stats(); stats.Revision != 2 || stats.ByteLength != 10 || stats.RuneCount != 5 || stats.LineCount != 2 {
		t.Fatalf("current stats = %+v", stats)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}

	noChange, err := session.ApplyBatch(context.Background(), 2, nil)
	if err != nil || noChange.Changes.Len() != 0 || noChange.Changes.BeforeRevision() != 2 || noChange.Changes.AfterRevision() != 2 {
		t.Fatalf("no-op result = (%+v, %v)", noChange, err)
	}
	undo, err := session.Undo()
	if err != nil || undo.Changes.BeforeRevision() != 2 || undo.Changes.AfterRevision() != 4 || undo.Changes.AfterLength() != 5 {
		t.Fatalf("Undo = (%+v, %v)", undo, err)
	}
	redo, err := session.Redo()
	if err != nil || redo.Changes.BeforeRevision() != 4 || redo.Changes.AfterRevision() != 6 || redo.Changes.AfterLength() != 10 {
		t.Fatalf("Redo = (%+v, %v)", redo, err)
	}
}

func TestSessionCoordinateIndexLifetimeAndErrors(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	before := generationReferences(session.generation)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.CoordinateIndex(canceled, coordinate.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled CoordinateIndex = %v", err)
	}
	if after := generationReferences(session.generation); after != before {
		t.Fatalf("snapshot lease leaked: refs %d -> %d", before, after)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.CoordinateIndex(context.Background(), coordinate.Options{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed CoordinateIndex = %v", err)
	}
}

func TestSessionRebuildCoordinateIndexAcrossChangeMapChain(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abcdef\n123456\n")
	defer session.Close()
	previous, err := session.CoordinateIndex(context.Background(), coordinate.Options{CheckpointBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()

	first, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 12, Insert: "é"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 15, Insert: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := first.Changes.Compose(second.Changes)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := session.RebuildCoordinateIndex(context.Background(), previous, changes)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	fresh, err := session.CoordinateIndex(context.Background(), coordinate.Options{CheckpointBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	rebuiltStats, freshStats := rebuilt.Stats(), fresh.Stats()
	if rebuiltStats.Revision != 2 || rebuiltStats.ReusedCheckpoints < 2 || rebuiltStats.ScannedBytes >= rebuiltStats.ByteLength ||
		rebuiltStats.ByteLength != freshStats.ByteLength || rebuiltStats.RuneCount != freshStats.RuneCount || rebuiltStats.LineCount != freshStats.LineCount {
		t.Fatalf("rebuilt=%+v fresh=%+v", rebuiltStats, freshStats)
	}
	for offset := int64(0); offset <= rebuiltStats.ByteLength; offset++ {
		left, leftErr := rebuilt.ByteToPosition(context.Background(), offset)
		right, rightErr := fresh.ByteToPosition(context.Background(), offset)
		if left != right || (leftErr == nil) != (rightErr == nil) {
			t.Fatalf("offset %d: rebuilt=(%+v,%v) fresh=(%+v,%v)", offset, left, leftErr, right, rightErr)
		}
	}
	oldEOF, err := previous.ByteToPosition(context.Background(), 14)
	if err != nil || oldEOF.RuneOffset != 14 {
		t.Fatalf("previous index lifetime = (%+v, %v)", oldEOF, err)
	}

	beforeRefs := generationReferences(session.generation)
	identity, _ := coordinate.Identity(0, 14)
	if _, err := session.RebuildCoordinateIndex(context.Background(), previous, identity); !errors.Is(err, coordinate.ErrRevisionMismatch) {
		t.Fatalf("current revision mismatch = %v", err)
	}
	if afterRefs := generationReferences(session.generation); afterRefs != beforeRefs {
		t.Fatalf("mismatch leaked Snapshot: %d -> %d", beforeRefs, afterRefs)
	}
	if _, err := session.RebuildCoordinateIndex(nil, previous, changes); !errors.Is(err, coordinate.ErrInvalidContext) {
		t.Fatalf("nil context = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.RebuildCoordinateIndex(canceled, previous, changes); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context = %v", err)
	}
	if _, err := session.RebuildCoordinateIndex(context.Background(), nil, changes); !errors.Is(err, coordinate.ErrInvalidIndex) {
		t.Fatalf("nil previous = %v", err)
	}
	if afterRefs := generationReferences(session.generation); afterRefs != beforeRefs {
		t.Fatalf("failed rebuild leaked Snapshot: %d -> %d", beforeRefs, afterRefs)
	}
}

func TestRebuildCoordinateIndexAfterSessionClose(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	previous, err := session.CoordinateIndex(context.Background(), coordinate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := coordinate.Identity(0, 3)
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.RebuildCoordinateIndex(context.Background(), previous, identity); !errors.Is(err, coordinate.ErrClosed) {
		t.Fatalf("closed previous = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.RebuildCoordinateIndex(context.Background(), previous, identity); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Session = %v", err)
	}
}

func TestRebuiltCoordinateIndexHoldsSessionCloseBarrier(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	previous, err := session.CoordinateIndex(context.Background(), coordinate.Options{CheckpointBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "é"}})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := session.RebuildCoordinateIndex(context.Background(), previous, result.Changes)
	if err != nil {
		t.Fatal(err)
	}
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	waitForSessionClosedFlag(t, session)
	select {
	case err := <-closed:
		t.Fatalf("Session Close crossed rebuilt-index barrier: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if position, err := rebuilt.ByteToPosition(context.Background(), 5); err != nil || position.RuneOffset != 4 {
		t.Fatalf("rebuilt index during Session close = (%+v, %v)", position, err)
	}
	if err := rebuilt.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Session Close did not finish after rebuilt index release")
	}
}

func generationReferences(generation *sourceGeneration) int {
	generation.mu.Lock()
	defer generation.mu.Unlock()
	return generation.refs
}
