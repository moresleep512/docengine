package recovery

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func FuzzJournalDecoders(f *testing.F) {
	payload, _, _ := encodeBatchPayload(1, 1, []ReplaceOperation{{Inserted: []byte("seed")}})
	header := encodeBatchHeader(Batch{FirstRevision: 1, Group: 1, Operations: make([]Operation, 1)}, payload)
	f.Add(append(header, payload...))
	f.Add([]byte("short"))
	f.Fuzz(func(t *testing.T, value []byte) {
		if len(value) >= batchHeaderSize {
			batch, payloadLength, _, ok := decodeBatchHeader(value[:batchHeaderSize])
			if ok {
				if batch.FirstRevision == 0 || batch.Group == 0 || len(batch.Operations) == 0 || len(batch.Operations) > maximumBatchSize || payloadLength < 0 {
					t.Fatalf("decoder violated invariants: %+v length=%d", batch, payloadLength)
				}
			}
		}
		if len(value) >= fileHeaderSize {
			_, _ = readFileHeader(bytes.NewReader(value[:fileHeaderSize]))
		}
	})
}

func FuzzJournalStateMachine(f *testing.F) {
	f.Add([]byte{1, 1, 2, 3, 4, 5})
	f.Add([]byte{2, 255, 0, 7, 9})
	f.Fuzz(func(t *testing.T, program []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.docengine-journal-v2")
		fingerprint := testFingerprint(filepath.Join(dir, "base"), []byte("base"))
		journal, _, err := Open(path, fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		var expected []Batch
		revision := uint64(1)
		for position := 0; position < len(program); position++ {
			switch program[position] % 5 {
			case 0, 1:
				inserted := []byte{program[position]}
				if _, err := journal.AppendBatch(revision, revision, []ReplaceOperation{{Start: int64(position), Inserted: inserted}}); err != nil {
					t.Fatal(err)
				}
				expected = append(expected, Batch{FirstRevision: revision, Group: revision, Operations: []Operation{{Start: int64(position), InsertLength: 1}}})
				revision++
			case 2:
				if err := journal.Sync(); err != nil {
					t.Fatal(err)
				}
			case 3:
				replay, err := journal.Replay()
				if err != nil || len(replay.Batches) != len(expected) {
					t.Fatalf("Replay = (%+v, %v), expected %d", replay, err, len(expected))
				}
			case 4:
				if err := journal.Close(); err != nil {
					t.Fatal(err)
				}
				journal, _, err = Open(path, fingerprint)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		replay, err := journal.Replay()
		if err != nil || len(replay.Batches) != len(expected) {
			t.Fatalf("final Replay = (%+v, %v), expected %d", replay, err, len(expected))
		}
		for index, batch := range replay.Batches {
			if batch.FirstRevision != expected[index].FirstRevision || batch.Group != expected[index].Group || len(batch.Operations) != 1 {
				t.Fatalf("batch %d = %+v", index, batch)
			}
		}
		_ = journal.Close()

		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) > fileHeaderSize && len(program) > 0 {
			cut := fileHeaderSize + int(program[0])%(len(content)-fileHeaderSize)
			truncated := append([]byte(nil), content[:cut]...)
			candidate := filepath.Join(dir, "truncated.docengine-journal-v2")
			if err := os.WriteFile(candidate, truncated, 0o600); err != nil {
				t.Fatal(err)
			}
			opened, result, err := Open(candidate, fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			wantTruncated := result.ValidBytes < int64(cut)
			if result.Truncated != wantTruncated || result.ValidBytes > int64(cut) || len(result.Batches) > len(expected) {
				t.Fatalf("truncated replay = %+v, cut=%d", result, cut)
			}
			for index, batch := range result.Batches {
				want := expected[index]
				if batch.FirstRevision != want.FirstRevision || batch.Group != want.Group || len(batch.Operations) != len(want.Operations) {
					t.Fatalf("truncated batch %d = %+v, want %+v", index, batch, want)
				}
			}
		}
	})
}

func TestBatchCRCIncludesHeaderAndPayload(t *testing.T) {
	payload := make([]byte, batchRecordSize+1)
	binary.LittleEndian.PutUint64(payload[16:24], 1)
	payload[len(payload)-1] = 'x'
	header := encodeBatchHeader(Batch{FirstRevision: 1, Group: 1, Operations: make([]Operation, 1)}, payload)
	stored := binary.LittleEndian.Uint32(header[56:60])
	calculated := crc32.Update(0, castagnoli, header[:56])
	calculated = crc32.Update(calculated, castagnoli, payload)
	if stored != calculated {
		t.Fatalf("CRC = %08x, want %08x", stored, calculated)
	}
}
