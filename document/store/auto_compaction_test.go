package store

import (
	"bytes"
	"errors"
	"math"
	"sync"
	"testing"
)

func TestAutoCompactionThresholdPreservesSnapshots(t *testing.T) {
	tree, err := NewWithOptions(nil, 0, Options{AutoCompactPieces: 4})
	if err != nil {
		t.Fatal(err)
	}
	tree.SetSource(SourceJournal, bytes.NewReader([]byte("abcdefgh")))

	var retained []Snapshot
	for offset := int64(0); offset < 4; offset++ {
		_, after, err := tree.ReplacePiece(tree.Len(), 0, Piece{
			Source: SourceJournal, Offset: offset, Length: 1,
			NewlinesKnown: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, after)
	}
	stats := tree.Stats()
	if stats.ByteLength != 4 || stats.PieceCount != 1 ||
		stats.AutoCompactPieces != 4 || stats.NextAutoCompactPieces != 4 ||
		stats.AutoCompactions != 1 || !stats.LineBreaksKnown || stats.LineBreaks != 0 {
		t.Fatalf("Stats after threshold compaction = %+v", stats)
	}
	if pieces := nodePieceCount(retained[2].root); pieces != 3 {
		t.Fatalf("pre-compaction Snapshot pieces = %d, want 3", pieces)
	}
	for index, snapshot := range retained {
		assertSnapshot(t, snapshot, "abcd"[:index+1])
		assertSnapshotInvariants(t, snapshot)
	}
	assertSnapshot(t, tree.Snapshot(), "abcd")
	assertTreeInvariants(t, tree)

	for offset := int64(4); offset < 7; offset++ {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
			Source: SourceJournal, Offset: offset, Length: 1,
			NewlinesKnown: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if stats := tree.Stats(); stats.PieceCount != 1 || stats.AutoCompactions != 2 {
		t.Fatalf("second threshold compaction Stats = %+v", stats)
	}
	assertSnapshot(t, tree.Snapshot(), "abcdefg")
}

func TestAutoCompactionBacksOffWhenNothingCanMerge(t *testing.T) {
	tree, err := NewWithOptions(nil, 0, Options{AutoCompactPieces: 4})
	if err != nil {
		t.Fatal(err)
	}
	source := []byte("abcdefghijklmnop")
	tree.SetSource(SourceJournal, bytes.NewReader(source))

	for index := int64(0); index < 4; index++ {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
			Source: SourceJournal, Offset: index * 2, Length: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if stats := tree.Stats(); stats.PieceCount != 4 || stats.AutoCompactions != 1 ||
		stats.NextAutoCompactPieces != 8 {
		t.Fatalf("first no-op compaction Stats = %+v", stats)
	}
	for index := int64(4); index < 7; index++ {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
			Source: SourceJournal, Offset: index * 2, Length: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if stats := tree.Stats(); stats.PieceCount != 7 || stats.AutoCompactions != 1 {
		t.Fatalf("backoff performed eager rescan: %+v", stats)
	}
	if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
		Source: SourceJournal, Offset: 14, Length: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if stats := tree.Stats(); stats.PieceCount != 8 || stats.AutoCompactions != 2 ||
		stats.NextAutoCompactPieces != 12 {
		t.Fatalf("second no-op compaction Stats = %+v", stats)
	}
	assertSnapshot(t, tree.Snapshot(), "acegikmo")
}

func TestAutoCompactionOptionsStatsAndScheduling(t *testing.T) {
	defaults, err := New(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats := defaults.Stats(); stats.AutoCompactPieces != DefaultAutoCompactPieces ||
		stats.NextAutoCompactPieces != DefaultAutoCompactPieces {
		t.Fatalf("default Stats = %+v", stats)
	}

	disabled, err := NewWithBasePieceOptions(bytes.NewReader([]byte("a\n")), Piece{
		Length: 2, Newlines: 1, NewlinesKnown: true,
	}, Options{DisableAutoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	disabled.SetSource(SourceJournal, bytes.NewReader([]byte("bcdefghi")))
	for offset := int64(0); offset < 8; offset++ {
		if _, _, err := disabled.ReplacePiece(disabled.Len(), 0, Piece{
			Source: SourceJournal, Offset: offset, Length: 1, NewlinesKnown: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if stats := disabled.Stats(); stats.AutoCompactPieces != 0 ||
		stats.NextAutoCompactPieces != 0 || stats.AutoCompactions != 0 ||
		stats.PieceCount != 9 || !stats.LineBreaksKnown || stats.LineBreaks != 1 {
		t.Fatalf("disabled Stats = %+v", stats)
	}
	if _, _, err := disabled.ReplacePiece(1, 0, Piece{
		Source: SourceJournal, Offset: 0, Length: 1,
	}); err != nil {
		t.Fatal(err)
	}
	lines, known := disabled.LineBreaks()
	if stats := disabled.Stats(); stats.LineBreaks != lines ||
		stats.LineBreaksKnown != known || stats.LineBreaksKnown {
		t.Fatalf("unknown-line Stats = %+v, LineBreaks = (%d, %v)", stats, lines, known)
	}

	for _, options := range []Options{
		{AutoCompactPieces: -1},
		{AutoCompactPieces: 1},
		{AutoCompactPieces: 2, DisableAutoCompact: true},
	} {
		if _, err := NewWithOptions(nil, 0, options); !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("NewWithOptions(%+v) = %v", options, err)
		}
	}

	disabled.mu.Lock()
	disabled.scheduleNextAutoCompactLocked(math.MaxInt64)
	if disabled.nextAutoCompactPieces != 0 {
		t.Fatalf("disabled next threshold = %d", disabled.nextAutoCompactPieces)
	}
	disabled.mu.Unlock()

	saturating, err := NewWithOptions(nil, 0, Options{AutoCompactPieces: 2})
	if err != nil {
		t.Fatal(err)
	}
	saturating.mu.Lock()
	saturating.scheduleNextAutoCompactLocked(math.MaxInt64)
	if saturating.nextAutoCompactPieces != math.MaxInt64 {
		t.Fatalf("saturated next threshold = %d", saturating.nextAutoCompactPieces)
	}
	saturating.mu.Unlock()
}

func TestRestoreRearmsAutomaticCompactionWithoutChangingSnapshot(t *testing.T) {
	fragmented, err := NewWithOptions(nil, 0, Options{DisableAutoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	fragmented.SetSource(SourceJournal, bytes.NewReader([]byte("abcde")))
	for offset := int64(0); offset < 4; offset++ {
		if _, _, err := fragmented.ReplacePiece(fragmented.Len(), 0, Piece{
			Source: SourceJournal, Offset: offset, Length: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := fragmented.Snapshot()

	tree, err := NewWithOptions(nil, 0, Options{AutoCompactPieces: 4})
	if err != nil {
		t.Fatal(err)
	}
	tree.Restore(snapshot)
	if tree.root != snapshot.root {
		t.Fatal("Restore changed the Snapshot root")
	}
	if stats := tree.Stats(); stats.PieceCount != 4 || stats.NextAutoCompactPieces != 4 ||
		stats.AutoCompactions != 0 {
		t.Fatalf("restored Stats = %+v", stats)
	}
	if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
		Source: SourceJournal, Offset: 4, Length: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if stats := tree.Stats(); stats.PieceCount != 1 || stats.AutoCompactions != 1 {
		t.Fatalf("post-Restore compaction Stats = %+v", stats)
	}
	assertSnapshot(t, snapshot, "abcd")
	assertSnapshot(t, tree.Snapshot(), "abcde")
}

func TestConcurrentSnapshotsRemainStableAcrossAutoCompaction(t *testing.T) {
	const bytesCount = 512
	tree, err := NewWithOptions(nil, 0, Options{AutoCompactPieces: 8})
	if err != nil {
		t.Fatal(err)
	}
	tree.SetSource(SourceJournal, bytes.NewReader(bytes.Repeat([]byte{'x'}, bytesCount)))

	done := make(chan struct{})
	failures := make(chan string, 8)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				snapshot := tree.Snapshot()
				if snapshot.Len() == 0 {
					continue
				}
				buffer := make([]byte, snapshot.Len())
				n, readErr := snapshot.ReadAt(buffer, 0)
				if int64(n) != snapshot.Len() || readErr != nil ||
					!bytes.Equal(buffer, bytes.Repeat([]byte{'x'}, len(buffer))) {
					failures <- "Snapshot changed across automatic compaction"
					return
				}
			}
		}()
	}
	for offset := int64(0); offset < bytesCount; offset++ {
		if _, _, err := tree.ReplacePiece(tree.Len(), 0, Piece{
			Source: SourceJournal, Offset: offset, Length: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	readers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if stats := tree.Stats(); stats.PieceCount >= 8 || stats.AutoCompactions == 0 {
		t.Fatalf("final automatic-compaction Stats = %+v", stats)
	}
	assertSnapshot(t, tree.Snapshot(), string(bytes.Repeat([]byte{'x'}, bytesCount)))
	assertTreeInvariants(t, tree)
}
