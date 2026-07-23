package coordinate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRebuildReusesSafePrefixAndMatchesFullBuild(t *testing.T) {
	before := []byte("line0\nline1\nline2\nline3🙂\n")
	previous, err := Build(context.Background(), &testSource{body: before}, 10, Options{CheckpointBytes: 6})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()

	insert := []byte("世界")
	edits := []Edit{{Start: 18, OldLength: 5, NewLength: int64(len(insert))}}
	after := replaceReference(before, edits[0], insert)
	changes, err := NewChangeMap(10, 11, int64(len(before)), edits)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := Rebuild(context.Background(), &testSource{body: after}, previous, changes)
	if err != nil {
		t.Fatal(err)
	}
	defer incremental.Close()
	fresh, err := Build(context.Background(), &testSource{body: after}, 11, Options{CheckpointBytes: 6})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	stats := incremental.Stats()
	if stats.Revision != 11 || stats.ReusedPrefixCheckpoints < 2 ||
		stats.ReusedSuffixCheckpoints == 0 || stats.ReusedCheckpoints != stats.ReusedPrefixCheckpoints+stats.ReusedSuffixCheckpoints ||
		stats.ScannedBytes >= stats.ByteLength-18 {
		t.Fatalf("incremental stats = %+v", stats)
	}
	if full := fresh.Stats(); full.ReusedCheckpoints != 0 || full.ScannedBytes != full.ByteLength {
		t.Fatalf("full stats = %+v", full)
	}
	assertIndexesEquivalent(t, after, incremental, fresh)

	oldEOF, err := previous.ByteToPosition(context.Background(), int64(len(before)))
	if err != nil || oldEOF.RuneOffset != 25 {
		t.Fatalf("previous index changed = (%+v, %v)", oldEOF, err)
	}
}

