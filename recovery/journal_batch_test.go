package recovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendReplaceBatchReplaysOnlyAfterCompleteValidation(t *testing.T) {
	journal, path := openTestJournal(t)
	operations := []ReplaceOperation{
		{Start: 1, DeleteLength: 2, Inserted: []byte("XYZ")},
		{Start: 0, Inserted: nil},
		{Start: 4, DeleteLength: 1, Inserted: []byte("tail")},
	}
	appendResult, err := journal.AppendReplaceBatch(10, 77, operations)
	if err != nil {
		t.Fatal(err)
	}
	if appendResult.FrameOffset != fileHeaderSize || len(appendResult.PayloadOffsets) != len(operations) {
		t.Fatalf("append result = %+v", appendResult)
	}
	for index, operation := range operations {
		buffer := make([]byte, len(operation.Inserted))
		if _, err := journal.ReadAt(buffer, appendResult.PayloadOffsets[index]); err != nil {
			t.Fatalf("read payload %d: %v", index, err)
		}
		if !bytes.Equal(buffer, operation.Inserted) {
			t.Fatalf("payload %d = %q, want %q", index, buffer, operation.Inserted)
		}
	}

	replay, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	assertBatchReplay(t, replay, 10, 77, operations, appendResult.PayloadOffsets)
	expectedSize := int64(fileHeaderSize + frameHeaderSize + len(operations)*batchRecordSize + len("XYZtail"))
	if info, err := os.Stat(path); err != nil || info.Size() != expectedSize {
		t.Fatalf("journal size = (%v, %v), want %d", info, err, expectedSize)
	}

	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, reopenedReplay, err := Open(path, Fingerprint{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertBatchReplay(t, reopenedReplay, 10, 77, operations, appendResult.PayloadOffsets)
}

func TestLegacyFramesAndAtomicBatchesReplayTogether(t *testing.T) {
	journal, _ := openTestJournal(t)
	defer journal.Close()
	legacyOffset, err := journal.AppendReplaceGroup(1, 0, 0, []byte("A"), 1)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := journal.AppendReplaceBatch(2, 2, []ReplaceOperation{
		{Start: 1, Inserted: []byte("B")},
		{Start: 2, Inserted: []byte("C")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.AppendRoot(4, 0); err != nil {
		t.Fatal(err)
	}
	replay, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if replay.Truncated || len(replay.Frames) != 4 {
		t.Fatalf("replay = %+v", replay)
	}
	if replay.Frames[0].PayloadOffset != legacyOffset || replay.Frames[0].Revision != 1 {
		t.Fatalf("legacy frame = %+v", replay.Frames[0])
	}
	if replay.Frames[1].PayloadOffset != batch.PayloadOffsets[0] || replay.Frames[2].PayloadOffset != batch.PayloadOffsets[1] {
		t.Fatalf("batch frames = %+v", replay.Frames[1:3])
	}
	if replay.Frames[3].Kind != FrameRoot || replay.Frames[3].Revision != 4 {
		t.Fatalf("root frame = %+v", replay.Frames[3])
	}
}

func TestAppendReplaceBatchRejectsInvalidInputWithoutWriting(t *testing.T) {
	tooMany := make([]ReplaceOperation, maximumBatchSize+1)
	tests := []struct {
		name       string
		revision   uint64
		operations []ReplaceOperation
	}{
		{name: "empty", revision: 1},
		{name: "too many", revision: 1, operations: tooMany},
		{name: "zero revision", operations: []ReplaceOperation{{}}},
		{name: "revision overflow", revision: math.MaxUint64, operations: []ReplaceOperation{{}, {}}},
		{name: "negative start", revision: 1, operations: []ReplaceOperation{{Start: -1}}},
		{name: "negative delete", revision: 1, operations: []ReplaceOperation{{DeleteLength: -1}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal, path := openTestJournal(t)
			defer journal.Close()
			before, _ := os.Stat(path)
			if _, err := journal.AppendReplaceBatch(test.revision, 1, test.operations); !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidBatch)
			}
			after, _ := os.Stat(path)
			if after.Size() != before.Size() {
				t.Fatalf("journal grew from %d to %d", before.Size(), after.Size())
			}
		})
	}

	journal, _ := openTestJournal(t)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.AppendReplaceBatch(1, 1, []ReplaceOperation{{}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed journal error = %v, want %v", err, ErrClosed)
	}
}

func TestReplayRejectsMalformedChecksummedBatch(t *testing.T) {
	validRecord := make([]byte, batchRecordSize)
	tests := []struct {
		name    string
		frame   Frame
		payload []byte
	}{
		{name: "zero operations", frame: Frame{Kind: FrameBatch, Revision: 1, DeleteLength: batchRecordSize}},
		{name: "wrong record size", frame: Frame{Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize + 1}, payload: validRecord},
		{name: "metadata truncated", frame: Frame{Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize}, payload: validRecord[:batchRecordSize-1]},
		{name: "zero revision", frame: Frame{Kind: FrameBatch, Start: 1, DeleteLength: batchRecordSize}, payload: validRecord},
		{name: "revision overflow", frame: Frame{Kind: FrameBatch, Revision: math.MaxUint64, Start: 2, DeleteLength: batchRecordSize}, payload: make([]byte, 2*batchRecordSize)},
		{name: "trailing bytes", frame: Frame{Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize}, payload: append(append([]byte(nil), validRecord...), 1)},
	}
	negativeStart := append([]byte(nil), validRecord...)
	binary.LittleEndian.PutUint64(negativeStart[0:8], math.MaxUint64)
	tests = append(tests, struct {
		name    string
		frame   Frame
		payload []byte
	}{name: "negative start", frame: Frame{Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize}, payload: negativeStart})
	oversizedInsert := append([]byte(nil), validRecord...)
	binary.LittleEndian.PutUint64(oversizedInsert[16:24], 1)
	tests = append(tests, struct {
		name    string
		frame   Frame
		payload []byte
	}{name: "missing insert payload", frame: Frame{Kind: FrameBatch, Revision: 1, Start: 1, DeleteLength: batchRecordSize}, payload: oversizedInsert})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal, _ := openTestJournal(t)
			defer journal.Close()
			if _, err := journal.appendFrame(test.frame, test.payload); err != nil {
				t.Fatal(err)
			}
			replay, err := journal.Replay()
			if err != nil {
				t.Fatal(err)
			}
			if !replay.Truncated || len(replay.Frames) != 0 || replay.ValidBytes != fileHeaderSize {
				t.Fatalf("replay = %+v", replay)
			}
			if err := journal.RepairTail(replay.ValidBytes); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTruncatedOrCorruptBatchNeverReplaysPrefix(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, path string, appendResult BatchAppendResult)
	}{
		{
			name: "truncated payload",
			mutate: func(t *testing.T, path string, _ BatchAppendResult) {
				info, _ := os.Stat(path)
				if err := os.Truncate(path, info.Size()-1); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt payload",
			mutate: func(t *testing.T, path string, appendResult BatchAppendResult) {
				file, err := os.OpenFile(path, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if _, err := file.WriteAt([]byte{'!'}, appendResult.PayloadOffsets[0]); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal, path := openTestJournal(t)
			appendResult, err := journal.AppendReplaceBatch(1, 1, []ReplaceOperation{
				{Start: 0, Inserted: []byte("first")},
				{Start: 5, Inserted: []byte("second")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path, appendResult)
			reopened, replay, err := Open(path, Fingerprint{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if !replay.Truncated || len(replay.Frames) != 0 || replay.ValidBytes != fileHeaderSize {
				t.Fatalf("replay = %+v", replay)
			}
		})
	}
}

func TestEveryPhysicalBatchTruncationAndCorruptionIsAtomic(t *testing.T) {
	journal, path := openTestJournal(t)
	if _, err := journal.AppendReplaceBatch(1, 1, []ReplaceOperation{
		{Start: 0, Inserted: []byte("first payload")},
		{Start: 13, DeleteLength: 2, Inserted: []byte("second payload")},
		{Start: 3, DeleteLength: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	complete, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for cut := fileHeaderSize + 1; cut < len(complete); cut++ {
		if err := os.WriteFile(path, complete[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		opened, replay, err := Open(path, Fingerprint{})
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if !replay.Truncated || replay.ValidBytes != fileHeaderSize || len(replay.Frames) != 0 {
			t.Fatalf("cut %d exposed a batch prefix: %+v", cut, replay)
		}
	}

	for position := fileHeaderSize; position < len(complete); position++ {
		// Bytes 60..63 of a frame header are reserved and deliberately outside
		// the checksum. Mutating them has no semantic effect.
		frameRelative := position - fileHeaderSize
		if frameRelative >= 60 && frameRelative < frameHeaderSize {
			continue
		}
		corrupt := append([]byte(nil), complete...)
		corrupt[position] ^= 0xff
		if err := os.WriteFile(path, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		opened, replay, err := Open(path, Fingerprint{})
		if err != nil {
			t.Fatalf("corrupt byte %d: %v", position, err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if !replay.Truncated || replay.ValidBytes != fileHeaderSize || len(replay.Frames) != 0 {
			t.Fatalf("corrupt byte %d exposed a batch prefix: %+v", position, replay)
		}
	}
}

func TestMaximumOperationCountBatch(t *testing.T) {
	journal, _ := openTestJournal(t)
	defer journal.Close()
	operations := make([]ReplaceOperation, maximumBatchSize)
	result, err := journal.AppendReplaceBatch(1, 1, operations)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PayloadOffsets) != maximumBatchSize {
		t.Fatalf("offset count = %d", len(result.PayloadOffsets))
	}
	replay, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if replay.Truncated || len(replay.Frames) != maximumBatchSize || replay.Frames[maximumBatchSize-1].Revision != maximumBatchSize {
		t.Fatalf("replay summary = (%d frames, truncated=%v, last=%+v)", len(replay.Frames), replay.Truncated, replay.Frames[len(replay.Frames)-1])
	}
}

func openTestJournal(t testing.TB) (*Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.journal")
	journal, replay, err := Open(path, Fingerprint{})
	if err != nil {
		t.Fatal(err)
	}
	if replay.ValidBytes != fileHeaderSize || replay.Truncated || len(replay.Frames) != 0 {
		t.Fatalf("new journal replay = %+v", replay)
	}
	return journal, path
}

func assertBatchReplay(t testing.TB, replay ReplayResult, firstRevision, group uint64, operations []ReplaceOperation, offsets []int64) {
	t.Helper()
	if replay.Truncated || len(replay.Frames) != len(operations) {
		t.Fatalf("replay = %+v", replay)
	}
	for index, operation := range operations {
		frame := replay.Frames[index]
		if frame.Kind != FrameReplace || frame.Revision != firstRevision+uint64(index) || frame.TargetRevision != group ||
			frame.Start != operation.Start || frame.DeleteLength != operation.DeleteLength || frame.InsertLength != int64(len(operation.Inserted)) ||
			frame.PayloadOffset != offsets[index] {
			t.Fatalf("frame %d = %+v, operation = %+v, offset = %d", index, frame, operation, offsets[index])
		}
	}
}
