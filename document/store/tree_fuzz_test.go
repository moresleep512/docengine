package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func FuzzTreeMatchesReference(f *testing.F) {
	f.Add([]byte("0123456789"), []byte{0, 0, 0, 0, 3, 0, 5, 0, 2, 1, 4, 0})
	f.Add([]byte{}, []byte{0xff, 0xff, 0, 0, 1, 0, 0, 0, 0xff, 0, 8, 0})
	f.Add([]byte("a\nb\n"), bytes.Repeat([]byte{1, 0, 1, 0, 2, 0}, 32))
	f.Add(bytes.Repeat([]byte{'x'}, 128), bytes.Repeat([]byte{0xff, 0x7f, 31, 7, 8, 0}, 64))

	f.Fuzz(func(t *testing.T, base, program []byte) {
		const (
			maxBaseBytes    = 512
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
		tree := mustNewTree(t, base)
		tree.SetSource(SourceJournal, bytes.NewReader(payload))
		reference := append([]byte(nil), base...)

		type savedSnapshot struct {
			snapshot Snapshot
			content  []byte
		}
		saved := []savedSnapshot{{snapshot: tree.Snapshot(), content: append([]byte(nil), reference...)}}

		for cursor := 0; cursor+6 <= len(program); cursor += 6 {
			start := int(binary.LittleEndian.Uint16(program[cursor:cursor+2])) % (len(reference) + 1)
			maximumDelete := min(len(reference)-start, 32)
			deleteLength := int(program[cursor+2]) % (maximumDelete + 1)

			offset, insertLength := 0, 0
			if len(payload) > 0 {
				offset = int(binary.LittleEndian.Uint16(program[cursor+3:cursor+5])) % len(payload)
				maximumInsert := min(len(payload)-offset, 8)
				insertLength = int(program[cursor+5]) % (maximumInsert + 1)
			}
			if len(reference)-deleteLength+insertLength > maxDocument {
				insertLength = 0
			}

			replacement := Piece{}
			if insertLength > 0 {
				inserted := payload[offset : offset+insertLength]
				replacement = Piece{
					Source:        SourceJournal,
					Offset:        int64(offset),
					Length:        int64(insertLength),
					Newlines:      int64(bytes.Count(inserted, []byte{'\n'})),
					NewlinesKnown: true,
				}
			}

			before, after, err := tree.ReplacePiece(int64(start), int64(deleteLength), replacement)
			if err != nil {
				t.Fatalf("operation at byte %d failed: %v", cursor, err)
			}
			assertSnapshotBytes(t, before, reference)
			next := make([]byte, 0, len(reference)-deleteLength+insertLength)
			next = append(next, reference[:start]...)
			next = append(next, payload[offset:offset+insertLength]...)
			next = append(next, reference[start+deleteLength:]...)
			reference = next
			assertSnapshotBytes(t, after, reference)
			assertSnapshotBytes(t, tree.Snapshot(), reference)
			assertTreeInvariants(t, tree)
			assertSnapshotInvariants(t, before)
			assertSnapshotInvariants(t, after)

			saved = append(saved, savedSnapshot{snapshot: after, content: append([]byte(nil), reference...)})
			if len(saved) > maxSnapshots {
				saved = append(saved[:1], saved[len(saved)-maxSnapshots+1:]...)
			}
		}

		for _, item := range saved {
			assertSnapshotBytes(t, item.snapshot, item.content)
			assertSnapshotInvariants(t, item.snapshot)
		}
		assertReadAtMatchesBytesReader(t, tree.Snapshot(), reference)
		var output bytes.Buffer
		n, err := tree.Snapshot().WriteTo(&output)
		if err != nil || n != int64(len(reference)) || !bytes.Equal(output.Bytes(), reference) {
			t.Fatalf("WriteTo = (%d, %v, %x), want (%d, nil, %x)", n, err, output.Bytes(), len(reference), reference)
		}
	})
}

func assertReadAtMatchesBytesReader(t testing.TB, snapshot Snapshot, reference []byte) {
	t.Helper()
	wantReader := bytes.NewReader(reference)
	offsets := []int64{-1, 0, int64(len(reference) / 2), int64(len(reference)), int64(len(reference) + 1)}
	sizes := []int{0, 1, 7, len(reference) + 3}
	for _, offset := range offsets {
		for _, size := range sizes {
			got := make([]byte, size)
			want := make([]byte, size)
			gotN, gotErr := snapshot.ReadAt(got, offset)
			wantN, wantErr := wantReader.ReadAt(want, offset)
			if offset < 0 {
				wantN, wantErr = 0, ErrInvalidRange
			} else if size == 0 {
				wantN, wantErr = 0, nil
			}
			if gotN != wantN || !bytes.Equal(got[:gotN], want[:wantN]) || !sameReadError(gotErr, wantErr) {
				t.Fatalf("ReadAt(size=%d, offset=%d) = (%d, %x, %v), want (%d, %x, %v)",
					size, offset, gotN, got[:gotN], gotErr, wantN, want[:wantN], wantErr)
			}
		}
	}
}

func sameReadError(left, right error) bool {
	if left == nil || right == nil {
		return left == right
	}
	return errors.Is(left, right) || errors.Is(right, left) || errors.Is(left, io.EOF) && errors.Is(right, io.EOF)
}
