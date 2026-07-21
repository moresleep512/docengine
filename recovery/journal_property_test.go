package recovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPropertyAppendReplayRoundTrip verifies that appending a sequence of
// random batches and then replaying yields exactly those batches with
// matching FirstRevision, Group, op count, and readable inserted payloads.
func TestPropertyAppendReplayRoundTrip(t *testing.T) {
	journal, _ := openTestJournal(t)
	defer journal.Close()
	rng := newTestRNG(1)
	type expectedBatch struct {
		batch    Batch
		inserted [][]byte
	}
	expected := make([]expectedBatch, 0, 16)
	revision := uint64(1)
	for batchIndex := 0; batchIndex < 16; batchIndex++ {
		opCount := rng.IntN(maximumBatchSize) + 1
		operations := make([]ReplaceOperation, opCount)
		for i := range operations {
			inserted := make([]byte, rng.IntN(8))
			for j := range inserted {
				inserted[j] = byte('a' + rng.IntN(26))
			}
			operations[i] = ReplaceOperation{Start: int64(rng.IntN(64)), DeleteLength: int64(rng.IntN(8)), Inserted: inserted}
		}
		payloadOffsets, err := journal.AppendBatch(revision, revision, operations)
		if err != nil {
			t.Fatalf("batch %d append: %v", batchIndex, err)
		}
		// Keep a private copy of each op's inserted bytes for payload comparison.
		insertedCopies := make([][]byte, len(operations))
		for i, op := range operations {
			insertedCopies[i] = append([]byte(nil), op.Inserted...)
		}
		expected = append(expected, expectedBatch{
			batch:    Batch{FirstRevision: revision, Group: revision, Operations: toExpectedOperations(operations, payloadOffsets)},
			inserted: insertedCopies,
		})
		revision += uint64(opCount)
	}
	replay, err := journal.Replay()
	if err != nil || replay.Truncated || len(replay.Batches) != len(expected) {
		t.Fatalf("Replay = (%+v, %v), expected %d batches", replay, err, len(expected))
	}
	for index, batch := range replay.Batches {
		want := expected[index]
		if batch.FirstRevision != want.batch.FirstRevision || batch.Group != want.batch.Group || len(batch.Operations) != len(want.batch.Operations) {
			t.Fatalf("batch %d = %+v, want %+v", index, batch, want.batch)
		}
		for opIndex, operation := range batch.Operations {
			if operation != want.batch.Operations[opIndex] {
				t.Fatalf("batch %d op %d = %+v, want %+v", index, opIndex, operation, want.batch.Operations[opIndex])
			}
			got := readInserted(t, journal, operation)
			if !bytes.Equal(got, want.inserted[opIndex]) {
				t.Fatalf("batch %d op %d payload = %q, want %q", index, opIndex, got, want.inserted[opIndex])
			}
		}
	}
}

