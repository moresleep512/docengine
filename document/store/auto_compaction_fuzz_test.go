package store

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func FuzzTreeAutoCompactionMatchesReference(f *testing.F) {
	f.Add([]byte("base"), []byte{0, 0, 0, 0, 4, 1, 4, 0, 0, 4, 4, 1}, uint8(4))
	f.Add([]byte{}, bytes.Repeat([]byte{0, 0, 0, 0, 1, 1}, 32), uint8(2))
	f.Fuzz(func(t *testing.T, base, program []byte, thresholdSeed uint8) {
		const (
			maxBaseBytes    = 256
			maxProgramBytes = 4 << 10
			maxDocument     = 4 << 10
			maxSnapshots    = 16
		)
		if len(base) > maxBaseBytes {
			base = base[:maxBaseBytes]
		}
		if len(program) > maxProgramBytes {
			program = program[:maxProgramBytes]
		}
		base = append([]byte(nil), base...)
		payload := append([]byte(nil), program...)
		threshold := int64(2 + thresholdSeed%31)
		tree, err := NewWithOptions(bytes.NewReader(base), int64(len(base)), Options{
			AutoCompactPieces: threshold,
		})
		if err != nil {
			t.Fatal(err)
		}
		tree.SetSource(SourceJournal, bytes.NewReader(payload))
		reference := append([]byte(nil), base...)

		type retainedSnapshot struct {
			snapshot Snapshot
			content  []byte
		}
		retained := []retainedSnapshot{{
			snapshot: tree.Snapshot(), content: append([]byte(nil), reference...),
		}}
		for cursor := 0; cursor+6 <= len(program); cursor += 6 {
			start := int(binary.LittleEndian.Uint16(program[cursor:cursor+2])) % (len(reference) + 1)
			deleteLength := int(program[cursor+2]) % (min(len(reference)-start, 32) + 1)
			offset, insertLength := 0, 0
			if len(payload) != 0 {
				offset = int(binary.LittleEndian.Uint16(program[cursor+3:cursor+5])) % len(payload)
				insertLength = int(program[cursor+5]) % (min(len(payload)-offset, 8) + 1)
			}
			if len(reference)-deleteLength+insertLength > maxDocument {
				insertLength = 0
			}
			replacement := Piece{}
			if insertLength != 0 {
				replacement = Piece{
					Source: SourceJournal, Offset: int64(offset), Length: int64(insertLength),
				}
			}
			before, after, err := tree.ReplacePiece(int64(start), int64(deleteLength), replacement)
			if err != nil {
				t.Fatalf("ReplacePiece at %d: %v", cursor, err)
			}
			assertSnapshotBytes(t, before, reference)
			reference = splice(reference, start, deleteLength, payload[offset:offset+insertLength])
			assertSnapshotBytes(t, after, reference)
			assertTreeInvariants(t, tree)
			if len(retained) < maxSnapshots {
				retained = append(retained, retainedSnapshot{
					snapshot: after, content: append([]byte(nil), reference...),
				})
			} else {
				retained[(cursor/6)%maxSnapshots] = retainedSnapshot{
					snapshot: after, content: append([]byte(nil), reference...),
				}
			}
			stats := tree.Stats()
			if stats.AutoCompactPieces != threshold || stats.PieceCount != tree.PieceCount() ||
				stats.ByteLength != int64(len(reference)) ||
				stats.NextAutoCompactPieces < threshold {
				t.Fatalf("invalid automatic-compaction Stats: %+v", stats)
			}
		}
		for _, entry := range retained {
			assertSnapshotBytes(t, entry.snapshot, entry.content)
			assertSnapshotInvariants(t, entry.snapshot)
		}
		assertSnapshotBytes(t, tree.Snapshot(), reference)
	})
}
