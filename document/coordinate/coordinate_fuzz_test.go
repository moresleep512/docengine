package coordinate

import (
	"context"
	"errors"
	"testing"
	"unicode/utf8"
)

func FuzzIndexMatchesUTF8Reference(f *testing.F) {
	f.Add([]byte("hello\n世界🙂"), uint16(7))
	f.Add([]byte{}, uint16(1))
	f.Add([]byte{0xff}, uint16(64))
	f.Fuzz(func(t *testing.T, body []byte, checkpoint uint16) {
		if len(body) > 4096 {
			body = body[:4096]
		}
		checkpointBytes := int64(checkpoint%257 + 1)
		index, err := Build(context.Background(), &testSource{body: append([]byte(nil), body...)}, 42, Options{CheckpointBytes: checkpointBytes})
		if !utf8.Valid(body) {
			if !errors.Is(err, ErrInvalidUTF8) {
				t.Fatalf("invalid UTF-8 Build = %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer index.Close()
		positions := referencePositions(body)
		if stats := index.Stats(); stats.ByteLength != int64(len(body)) || stats.RuneCount != int64(len(positions)-1) || stats.Revision != 42 {
			t.Fatalf("Stats = %+v", stats)
		}
		boundaries := make(map[int64]Position, len(positions))
		for _, want := range positions {
			boundaries[want.ByteOffset] = want
			got, err := index.ByteToPosition(context.Background(), want.ByteOffset)
			if err != nil || got != want {
				t.Fatalf("ByteToPosition(%d) = (%+v, %v), want %+v", want.ByteOffset, got, err, want)
			}
			byteOffset, err := index.RuneToByte(context.Background(), want.RuneOffset)
			if err != nil || byteOffset != want.ByteOffset {
				t.Fatalf("RuneToByte(%d) = (%d, %v), want %d", want.RuneOffset, byteOffset, err, want.ByteOffset)
			}
			byteOffset, err = index.PositionToByte(context.Background(), want.Line, want.Column)
			if err != nil || byteOffset != want.ByteOffset {
				t.Fatalf("PositionToByte(%d,%d) = (%d, %v), want %d", want.Line, want.Column, byteOffset, err, want.ByteOffset)
			}
		}
		for offset := int64(0); offset <= int64(len(body)); offset++ {
			if _, boundary := boundaries[offset]; boundary {
				continue
			}
			if _, err := index.ByteToPosition(context.Background(), offset); !errors.Is(err, ErrNotRuneBoundary) {
				t.Fatalf("non-boundary %d = %v", offset, err)
			}
		}
	})
}

func FuzzChangeMapBoundsAndComposition(f *testing.F) {
	f.Add(uint16(10), []byte{2, 3, 1, 6, 0, 2})
	f.Add(uint16(0), []byte{})
	f.Fuzz(func(t *testing.T, initial uint16, program []byte) {
		if len(program) > 768 {
			program = program[:768]
		}
		length := int64(initial % 257)
		edits := make([]Edit, 0, len(program)/3)
		current := length
		for position := 0; position+2 < len(program); position += 3 {
			start := int64(program[position]) % (current + 1)
			oldLength := int64(program[position+1])
			if oldLength > current-start {
				oldLength = current - start
			}
			newLength := int64(program[position+2] % 9)
			edits = append(edits, Edit{Start: start, OldLength: oldLength, NewLength: newLength})
			current = current - oldLength + newLength
		}
		afterRevision := uint64(len(edits))
		change, err := NewChangeMap(0, afterRevision, length, edits)
		if len(edits) == 0 {
			change, err = Identity(0, length)
		}
		if err != nil {
			t.Fatal(err)
		}
		if change.AfterLength() != current {
			t.Fatalf("AfterLength = %d, want %d", change.AfterLength(), current)
		}
		for offset := int64(0); offset <= length; offset++ {
			for _, affinity := range []Affinity{AffinityBefore, AffinityAfter} {
				got, err := change.Transform(Anchor{Offset: offset, Affinity: affinity})
				if err != nil || got.Offset < 0 || got.Offset > current || got.Affinity != affinity {
					t.Fatalf("Transform(%d,%d) = (%+v,%v), after=%d", offset, affinity, got, err, current)
				}
			}
		}
		inverse := change.Invert()
		composed, err := change.Compose(inverse)
		if err != nil || composed.BeforeLength() != length || composed.AfterLength() != length {
			t.Fatalf("compose inverse = (%+v,%v)", composed, err)
		}
	})
}

func FuzzIncrementalIndexMatchesFullBuild(f *testing.F) {
	f.Add([]byte("first\nsecond🙂"), []byte{9, 1, 2, 7, 0, 4, 3, 5}, uint8(7))
	f.Add([]byte{}, []byte{}, uint8(1))
	f.Add([]byte{0xff, 0, 1, 2}, []byte{0, 0, 3, 4}, uint8(31))
	f.Fuzz(func(t *testing.T, raw, program []byte, checkpoint uint8) {
		if len(raw) > 64 {
			raw = raw[:64]
		}
		if len(program) > 48 {
			program = program[:48]
		}
		before := normalizedFuzzUTF8(raw)
		current := append([]byte(nil), before...)
		edits := make([]Edit, 0, len(program)/4)
		for cursor := 0; cursor+3 < len(program); cursor += 4 {
			positions := referencePositions(current)
			startIndex := int(program[cursor]) % len(positions)
			endIndex := startIndex + int(program[cursor+1])%(len(positions)-startIndex)
			insert := normalizedFuzzUTF8(program[cursor+2 : cursor+4])
			edit := Edit{
				Start: positions[startIndex].ByteOffset, OldLength: positions[endIndex].ByteOffset - positions[startIndex].ByteOffset,
				NewLength: int64(len(insert)),
			}
			current = replaceReference(current, edit, insert)
			edits = append(edits, edit)
		}

		checkpointBytes := int64(checkpoint%32) + 1
		previous, err := Build(context.Background(), &testSource{body: before}, 5, Options{CheckpointBytes: checkpointBytes})
		if err != nil {
			t.Fatal(err)
		}
		defer previous.Close()
		var changes ChangeMap
		if len(edits) == 0 {
			changes, err = Identity(5, int64(len(before)))
		} else {
			changes, err = NewChangeMap(5, 5+uint64(len(edits)), int64(len(before)), edits)
		}
		if err != nil {
			t.Fatal(err)
		}
		incremental, err := Rebuild(context.Background(), &testSource{body: current}, previous, changes)
		if err != nil {
			t.Fatal(err)
		}
		defer incremental.Close()
		fresh, err := Build(context.Background(), &testSource{body: current}, changes.AfterRevision(), Options{CheckpointBytes: checkpointBytes})
		if err != nil {
			t.Fatal(err)
		}
		defer fresh.Close()
		assertIndexesEquivalent(t, current, incremental, fresh)
	})
}

func normalizedFuzzUTF8(input []byte) []byte {
	tokens := [...]string{"a", "\n", "é", "界", "🙂", "\r"}
	result := make([]byte, 0, len(input)*2)
	for _, value := range input {
		result = append(result, tokens[int(value)%len(tokens)]...)
	}
	return result
}