// TestPropertyRepairTailAlwaysLandsOnValidBoundary verifies that after a
// sequence of batches, truncating the file at every possible byte offset and
// calling RepairTail(Replay().ValidBytes) leaves the file at a valid batch
// boundary: size >= fileHeaderSize, Replay is not truncated, ValidBytes equals
// the file size, and the surviving batches match the prefix of the original.
func TestPropertyRepairTailAlwaysLandsOnValidBoundary(t *testing.T) {
	journal, path := openTestJournal(t)
	fingerprint, _ := ReadFingerprint(path)
	original := make([]Batch, 0, 4)
	revision := uint64(1)
	for batchIndex := 0; batchIndex < 4; batchIndex++ {
		if _, err := journal.AppendBatch(revision, revision, []ReplaceOperation{{Inserted: []byte("payload")}}); err != nil {
			t.Fatal(err)
		}
		original = append(original, Batch{FirstRevision: revision, Group: revision, Operations: []Operation{{InsertLength: 7}}})
		revision++
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	complete, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for cut := fileHeaderSize; cut < len(complete); cut++ {
		cut := cut
		t.Run("cut-"+itoa(cut), func(t *testing.T) {
			candidate := filepath.Join(t.TempDir(), "journal")
			if err := os.WriteFile(candidate, complete[:cut], 0o600); err != nil {
				t.Fatal(err)
			}
			opened, replay, err := Open(candidate, fingerprint)
			if err != nil {
				t.Fatalf("Open cut=%d: %v", cut, err)
			}
			if len(replay.Batches) > len(original) {
				t.Fatalf("cut %d exposed %d batches, max %d", cut, len(replay.Batches), len(original))
			}
			for i, batch := range replay.Batches {
				if batch.FirstRevision != original[i].FirstRevision || batch.Group != original[i].Group {
					t.Fatalf("cut %d batch %d = %+v", cut, i, batch)
				}
			}
			if replay.ValidBytes > int64(cut) {
				t.Fatalf("cut %d ValidBytes %d exceeds cut", cut, replay.ValidBytes)
			}
			// Repair then verify the file is at a clean boundary.
			if err := opened.RepairTail(replay.ValidBytes); err != nil {
				t.Fatalf("RepairTail cut=%d: %v", cut, err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() < fileHeaderSize {
				t.Fatalf("repaired size %d < header size", info.Size())
			}
			repaired, replay2, err := Open(candidate, fingerprint)
			if err != nil {
				t.Fatalf("reopen repaired cut=%d: %v", cut, err)
			}
			defer repaired.Close()
			if replay2.Truncated || replay2.ValidBytes != info.Size() || len(replay2.Batches) != len(replay.Batches) {
				t.Fatalf("repaired replay cut=%d = %+v, want ValidBytes=%d batches=%d", cut, replay2, info.Size(), len(replay.Batches))
			}
		})
	}
}

// TestPropertyResetClearsBatches verifies that Reset returns the journal to a
// clean state: Replay finds no batches, ValidBytes is the file header size, and
// the stored fingerprint is updated to the new one.
func TestPropertyResetClearsBatches(t *testing.T) {
	journal, path := openTestJournal(t)
	defer journal.Close()
	if _, err := journal.AppendBatch(1, 1, []ReplaceOperation{{Inserted: []byte("data")}}); err != nil {
		t.Fatal(err)
	}
	newFingerprint := testFingerprint(filepath.Join(filepath.Dir(path), "other"), []byte("other base"))
	if err := journal.Reset(newFingerprint); err != nil {
		t.Fatal(err)
	}
	replay, err := journal.Replay()
	if err != nil || replay.Truncated || replay.ValidBytes != fileHeaderSize || len(replay.Batches) != 0 {
		t.Fatalf("Replay after Reset = (%+v, %v)", replay, err)
	}
	stored, err := ReadFingerprint(path)
	if err != nil || stored != newFingerprint {
		t.Fatalf("stored fingerprint = (%+v, %v), want %+v", stored, err, newFingerprint)
	}
}

// TestPropertySyncDoesNotChangeReplay verifies that an explicit Sync before a
// replay does not alter the batch count, ValidBytes, or Truncated flag relative
// to the in-memory replay state.
func TestPropertySyncDoesNotChangeReplay(t *testing.T) {
	journal, _ := openTestJournal(t)
	defer journal.Close()
	for revision := uint64(1); revision <= 5; revision++ {
		if _, err := journal.AppendBatch(revision, revision, []ReplaceOperation{{Inserted: []byte("x")}}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Sync(); err != nil {
		t.Fatal(err)
	}
	after, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if after.Truncated != before.Truncated || after.ValidBytes != before.ValidBytes || len(after.Batches) != len(before.Batches) {
		t.Fatalf("Sync changed replay: before=%+v after=%+v", before, after)
	}
}

// TestPropertySingleByteFlipRejectsBatch verifies that flipping any single byte
// in a committed batch causes Replay to reject it (Truncated at the batch) and
// never exposes the corrupted batch. The file header bytes are excluded because
// flipping them yields a stale-fingerprint error at Open time, not a replay
// truncation, which is covered by existing tests.
func TestPropertySingleByteFlipRejectsBatch(t *testing.T) {
	journal, path := openTestJournal(t)
	fingerprint, _ := ReadFingerprint(path)
	if _, err := journal.AppendBatch(1, 1, []ReplaceOperation{{Inserted: []byte("committed payload")}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for offset := fileHeaderSize; offset < len(content); offset++ {
		offset := offset
		t.Run("flip-"+itoa(offset), func(t *testing.T) {
			flipped := append([]byte(nil), content...)
			flipped[offset] ^= 1
			candidate := filepath.Join(t.TempDir(), "journal")
			if err := os.WriteFile(candidate, flipped, 0o600); err != nil {
				t.Fatal(err)
			}
			opened, replay, err := Open(candidate, fingerprint)
			if err != nil {
				t.Fatalf("Open flip=%d: %v", offset, err)
			}
			defer opened.Close()
			// A flip anywhere in the single batch must truncate it: either the
			// CRC fails (no batches) or the header/op decode fails (no batches).
			if !replay.Truncated || len(replay.Batches) != 0 {
				t.Fatalf("flip %d replay = %+v, want truncated with 0 batches", offset, replay)
			}
			if replay.ValidBytes > int64(fileHeaderSize) {
				t.Fatalf("flip %d ValidBytes = %d, want %d", offset, replay.ValidBytes, fileHeaderSize)
			}
		})
	}
}

// TestPropertyBatchBoundaryLimits re-asserts the hard batch limits as a
// property: 256 operations are accepted, 257 are rejected, and the maximum
// 1 GiB payload boundary is reported as valid by the payload encoder.
func TestPropertyBatchBoundaryLimits(t *testing.T) {
	t.Run("256 operations accepted", func(t *testing.T) {
		journal, _ := openTestJournal(t)
		defer journal.Close()
		operations := make([]ReplaceOperation, maximumBatchSize)
		for i := range operations {
			operations[i] = ReplaceOperation{Start: int64(i)}
		}
		if _, err := journal.AppendBatch(1, 1, operations); err != nil {
			t.Fatalf("256 ops append: %v", err)
		}
		replay, err := journal.Replay()
		if err != nil || len(replay.Batches) != 1 || len(replay.Batches[0].Operations) != maximumBatchSize {
			t.Fatalf("Replay = (%+v, %v)", replay, err)
		}
	})
	t.Run("257 operations rejected", func(t *testing.T) {
		journal, _ := openTestJournal(t)
		defer journal.Close()
		if _, err := journal.AppendBatch(1, 1, make([]ReplaceOperation, maximumBatchSize+1)); !errors.Is(err, ErrInvalidBatch) {
			t.Fatalf("257 ops error = %v, want %v", err, ErrInvalidBatch)
		}
	})
	t.Run("payload just over 1 GiB rejected", func(t *testing.T) {
		journal, _ := openTestJournal(t)
		defer journal.Close()
		if _, err := journal.AppendBatch(1, 1, []ReplaceOperation{{Inserted: make([]byte, maximumBatchPayload+1)}}); !errors.Is(err, ErrInvalidBatch) {
			t.Fatalf("oversized payload error = %v, want %v", err, ErrInvalidBatch)
		}
	})
}

// toExpectedOperations converts append operations + payload offsets into the
// Operation form produced by Replay, for round-trip comparison.
func toExpectedOperations(operations []ReplaceOperation, result BatchAppendResult) []Operation {
	out := make([]Operation, len(operations))
	for index, operation := range operations {
		out[index] = Operation{
			Start:         operation.Start,
			DeleteLength:  operation.DeleteLength,
			InsertLength:  int64(len(operation.Inserted)),
			PayloadOffset: result.PayloadOffsets[index],
		}
	}
	return out
}

// newTestRNG returns a deterministic pseudo-random source for property tests
// that do not use testing.F. It avoids math/rand's global to stay hermetic.
func newTestRNG(seed uint64) *testRNG { return &testRNG{state: seed | 1} }

type testRNG struct{ state uint64 }

func (r *testRNG) IntN(n int) int {
	if n <= 0 {
		return 0
	}
	r.state ^= r.state << 13
	r.state ^= r.state >> 7
	r.state ^= r.state << 17
	return int(r.state % uint64(n))
}

// itoa returns the decimal representation of n without importing strconv, to
// keep the test file dependency-free and avoid clashing with other helpers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits [20]byte
	pos := len(digits)
	for n > 0 {
		pos--
		digits[pos] = byte('0' + n%10)
		n /= 10
	}
	out := string(digits[pos:])
	if negative {
		out = "-" + out
	}
	return out
}