func TestRebuildUsesEarliestSequentialEditAndUTF8Boundaries(t *testing.T) {
	before := []byte("abcdefghijklmnopqrstuvwxyz012345")
	previous, err := Build(context.Background(), &testSource{body: before}, 1, Options{CheckpointBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()

	firstInsert := []byte("XY")
	secondInsert := []byte("🙂x")
	edits := []Edit{
		{Start: 24, OldLength: 2, NewLength: int64(len(firstInsert))},
		{Start: 4, OldLength: 3, NewLength: int64(len(secondInsert))},
	}
	afterFirst := replaceReference(before, edits[0], firstInsert)
	after := replaceReference(afterFirst, edits[1], secondInsert)
	changes, err := NewChangeMap(1, 3, int64(len(before)), edits)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := Rebuild(context.Background(), &testSource{body: after}, previous, changes)
	if err != nil {
		t.Fatal(err)
	}
	defer incremental.Close()
	fresh, err := Build(context.Background(), &testSource{body: after}, 3, Options{CheckpointBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if stats := incremental.Stats(); stats.ReusedPrefixCheckpoints != 1 ||
		stats.ReusedSuffixCheckpoints != 0 || stats.ScannedBytes != stats.ByteLength {
		t.Fatalf("earliest edit stats = %+v", stats)
	}
	assertIndexesEquivalent(t, after, incremental, fresh)
}

func TestRebuildTranslatesSuffixRuneLineAndColumnState(t *testing.T) {
	before := []byte("head\nalpha beta gamma\nsuffix-one\nsuffix-two🙂\n")
	previous, err := Build(context.Background(), &testSource{body: before}, 20, Options{CheckpointBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()

	insert := []byte("世界\nnew\nlines")
	edit := Edit{Start: 8, OldLength: 7, NewLength: int64(len(insert))}
	after := replaceReference(before, edit, insert)
	changes, err := NewChangeMap(20, 21, int64(len(before)), []Edit{edit})
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := Rebuild(context.Background(), &testSource{body: after}, previous, changes)
	if err != nil {
		t.Fatal(err)
	}
	defer incremental.Close()
	fresh, err := Build(context.Background(), &testSource{body: after}, 21, Options{CheckpointBytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	stats := incremental.Stats()
	if stats.ReusedPrefixCheckpoints == 0 || stats.ReusedSuffixCheckpoints < 2 ||
		stats.ScannedBytes >= stats.ByteLength/2 {
		t.Fatalf("suffix translation did not bound decoding: %+v", stats)
	}
	assertIndexesEquivalent(t, after, incremental, fresh)
}

func TestRebuildFallsBackWhenNoSuffixCheckpointCanSaveWork(t *testing.T) {
	before := []byte("a🙂")
	previous, err := Build(context.Background(), &testSource{body: before}, 1, Options{CheckpointBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	insert := []byte("xyz")
	edit := Edit{Start: 1, OldLength: int64(len("🙂")), NewLength: int64(len(insert))}
	after := replaceReference(before, edit, insert)
	changes, err := NewChangeMap(1, 2, int64(len(before)), []Edit{edit})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Rebuild(context.Background(), &testSource{body: after}, previous, changes)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	// With only the zero and EOF checkpoints, the changed tail is decoded
	// rather than reporting the EOF marker as useful suffix reuse.
	if stats := rebuilt.Stats(); stats.ReusedSuffixCheckpoints != 0 ||
		stats.ScannedBytes != stats.ByteLength {
		t.Fatalf("tail fallback stats = %+v", stats)
	}
}

func TestRebuildRejectsNonBoundarySuffixSeam(t *testing.T) {
	before := []byte("aébcdefgh")
	previous, err := Build(context.Background(), &testSource{body: before}, 1, Options{CheckpointBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	// The map claims a one-byte replacement inside é. The supplied new source
	// is valid UTF-8, but it is not the exact sequential result described by
	// the map, so the mapped old checkpoint lands inside a rune.
	changes, err := NewChangeMap(1, 2, int64(len(before)), []Edit{{Start: 1, OldLength: 1, NewLength: 1}})
	if err != nil {
		t.Fatal(err)
	}
	after := []byte("a界bcdef")
	if int64(len(after)) != changes.AfterLength() {
		after = append(after, 'x')
	}
	if _, err := Rebuild(context.Background(), &testSource{body: after}, previous, changes); !errors.Is(err, ErrSourceInconsistent) && !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("mismatched suffix seam = %v", err)
	}
}

func TestRebuildNetNoopDeduplicatesSuffixSeam(t *testing.T) {
	before := []byte("abcdefgh\nijklmnop")
	previous, err := Build(context.Background(), &testSource{body: before}, 1, Options{CheckpointBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	changes, err := NewChangeMap(1, 3, int64(len(before)), []Edit{
		{Start: 4, NewLength: 1},
		{Start: 4, OldLength: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Rebuild(context.Background(), &testSource{body: before}, previous, changes)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	stats := rebuilt.Stats()
	if stats.ScannedBytes != 0 || stats.CheckpointCount != previous.Stats().CheckpointCount ||
		stats.ReusedCheckpoints != stats.CheckpointCount ||
		stats.ReusedSuffixCheckpoints == 0 {
		t.Fatalf("net-noop reuse stats = %+v", stats)
	}
	fresh, err := Build(context.Background(), &testSource{body: before}, 3, Options{CheckpointBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	assertIndexesEquivalent(t, before, rebuilt, fresh)
}

func TestRebuildEOFInsertionAndIdentityReuse(t *testing.T) {
	before := []byte("abcdef")
	lineage := NewLineage()
	previous, err := Build(context.Background(), &testSource{body: before}, 4, Options{CheckpointBytes: 2, CacheBytes: 6, Lineage: lineage})
	if err != nil {
		t.Fatal(err)
	}
	defer previous.Close()
	if !previous.BelongsTo(lineage) || previous.BelongsTo(NewLineage()) || previous.BelongsTo(nil) {
		t.Fatal("initial lineage mismatch")
	}
	insert := []byte("é\n")
	after := append(append([]byte(nil), before...), insert...)
	changes, err := NewChangeMap(4, 5, int64(len(before)), []Edit{{Start: int64(len(before)), NewLength: int64(len(insert))}})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := Rebuild(context.Background(), &testSource{body: after}, previous, changes)
	if err != nil {
		t.Fatal(err)
	}
	defer appended.Close()
	if stats := appended.Stats(); stats.ScannedBytes != int64(len(insert)) || stats.ReusedCheckpoints != previous.Stats().CheckpointCount {
		t.Fatalf("EOF insertion stats = %+v", stats)
	}
	if stats := appended.Stats(); stats.MaximumCacheBytes != 6 {
		t.Fatalf("Rebuild did not inherit cache budget: %+v", stats)
	}
	if !appended.BelongsTo(lineage) {
		t.Fatal("Rebuild did not preserve lineage")
	}

	identity, err := Identity(5, int64(len(after)))
	if err != nil {
		t.Fatal(err)
	}
	owned := &testSource{body: append([]byte(nil), after...)}
	reused, err := RebuildOwned(context.Background(), owned, appended, identity)
	if err != nil {
		t.Fatal(err)
	}
	if stats := reused.Stats(); stats.ScannedBytes != 0 || stats.ReusedCheckpoints != appended.Stats().CheckpointCount {
		t.Fatalf("identity stats = %+v", stats)
	}
	if err := reused.Close(); err != nil || owned.closeCalls != 1 {
		t.Fatalf("owned Close = (%v, calls=%d)", err, owned.closeCalls)
	}
	if !reused.BelongsTo(lineage) {
		t.Fatal("closed Index lost lineage")
	}
}

func TestRebuildValidationAndOwnedFailure(t *testing.T) {
	base := &testSource{body: []byte("abc")}
	previous, err := Build(context.Background(), base, 1, Options{CheckpointBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := Identity(1, 3)
	if _, err := Rebuild(nil, &testSource{body: []byte("abc")}, previous, identity); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil context = %v", err)
	}
	if _, err := Rebuild(context.Background(), nil, previous, identity); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil source = %v", err)
	}
	if _, err := RebuildOwned(context.Background(), nil, previous, identity); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil owned source = %v", err)
	}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, nil, identity); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("nil previous = %v", err)
	}
	if _, err := Rebuild(context.Background(), &testSource{length: -1, overrideLength: true}, previous, identity); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("negative source length = %v", err)
	}

	revisionMismatch, _ := Identity(2, 3)
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, previous, revisionMismatch); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("revision mismatch = %v", err)
	}
	beforeMismatch, _ := Identity(1, 2)
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("ab")}, previous, beforeMismatch); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("before length mismatch = %v", err)
	}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abcd")}, previous, identity); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("after length mismatch = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Rebuild(canceled, &testSource{body: []byte("abc")}, previous, identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled identity = %v", err)
	}
	if err := previous.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, previous, identity); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed previous = %v", err)
	}

	invalid := &Index{revision: 1, byteLength: 3}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, invalid, identity); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("malformed previous = %v", err)
	}
	invalid = &Index{revision: 1, byteLength: 3, checkpointBytes: 1, checkpoints: []checkpoint{{byteOffset: 1}}}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, invalid, identity); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("missing zero checkpoint = %v", err)
	}
	invalid = &Index{revision: 1, byteLength: 3, checkpointBytes: 1, checkpoints: []checkpoint{{}, {byteOffset: -1}}}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, invalid, identity); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("negative checkpoint = %v", err)
	}
	invalidChange := ChangeMap{beforeRevision: 1, afterRevision: 2, beforeLength: 3, afterLength: 3, edits: []Edit{{Start: -1}}}
	validPrevious := &Index{revision: 1, byteLength: 3, checkpointBytes: 1, checkpoints: []checkpoint{{}}}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, validPrevious, invalidChange); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("negative stable prefix = %v", err)
	}
	lengthInvalidChange := ChangeMap{beforeRevision: 1, afterRevision: 2, beforeLength: 3, afterLength: 3, edits: []Edit{{NewLength: 1}}}
	if _, err := Rebuild(context.Background(), &testSource{body: []byte("abc")}, validPrevious, lengthInvalidChange); !errors.Is(err, ErrInvalidIndex) {
		t.Fatalf("invalid stable suffix = %v", err)
	}

	old, err := Build(context.Background(), &testSource{body: []byte("a")}, 7, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	invalidUTF8 := &testSource{body: []byte{'a', 0xff}, closeErr: errors.New("close")}
	appendInvalid, _ := NewChangeMap(7, 8, 1, []Edit{{Start: 1, NewLength: 1}})
	if _, err := RebuildOwned(context.Background(), invalidUTF8, old, appendInvalid); !errors.Is(err, ErrInvalidUTF8) || !errors.Is(err, invalidUTF8.closeErr) || invalidUTF8.closeCalls != 1 {
		t.Fatalf("invalid owned rebuild = (%v, calls=%d)", err, invalidUTF8.closeCalls)
	}
}

