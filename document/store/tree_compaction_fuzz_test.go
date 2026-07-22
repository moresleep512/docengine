package store

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzTreeCompactionPreservesSnapshots checks the compactor against an
// in-memory byte model while retaining immutable roots from before compaction.
// It also makes idempotence and piece-count monotonicity explicit invariants.
func FuzzTreeCompactionPreservesSnapshots(f *testing.F) {
	f.Add([]byte("abcdef"), []byte{1, 0, 1, 0, 2, 3, 2, 0, 0, 1, 2, 4})
	f.Add([]byte{}, bytes.Repeat([]byte{0, 0, 0, 0, 1, 1}, 32))
	f.Add([]byte("a\nb\n"), bytes.Repeat([]byte{2, 0, 1, 0, 4, 0xff}, 64))
	f.Fuzz(func(t *testing.T, base, program []byte) {
		const (
			maxBaseBytes    = 512
			maxProgramBytes = 4 << 10
			maxDocument     = 4 << 10
			maxSnapshots    = 24
		)
		if len(base) > maxBaseBytes {
			base = base[:maxBaseBytes]
		}
		if len(program) > maxProgramBytes {
			program = program[:maxProgramBytes]
		}
		base = append([]byte(nil), base...)
		payload := append([]byte(nil), program...)
		tree := mustNewTree(t, base)
		tree.SetSource(SourceJournal, bytes.NewReader(payload))
		reference := append([]byte(nil), base...)

		type retainedSnapshot struct {
			snapshot Snapshot
			content  []byte
		}
		retained := []retainedSnapshot{{snapshot: tree.Snapshot(), content: append([]byte(nil), reference...)}}
		for cursor := 0; cursor+6 <= len(program); cursor += 6 {
			start := int(binary.LittleEndian.Uint16(program[cursor:cursor+2])) % (len(reference) + 1)
			maximumDelete := min(len(reference)-start, 32)
			deleteLength := int(program[cursor+2]) % (maximumDelete + 1)
			offset, insertLength := 0, 0
			if len(payload) > 0 {
				offset = int(binary.LittleEndian.Uint16(program[cursor+3:cursor+5])) % len(payload)
				insertLength = int(program[cursor+5]) % (min(len(payload)-offset, 8) + 1)
			}
			if len(reference)-deleteLength+insertLength > maxDocument {
				insertLength = 0
			}
			replacement := Piece{}
			if insertLength > 0 {
				inserted := payload[offset : offset+insertLength]
				replacement = Piece{Source: SourceJournal, Offset: int64(offset), Length: int64(insertLength)}
				if program[cursor+5]&1 == 0 {
					replacement.Newlines = int64(bytes.Count(inserted, []byte{'\n'}))
					replacement.NewlinesKnown = true
				}
			}
			before, after, err := tree.ReplacePiece(int64(start), int64(deleteLength), replacement)
			if err != nil {
				t.Fatalf("operation at byte %d: %v", cursor, err)
			}
			assertSnapshotBytes(t, before, reference)
			reference = splice(reference, start, deleteLength, payload[offset:offset+insertLength])
			assertSnapshotBytes(t, after, reference)
			if len(retained) < maxSnapshots || cursor%24 == 0 {
				entry := retainedSnapshot{snapshot: after, content: append([]byte(nil), reference...)}
				if len(retained) < maxSnapshots {
					retained = append(retained, entry)
				} else {
					retained[(cursor/6)%maxSnapshots] = entry
				}
			}
		}

		beforePieces := tree.PieceCount()
		first := tree.Compact()
		if first.BeforePieces != beforePieces || first.AfterPieces != tree.PieceCount() || first.AfterPieces > first.BeforePieces {
			t.Fatalf("first Compact = %+v, before=%d current=%d", first, beforePieces, tree.PieceCount())
		}
		assertSnapshotBytes(t, tree.Snapshot(), reference)
		assertTreeInvariants(t, tree)
		for _, entry := range retained {
			assertSnapshotBytes(t, entry.snapshot, entry.content)
			assertSnapshotInvariants(t, entry.snapshot)
		}

		second := tree.Compact()
		if second.BeforePieces != first.AfterPieces || second.AfterPieces != first.AfterPieces {
			t.Fatalf("non-idempotent Compact: first=%+v second=%+v", first, second)
		}
		assertSnapshotBytes(t, tree.Snapshot(), reference)
		assertTreeInvariants(t, tree)
	})
}
