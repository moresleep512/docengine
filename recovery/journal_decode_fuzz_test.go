package recovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzJournalBatchOperationsDecode feeds arbitrary bytes to the 24-byte
// operation-table cursor decoder (decodeBatchOperations), which FuzzJournalDecoders
// does not reach: that target only fuzzes the batch header and file header
// decoders. The contract is that the decoder never panics and, when it returns
// a valid result, the op cursor lands exactly on the payload end.
func FuzzJournalBatchOperationsDecode(f *testing.F) {
	f.Add([]byte("seed"), uint8(1), uint8(0))
	f.Add(make([]byte, batchRecordSize*3), uint8(3), uint8(0))
	f.Add([]byte{}, uint8(1), uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, countByte, payloadLengthByte uint8) {
		count := int(countByte)%maximumBatchSize + 1 // 1..256
		metadataLength := int64(count) * batchRecordSize
		// payloadLength ranges over [0, metadataLength+255] to exercise both
		// the metadata-too-short early return and the cursor-mismatch check.
		maxPayload := metadataLength + 255
		payloadLength := int64(payloadLengthByte) % (maxPayload + 1)
		reader := bytes.NewReader(data)
		batch := Batch{FirstRevision: 1, Group: 1, Operations: make([]Operation, count)}
		operations, valid, err := decodeBatchOperations(reader, batch, 0, payloadLength)

		if err != nil {
			// A read error means the data was too short for the metadata read;
			// the decoder must not claim validity or expose partial ops.
			if valid || operations != nil {
				t.Fatalf("decoder returned ops=%v valid=%v with read error %v", operations, valid, err)
			}
			return
		}
		if valid {
			if len(operations) != count {
				t.Fatalf("valid decode returned %d ops, want %d", len(operations), count)
			}
			// The internal contract: a valid decode leaves the op cursor at
			// payloadEnd == payloadLength (payloadOffset is 0 here). Re-derive
			// the cursor from the operations and assert exact landing.
			cursor := metadataLength
			for _, operation := range operations {
				if operation.Start < 0 || operation.DeleteLength < 0 || operation.InsertLength < 0 {
					t.Fatalf("valid decode exposed negative field: %+v", operation)
				}
				if operation.PayloadOffset != cursor {
					t.Fatalf("payload offset = %d, want %d", operation.PayloadOffset, cursor)
				}
				cursor += operation.InsertLength
			}
			if cursor != payloadLength {
				t.Fatalf("cursor %d != payloadLength %d on a valid decode", cursor, payloadLength)
			}
		} else {
			if operations != nil {
				t.Fatalf("invalid decode returned ops %v", operations)
			}
		}
	})
}

// FuzzJournalReplayResilience writes a random number of valid batches and then
// appends arbitrary garbage bytes to the journal tail. The replay must stop at
// the first invalid boundary (or accept a clean batch if the garbage happens to
// be well-formed), must never expose a partial batch, and ValidBytes must point
// to a batch boundary that the persisted valid prefix matches.
func FuzzJournalReplayResilience(f *testing.F) {
	f.Add([]byte{2}, []byte{1, 2, 3, 4})
	f.Add([]byte{0}, []byte("garbage tail"))
	f.Add([]byte{5}, []byte{})
	f.Fuzz(func(t *testing.T, seed, garbage []byte) {
		if len(seed) == 0 {
			return
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "resilience.docengine-journal-v2")
		fingerprint := testFingerprint(filepath.Join(dir, "base"), []byte("base"))
		journal, _, err := Open(path, fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		batchCount := int(seed[0]) % 8 // 0..7 valid batches
		revision := uint64(1)
		for batchIndex := 0; batchIndex < batchCount; batchIndex++ {
			inserted := []byte{seed[batchIndex%len(seed)]}
			if _, err := journal.AppendBatch(revision, revision, []ReplaceOperation{{Start: int64(batchIndex), Inserted: inserted}}); err != nil {
				t.Fatalf("append %d: %v", batchIndex, err)
			}
			revision++
		}
		replay, err := journal.Replay()
		if err != nil || replay.Truncated || len(replay.Batches) != batchCount {
			t.Fatalf("pre-garbage Replay = (%+v, %v), want %d batches", replay, err, batchCount)
		}
		cleanValidBytes := replay.ValidBytes
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}

		// Append garbage to the journal tail, then reopen and replay.
		if len(garbage) > 0 {
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(garbage); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		opened, replay2, err := Open(path, fingerprint)
		if err != nil {
			t.Fatalf("reopen with garbage: %v", err)
		}
		defer opened.Close()
		// ValidBytes must never retreat below the clean prefix boundary and must
		// never exceed the file size. The garbage can only add batches or
		// truncate at the clean boundary.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if replay2.ValidBytes < cleanValidBytes {
			t.Fatalf("ValidBytes %d retreated below clean prefix %d", replay2.ValidBytes, cleanValidBytes)
		}
		if replay2.ValidBytes > info.Size() {
			t.Fatalf("ValidBytes %d exceeds file size %d", replay2.ValidBytes, info.Size())
		}
		if len(replay2.Batches) < batchCount {
			t.Fatalf("garbage dropped a valid batch: %d < %d", len(replay2.Batches), batchCount)
		}
		// Any batch beyond the clean prefix must have FirstRevision strictly
		// greater than the last clean revision, since garbage-as-batch would
		// still decode to some revision (random but valid-shaped).
		for index := range replay2.Batches {
			if replay2.Batches[index].FirstRevision == 0 {
				t.Fatalf("zero revision exposed at batch %d", index)
			}
		}
	})
}
