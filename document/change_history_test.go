package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/moresleep512/docengine/document/coordinate"
)

func TestChangeHistoryForwardReverseExpiryAndBoundaries(t *testing.T) {
	history := newChangeHistory(3, 0, 10)
	first := mustChangeMap(t, 0, 2, 10, []coordinate.Edit{{Start: 2, OldLength: 3, NewLength: 1}})
	second := mustChangeMap(t, 2, 3, first.AfterLength(), []coordinate.Edit{{Start: 6, NewLength: 2}})
	third := mustChangeMap(t, 3, 5, second.AfterLength(), []coordinate.Edit{{Start: 1, OldLength: 1, NewLength: 3}})
	for _, change := range []coordinate.ChangeMap{first, second, third} {
		history.append(change)
	}
	if stats := history.stats(); stats != (ChangeHistoryStats{OldestRevision: 0, CurrentRevision: 5, Entries: 3, Limit: 3}) {
		t.Fatalf("stats = %+v", stats)
	}
	forward, err := history.between(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := first.Compose(second)
	want, _ = want.Compose(third)
	if forward.BeforeRevision() != 0 || forward.AfterRevision() != 5 || forward.BeforeLength() != 10 || forward.AfterLength() != third.AfterLength() || forward.Len() != want.Len() {
		t.Fatalf("forward = %+v", forward)
	}
	anchors := []coordinate.Anchor{{Offset: 0, Affinity: coordinate.AffinityBefore}, {Offset: 2, Affinity: coordinate.AffinityAfter}, {Offset: 10, Affinity: coordinate.AffinityAfter}}
	got, err := forward.TransformAnchors(anchors)
	expected, _ := want.TransformAnchors(anchors)
	if err != nil || !equalAnchors(got, expected) {
		t.Fatalf("forward anchors = (%+v, %v), want %+v", got, err, expected)
	}
	reverse, err := history.between(5, 0)
	if err != nil || reverse.BeforeRevision() != 5 || reverse.AfterRevision() != 0 || reverse.BeforeLength() != third.AfterLength() || reverse.AfterLength() != 10 {
		t.Fatalf("reverse = (%+v, %v)", reverse, err)
	}
	identity, err := history.between(2, 2)
	if err != nil || identity.BeforeLength() != first.AfterLength() || identity.Len() != 0 {
		t.Fatalf("identity = (%+v, %v)", identity, err)
	}
	if suffix, err := history.between(2, 5); err != nil || suffix.BeforeRevision() != 2 || suffix.AfterRevision() != 5 {
		t.Fatalf("suffix = (%+v, %v)", suffix, err)
	}
	for _, revision := range [][2]uint64{{1, 5}, {0, 4}, {0, 6}} {
		if _, err := history.between(revision[0], revision[1]); !errors.Is(err, ErrRevisionUnavailable) {
			t.Fatalf("unavailable %v = %v", revision, err)
		}
	}

	fourth := mustChangeMap(t, 5, 6, third.AfterLength(), []coordinate.Edit{{Start: third.AfterLength(), NewLength: 1}})
	history.append(fourth)
	if stats := history.stats(); stats.OldestRevision != 2 || stats.CurrentRevision != 6 || stats.Entries != 3 {
		t.Fatalf("evicted stats = %+v", stats)
	}
	if _, err := history.between(0, 6); !errors.Is(err, ErrChangeHistoryExpired) {
		t.Fatalf("expired history = %v", err)
	} else {
		var detail *ChangeHistoryError
		if !errors.As(err, &detail) || detail.FromRevision != 0 || detail.ToRevision != 6 || detail.OldestRevision != 2 || detail.CurrentRevision != 6 || detail.Error() == "" {
			t.Fatalf("expired detail = %+v", detail)
		}
	}
	if _, err := history.between(6, 0); !errors.Is(err, ErrChangeHistoryExpired) {
		t.Fatalf("reverse expired history = %v", err)
	}
	if retained, err := history.between(2, 6); err != nil || retained.BeforeLength() != first.AfterLength() || retained.AfterLength() != fourth.AfterLength() {
		t.Fatalf("retained map = (%+v, %v)", retained, err)
	}
}

func TestChangeHistoryResetAndIdentity(t *testing.T) {
	history := newChangeHistory(2, 0, 1)
	identity, _ := coordinate.Identity(0, 1)
	history.append(identity)
	if stats := history.stats(); stats.Entries != 0 || stats.OldestRevision != 0 || stats.CurrentRevision != 0 {
		t.Fatalf("identity stats = %+v", stats)
	}
	discontinuous := mustChangeMap(t, 10, 11, 4, []coordinate.Edit{{Start: 4, NewLength: 1}})
	history.append(discontinuous)
	if stats := history.stats(); stats.OldestRevision != 10 || stats.CurrentRevision != 11 || stats.Entries != 1 {
		t.Fatalf("reset stats = %+v", stats)
	}
	invalidIdentity := coordinate.ChangeMap{}
	history.append(invalidIdentity)
	if stats := history.stats(); stats.OldestRevision != 0 || stats.CurrentRevision != 0 || stats.Entries != 0 {
		t.Fatalf("invalid identity reset = %+v", stats)
	}

	first := mustChangeMap(t, 0, 1, 1, []coordinate.Edit{{Start: 1, NewLength: 1}})
	disjoint := mustChangeMap(t, 2, 3, first.AfterLength(), []coordinate.Edit{{Start: 0, NewLength: 1}})
	malformed := &changeHistory{
		entries: []coordinate.ChangeMap{first, disjoint}, count: 2,
		baseRevision: 0, baseLength: 1, currentRevision: 3, currentLength: disjoint.AfterLength(),
	}
	if _, err := malformed.between(0, 3); !errors.Is(err, ErrRevisionUnavailable) {
		t.Fatalf("discontinuous history = %v", err)
	}
	wrongLength := mustChangeMap(t, 0, 1, 2, []coordinate.Edit{{Start: 2, NewLength: 1}})
	malformed = &changeHistory{
		entries: []coordinate.ChangeMap{wrongLength}, count: 1,
		baseRevision: 0, baseLength: 1, currentRevision: 1, currentLength: wrongLength.AfterLength(),
	}
	if _, err := malformed.between(0, 1); !errors.Is(err, ErrRevisionUnavailable) {
		t.Fatalf("length-discontinuous history = %v", err)
	}
}

func TestSessionChangeHistoryAndBatchAnchors(t *testing.T) {
	session := openChangeHistoryTestSession(t, "abcdef", SessionLimits{ChangeHistory: 2, MaxAnchorBatch: 3})
	previousIndex, err := session.CoordinateIndex(context.Background(), coordinate.Options{CheckpointBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if stats := session.ChangeHistoryStats(); stats != (ChangeHistoryStats{OldestRevision: 0, CurrentRevision: 0, Entries: 0, Limit: 2}) {
		t.Fatalf("initial stats = %+v", stats)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if stats := session.ChangeHistoryStats(); stats.Entries != 0 {
		t.Fatalf("no-op history = %+v", stats)
	}
	first, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 2, DeleteLength: 1, Insert: "XY"}, {Start: 5, Insert: "z"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.ApplyBatch(context.Background(), 2, []ReplaceOperation{{Start: 0, Insert: "Q"}})
	if err != nil {
		t.Fatal(err)
	}
	third, err := session.ApplyBatch(context.Background(), 3, []ReplaceOperation{{Start: second.ByteLength, Insert: "!"}})
	if err != nil {
		t.Fatal(err)
	}
	if stats := session.ChangeHistoryStats(); stats.OldestRevision != 2 || stats.CurrentRevision != 4 || stats.Entries != 2 {
		t.Fatalf("retained stats = %+v", stats)
	}
	if _, err := session.ChangesBetween(0, 4); !errors.Is(err, ErrChangeHistoryExpired) {
		t.Fatalf("expired Session map = %v", err)
	}
	beforeRefs := generationReferences(session.generation)
	if _, err := session.RefreshCoordinateIndex(context.Background(), previousIndex); !errors.Is(err, ErrChangeHistoryExpired) {
		t.Fatalf("expired index refresh = %v", err)
	}
	if afterRefs := generationReferences(session.generation); afterRefs != beforeRefs {
		t.Fatalf("expired refresh leaked Snapshot: %d -> %d", beforeRefs, afterRefs)
	}
	retained, err := session.ChangesBetween(2, 4)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := second.Changes.Compose(third.Changes)
	if retained.Len() != want.Len() || retained.BeforeLength() != first.ByteLength || retained.AfterLength() != third.ByteLength {
		t.Fatalf("retained = %+v, want %+v", retained, want)
	}
	anchors := []coordinate.Anchor{{Offset: 0, Affinity: coordinate.AffinityBefore}, {Offset: first.ByteLength, Affinity: coordinate.AffinityAfter}}
	transformed, err := session.TransformAnchors(2, 4, anchors)
	expected, _ := want.TransformAnchors(anchors)
	if err != nil || !equalAnchors(transformed, expected) {
		t.Fatalf("Session anchors = (%+v, %v), want %+v", transformed, err, expected)
	}
	reversed, err := session.TransformAnchors(4, 2, transformed)
	if err != nil || len(reversed) != len(anchors) {
		t.Fatalf("reverse anchors = (%+v, %v)", reversed, err)
	}
	if got, err := session.TransformAnchors(2, 4, make([]coordinate.Anchor, 4)); !errors.Is(err, ErrLimitExceeded) || got != nil {
		t.Fatalf("anchor limit = (%+v, %v)", got, err)
	}
	invalid := []coordinate.Anchor{{Offset: -1, Affinity: coordinate.AffinityBefore}}
	if got, err := session.TransformAnchors(2, 4, invalid); !errors.Is(err, coordinate.ErrInvalidOffset) || got != nil {
		t.Fatalf("invalid anchors = (%+v, %v)", got, err)
	}
	if got, err := session.TransformAnchors(0, 4, anchors); !errors.Is(err, ErrChangeHistoryExpired) || got != nil {
		t.Fatalf("expired anchor transform = (%+v, %v)", got, err)
	}
	if err := previousIndex.Close(); err != nil {
		t.Fatal(err)
	}

	largeEdits := make([]coordinate.Edit, 1_024)
	largeMap := mustChangeMap(t, 100, 101, 0, largeEdits)
	session.mu.Lock()
	session.changeHistory = newChangeHistory(1, 100, 0)
	session.changeHistory.append(largeMap)
	session.config.Limits.MaxAnchorBatch = DefaultMaxAnchorBatch
	session.mu.Unlock()
	tooMuchWork := make([]coordinate.Anchor, maximumAnchorTransformSteps/len(largeEdits)+1)
	for index := range tooMuchWork {
		tooMuchWork[index].Affinity = coordinate.AffinityBefore
	}
	if got, err := session.TransformAnchors(100, 101, tooMuchWork); !errors.Is(err, ErrLimitExceeded) || got != nil {
		t.Fatalf("anchor work limit = (%d, %v)", len(got), err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if stats := session.ChangeHistoryStats(); stats.CurrentRevision != 101 {
		t.Fatalf("closed stats = %+v", stats)
	}
	if _, err := session.ChangesBetween(100, 101); err != nil {
		t.Fatalf("closed ChangesBetween = %v", err)
	}
}

func TestSessionChangeHistoryTracksUndoRedoAndConcurrentReads(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Redo(); err != nil {
		t.Fatal(err)
	}
	if stats := session.ChangeHistoryStats(); stats.Entries != 3 || stats.CurrentRevision != 3 {
		t.Fatalf("undo/redo stats = %+v", stats)
	}

	var readers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for count := 0; count < 100; count++ {
				stats := session.ChangeHistoryStats()
				if _, err := session.ChangesBetween(stats.OldestRevision, stats.CurrentRevision); err != nil && !errors.Is(err, ErrChangeHistoryExpired) {
					t.Errorf("concurrent ChangesBetween = %v", err)
					return
				}
			}
		}()
	}
	for revision := uint64(3); revision < 20; revision++ {
		metadata := session.Metadata()
		if _, err := session.ApplyBatch(context.Background(), revision, []ReplaceOperation{{Start: metadata.ByteLength, Insert: "y"}}); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveredSessionStartsChangeHistoryAtRecoveredRevision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := OpenOptions{
		RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session-1"),
		Limits: SessionLimits{ChangeHistory: 4},
	}
	first, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}, {Start: 4, Insert: "y"}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	options.SessionDir = filepath.Join(dir, "session-2")
	recovered, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if stats := recovered.ChangeHistoryStats(); stats.OldestRevision != 2 || stats.CurrentRevision != 2 || stats.Entries != 0 {
		t.Fatalf("recovered history = %+v", stats)
	}
	if _, err := recovered.ChangesBetween(0, 2); !errors.Is(err, ErrChangeHistoryExpired) {
		t.Fatalf("pre-open revision = %v", err)
	}
	if _, err := recovered.ApplyBatch(context.Background(), 2, []ReplaceOperation{{Start: 5, Insert: "z"}}); err != nil {
		t.Fatal(err)
	}
	beforeSave := recovered.ChangeHistoryStats()
	if _, err := recovered.Save(); err != nil {
		t.Fatal(err)
	}
	if afterSave := recovered.ChangeHistoryStats(); afterSave != beforeSave {
		t.Fatalf("Save changed history: %+v -> %+v", beforeSave, afterSave)
	}
	if _, err := recovered.ChangesBetween(2, 3); err != nil {
		t.Fatalf("post-recovery map = %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustChangeMap(t testing.TB, beforeRevision, afterRevision uint64, beforeLength int64, edits []coordinate.Edit) coordinate.ChangeMap {
	t.Helper()
	change, err := coordinate.NewChangeMap(beforeRevision, afterRevision, beforeLength, edits)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func equalAnchors(left, right []coordinate.Anchor) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func openChangeHistoryTestSession(t testing.TB, content string, limits SessionLimits) *Session {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session"), Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}
