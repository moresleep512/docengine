package document

import (
	"errors"
	"testing"

	"github.com/moresleep512/docengine/document/coordinate"
)

func FuzzChangeHistoryStateMachine(f *testing.F) {
	f.Add(uint8(3), uint8(10), []byte{1, 2, 3, 4, 5, 0, 2, 5, 7, 9})
	f.Add(uint8(1), uint8(0), []byte{})
	f.Fuzz(func(t *testing.T, rawLimit, rawLength uint8, program []byte) {
		if len(program) > 256 {
			program = program[:256]
		}
		limit := int(rawLimit%8) + 1
		length := int64(rawLength % 33)
		history := newChangeHistory(limit, 0, length)
		retained := make([]coordinate.ChangeMap, 0, limit)
		baseRevision, baseLength, currentRevision, currentLength := uint64(0), length, uint64(0), length

		check := func(from, to uint64) {
			t.Helper()
			got, gotErr := history.between(from, to)
			wantErr := referenceHistoryError(retained, baseRevision, currentRevision, from, to)
			if wantErr != nil {
				if !errors.Is(gotErr, wantErr) {
					t.Fatalf("between(%d,%d) = %v, want %v", from, to, gotErr, wantErr)
				}
				return
			}
			if gotErr != nil {
				t.Fatalf("between(%d,%d) = %v", from, to, gotErr)
			}
			fromLength, _ := referenceBoundaryLength(retained, baseRevision, baseLength, from)
			toLength, _ := referenceBoundaryLength(retained, baseRevision, baseLength, to)
			if got.BeforeRevision() != from || got.AfterRevision() != to || got.BeforeLength() != fromLength || got.AfterLength() != toLength {
				t.Fatalf("between(%d,%d) metadata = %+v, lengths %d -> %d", from, to, got, fromLength, toLength)
			}
			anchors := []coordinate.Anchor{
				{Offset: 0, Affinity: coordinate.AffinityBefore},
				{Offset: fromLength / 2, Affinity: coordinate.AffinityAfter},
				{Offset: fromLength, Affinity: coordinate.AffinityAfter},
			}
			wantAnchors := append([]coordinate.Anchor(nil), anchors...)
			if from < to {
				for _, change := range retained {
					if change.BeforeRevision() >= from && change.AfterRevision() <= to {
						wantAnchors, _ = change.TransformAnchors(wantAnchors)
					}
				}
			} else if from > to {
				for index := len(retained) - 1; index >= 0; index-- {
					change := retained[index]
					if change.BeforeRevision() >= to && change.AfterRevision() <= from {
						wantAnchors, _ = change.Invert().TransformAnchors(wantAnchors)
					}
				}
			}
			gotAnchors, err := got.TransformAnchors(anchors)
			if err != nil || !equalAnchors(gotAnchors, wantAnchors) {
				t.Fatalf("between(%d,%d) anchors = (%+v,%v), want %+v", from, to, gotAnchors, err, wantAnchors)
			}
		}

		for cursor := 0; cursor+4 < len(program); cursor += 5 {
			if program[cursor]%3 == 0 {
				span := currentRevision + 2
				check(uint64(program[cursor+1])%span, uint64(program[cursor+2])%span)
				continue
			}
			start := int64(program[cursor+1]) % (currentLength + 1)
			deleteLength := int64(program[cursor+2]) % (currentLength - start + 1)
			newLength := int64(program[cursor+3] % 8)
			revisionStep := uint64(program[cursor+4]%3) + 1
			change, err := coordinate.NewChangeMap(
				currentRevision, currentRevision+revisionStep, currentLength,
				[]coordinate.Edit{{Start: start, OldLength: deleteLength, NewLength: newLength}},
			)
			if err != nil {
				t.Fatal(err)
			}
			history.append(change)
			if len(retained) == limit {
				baseRevision, baseLength = retained[0].AfterRevision(), retained[0].AfterLength()
				copy(retained, retained[1:])
				retained[len(retained)-1] = change
			} else {
				retained = append(retained, change)
			}
			currentRevision, currentLength = change.AfterRevision(), change.AfterLength()
			stats := history.stats()
			if stats.OldestRevision != baseRevision || stats.CurrentRevision != currentRevision || stats.Entries != len(retained) || stats.Limit != limit {
				t.Fatalf("stats = %+v, base=%d current=%d retained=%d", stats, baseRevision, currentRevision, len(retained))
			}
			check(baseRevision, currentRevision)
			check(currentRevision, baseRevision)
		}
	})
}

func referenceHistoryError(retained []coordinate.ChangeMap, baseRevision, currentRevision, from, to uint64) error {
	if from < baseRevision || to < baseRevision {
		return ErrChangeHistoryExpired
	}
	if from > currentRevision || to > currentRevision {
		return ErrRevisionUnavailable
	}
	if _, ok := referenceBoundaryLength(retained, baseRevision, 0, from); !ok {
		return ErrRevisionUnavailable
	}
	if _, ok := referenceBoundaryLength(retained, baseRevision, 0, to); !ok {
		return ErrRevisionUnavailable
	}
	return nil
}

func referenceBoundaryLength(retained []coordinate.ChangeMap, baseRevision uint64, baseLength int64, revision uint64) (int64, bool) {
	if revision == baseRevision {
		return baseLength, true
	}
	for _, change := range retained {
		if revision == change.AfterRevision() {
			return change.AfterLength(), true
		}
	}
	return 0, false
}