func TestRebuildReleasesPreviousBeforeScanningNewSource(t *testing.T) {
	before := []byte("abcdefghijklmnopqrstuvwxyz")
	previous, err := Build(context.Background(), &testSource{body: before}, 1, Options{CheckpointBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	insert := []byte("🙂")
	edit := Edit{Start: 20, NewLength: int64(len(insert))}
	after := replaceReference(before, edit, insert)
	changes, err := NewChangeMap(1, 2, int64(len(before)), []Edit{edit})
	if err != nil {
		t.Fatal(err)
	}
	source := &gatedSource{testSource: testSource{body: after}, entered: make(chan struct{}), proceed: make(chan struct{})}
	result := make(chan struct {
		index *Index
		err   error
	}, 1)
	go func() {
		index, rebuildErr := Rebuild(context.Background(), source, previous, changes)
		result <- struct {
			index *Index
			err   error
		}{index: index, err: rebuildErr}
	}()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("Rebuild did not begin scanning")
	}
	closed := make(chan error, 1)
	go func() { closed <- previous.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("previous Close remained coupled to new-source scan")
	}
	close(source.proceed)
	var rebuilt *Index
	select {
	case outcome := <-result:
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		rebuilt = outcome.index
	case <-time.After(time.Second):
		t.Fatal("Rebuild did not finish")
	}
	defer rebuilt.Close()
	if position, err := rebuilt.ByteToPosition(context.Background(), int64(len(after))); err != nil || position.ByteOffset != int64(len(after)) {
		t.Fatalf("rebuilt EOF = (%+v, %v)", position, err)
	}
}

func replaceReference(body []byte, edit Edit, insert []byte) []byte {
	result := make([]byte, 0, int64(len(body))-edit.OldLength+int64(len(insert)))
	result = append(result, body[:edit.Start]...)
	result = append(result, insert...)
	result = append(result, body[edit.Start+edit.OldLength:]...)
	return result
}

func assertIndexesEquivalent(t testing.TB, body []byte, left, right *Index) {
	t.Helper()
	leftStats, rightStats := left.Stats(), right.Stats()
	if leftStats.Revision != rightStats.Revision || leftStats.ByteLength != rightStats.ByteLength ||
		leftStats.RuneCount != rightStats.RuneCount || leftStats.LineCount != rightStats.LineCount ||
		leftStats.CheckpointBytes != rightStats.CheckpointBytes {
		t.Fatalf("stats differ: left=%+v right=%+v", leftStats, rightStats)
	}
	for offset := int64(0); offset <= int64(len(body)); offset++ {
		leftPosition, leftErr := left.ByteToPosition(context.Background(), offset)
		rightPosition, rightErr := right.ByteToPosition(context.Background(), offset)
		if leftPosition != rightPosition || !sameCoordinateError(leftErr, rightErr) {
			t.Fatalf("ByteToPosition(%d): left=(%+v,%v) right=(%+v,%v)", offset, leftPosition, leftErr, rightPosition, rightErr)
		}
	}
	for _, position := range referencePositions(body) {
		leftByte, leftErr := left.RuneToByte(context.Background(), position.RuneOffset)
		rightByte, rightErr := right.RuneToByte(context.Background(), position.RuneOffset)
		if leftByte != rightByte || !sameCoordinateError(leftErr, rightErr) {
			t.Fatalf("RuneToByte(%d): left=(%d,%v) right=(%d,%v)", position.RuneOffset, leftByte, leftErr, rightByte, rightErr)
		}
		leftByte, leftErr = left.PositionToByte(context.Background(), position.Line, position.Column)
		rightByte, rightErr = right.PositionToByte(context.Background(), position.Line, position.Column)
		if leftByte != rightByte || !sameCoordinateError(leftErr, rightErr) {
			t.Fatalf("PositionToByte(%d,%d): left=(%d,%v) right=(%d,%v)", position.Line, position.Column, leftByte, leftErr, rightByte, rightErr)
		}
	}
}

func sameCoordinateError(left, right error) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return errors.Is(left, right) || errors.Is(right, left)
}

type gatedSource struct {
	testSource
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (s *gatedSource) ReadAt(buffer []byte, offset int64) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.proceed
	return s.testSource.ReadAt(buffer, offset)
}
