package coordinate

import (
	"errors"
	"math"
	"testing"
)

func TestChangeMapSequentialTransformComposeAndInvert(t *testing.T) {
	first, err := NewChangeMap(10, 12, 10, []Edit{
		{Start: 2, OldLength: 3, NewLength: 1},
		{Start: 6, OldLength: 0, NewLength: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.BeforeRevision() != 10 || first.AfterRevision() != 12 || first.BeforeLength() != 10 || first.AfterLength() != 10 || first.Len() != 2 {
		t.Fatalf("map metadata is wrong")
	}
	edits := first.Edits()
	edits[0].Start = 99
	if first.Edits()[0].Start != 2 {
		t.Fatal("Edits exposed internal storage")
	}

	tests := []struct {
		anchor Anchor
		want   int64
	}{
		{Anchor{Offset: 0, Affinity: AffinityBefore}, 0},
		{Anchor{Offset: 2, Affinity: AffinityBefore}, 2},
		{Anchor{Offset: 2, Affinity: AffinityAfter}, 3},
		{Anchor{Offset: 4, Affinity: AffinityBefore}, 2},
		{Anchor{Offset: 4, Affinity: AffinityAfter}, 3},
		{Anchor{Offset: 5, Affinity: AffinityBefore}, 3},
		{Anchor{Offset: 8, Affinity: AffinityBefore}, 6},
		{Anchor{Offset: 8, Affinity: AffinityAfter}, 8},
		{Anchor{Offset: 10, Affinity: AffinityAfter}, 10},
	}
	for _, test := range tests {
		got, err := first.Transform(test.anchor)
		if err != nil || got.Offset != test.want || got.Affinity != test.anchor.Affinity {
			t.Fatalf("Transform(%+v) = (%+v, %v), want %d", test.anchor, got, err, test.want)
		}
	}

	second, err := NewChangeMap(12, 13, first.AfterLength(), []Edit{{Start: 1, OldLength: 1, NewLength: 3}})
	if err != nil {
		t.Fatal(err)
	}
	composed, err := first.Compose(second)
	if err != nil {
		t.Fatal(err)
	}
	anchor := Anchor{Offset: 8, Affinity: AffinityAfter}
	throughFirst, _ := first.Transform(anchor)
	want, _ := second.Transform(throughFirst)
	got, err := composed.Transform(anchor)
	if err != nil || got != want || composed.BeforeRevision() != 10 || composed.AfterRevision() != 13 || composed.AfterLength() != 12 {
		t.Fatalf("composed transform = (%+v, %v), want %+v", got, err, want)
	}

	inverse := first.Invert()
	if inverse.BeforeRevision() != 12 || inverse.AfterRevision() != 10 || inverse.BeforeLength() != 10 || inverse.AfterLength() != 10 {
		t.Fatalf("inverse metadata is wrong")
	}
	if got := inverse.Edits(); len(got) != 2 || got[0] != (Edit{Start: 6, OldLength: 2, NewLength: 0}) || got[1] != (Edit{Start: 2, OldLength: 1, NewLength: 3}) {
		t.Fatalf("inverse edits = %+v", got)
	}
}

func TestChangeMapInsertionAffinityAndRanges(t *testing.T) {
	change, err := NewChangeMap(1, 2, 5, []Edit{{Start: 2, NewLength: 3}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		offset   int64
		affinity Affinity
		want     int64
	}{
		{1, AffinityBefore, 1},
		{2, AffinityBefore, 2},
		{2, AffinityAfter, 5},
		{3, AffinityBefore, 6},
	} {
		got, err := change.Transform(Anchor{Offset: test.offset, Affinity: test.affinity})
		if err != nil || got.Offset != test.want {
			t.Fatalf("Transform(%d,%d) = (%+v,%v)", test.offset, test.affinity, got, err)
		}
	}
	value := AnchoredRange{
		Start: Anchor{Offset: 2, Affinity: AffinityBefore},
		End:   Anchor{Offset: 2, Affinity: AffinityAfter},
	}
	got, err := change.TransformRange(value)
	if err != nil || got.Start.Offset != 2 || got.End.Offset != 5 {
		t.Fatalf("TransformRange = (%+v, %v)", got, err)
	}
	inverted := AnchoredRange{
		Start: Anchor{Offset: 2, Affinity: AffinityAfter},
		End:   Anchor{Offset: 2, Affinity: AffinityBefore},
	}
	if _, err := change.TransformRange(inverted); !errors.Is(err, ErrInvertedRange) {
		t.Fatalf("inverted transformed range = %v", err)
	}
	if _, err := change.TransformRange(AnchoredRange{Start: Anchor{Offset: 3, Affinity: AffinityBefore}, End: Anchor{Offset: 2, Affinity: AffinityAfter}}); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("invalid input range = %v", err)
	}
}

func TestChangeMapTransformsAnchorBatchAtomically(t *testing.T) {
	change, err := NewChangeMap(3, 5, 8, []Edit{
		{Start: 2, OldLength: 2, NewLength: 4},
		{Start: 7, OldLength: 1, NewLength: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	anchors := []Anchor{
		{Offset: 8, Affinity: AffinityAfter},
		{Offset: 2, Affinity: AffinityBefore},
		{Offset: 2, Affinity: AffinityAfter},
		{Offset: 0, Affinity: AffinityBefore},
	}
	original := append([]Anchor(nil), anchors...)
	transformed, err := change.TransformAnchors(anchors)
	if err != nil {
		t.Fatal(err)
	}
	for index, anchor := range anchors {
		want, transformErr := change.Transform(anchor)
		if transformErr != nil || transformed[index] != want {
			t.Fatalf("anchor %d = (%+v, %v), want %+v", index, transformed[index], transformErr, want)
		}
	}
	if len(transformed) != len(anchors) || len(anchors) != len(original) {
		t.Fatal("batch length changed")
	}
	for index := range anchors {
		if anchors[index] != original[index] {
			t.Fatal("TransformAnchors modified its input")
		}
	}
	if empty, err := change.TransformAnchors(nil); err != nil || empty != nil {
		t.Fatalf("nil batch = (%v, %v)", empty, err)
	}
	for _, invalid := range [][]Anchor{
		{{Offset: 1}},
		{{Offset: 9, Affinity: AffinityBefore}},
		{{Offset: 0, Affinity: AffinityBefore}, {Offset: -1, Affinity: AffinityAfter}},
	} {
		if got, err := change.TransformAnchors(invalid); err == nil || got != nil {
			t.Fatalf("invalid batch %+v = (%+v, %v)", invalid, got, err)
		}
	}
}

func TestChangeMapTransformsRangesAndOpaqueAnnotationsAtomically(t *testing.T) {
	change, err := NewChangeMap(1, 2, 5, []Edit{{Start: 2, NewLength: 3}})
	if err != nil {
		t.Fatal(err)
	}
	ranges := []AnchoredRange{
		{Start: Anchor{Offset: 0, Affinity: AffinityBefore}, End: Anchor{Offset: 2, Affinity: AffinityAfter}},
		{Start: Anchor{Offset: 5, Affinity: AffinityBefore}, End: Anchor{Offset: 5, Affinity: AffinityAfter}},
	}
	transformed, err := change.TransformRanges(ranges)
	if err != nil {
		t.Fatal(err)
	}
	if transformed[0].Start.Offset != 0 || transformed[0].End.Offset != 5 || transformed[1].Start.Offset != 8 || transformed[1].End.Offset != 8 {
		t.Fatalf("ranges = %+v", transformed)
	}
	if ranges[0].End.Offset != 2 {
		t.Fatal("TransformRanges modified its input")
	}
	annotations := []Annotation[map[string]int]{
		{Range: ranges[0], Value: map[string]int{"opaque": 7}},
		{Range: ranges[1], Value: map[string]int{"opaque": 9}},
	}
	mapped, err := TransformAnnotations(change, annotations)
	if err != nil || mapped[0].Value["opaque"] != 7 || mapped[1].Range != transformed[1] {
		t.Fatalf("annotations = (%+v, %v)", mapped, err)
	}
	if empty, err := change.TransformRanges(nil); err != nil || empty != nil {
		t.Fatalf("nil ranges = (%+v, %v)", empty, err)
	}
	if empty, err := TransformAnnotations[string](change, nil); err != nil || empty != nil {
		t.Fatalf("nil annotations = (%+v, %v)", empty, err)
	}

	invalid := []struct {
		value AnchoredRange
		err   error
	}{
		{AnchoredRange{Start: Anchor{Offset: 3, Affinity: AffinityBefore}, End: Anchor{Offset: 2, Affinity: AffinityAfter}}, ErrInvalidRange},
		{AnchoredRange{Start: Anchor{Offset: 0}, End: Anchor{Offset: 1, Affinity: AffinityAfter}}, ErrInvalidAffinity},
		{AnchoredRange{Start: Anchor{Offset: 0, Affinity: AffinityBefore}, End: Anchor{Offset: 6, Affinity: AffinityAfter}}, ErrInvalidOffset},
		{AnchoredRange{Start: Anchor{Offset: 2, Affinity: AffinityAfter}, End: Anchor{Offset: 2, Affinity: AffinityBefore}}, ErrInvertedRange},
	}
	for _, test := range invalid {
		if got, err := change.TransformRanges([]AnchoredRange{ranges[0], test.value}); got != nil || !errors.Is(err, test.err) {
			t.Fatalf("invalid range %+v = (%+v, %v)", test.value, got, err)
		}
		if got, err := TransformAnnotations(change, []Annotation[int]{{Range: test.value, Value: 1}}); got != nil || !errors.Is(err, test.err) {
			t.Fatalf("invalid annotation %+v = (%+v, %v)", test.value, got, err)
		}
	}
}

func TestChangeMapValidationAndCompositionErrors(t *testing.T) {
	invalid := []struct {
		beforeRevision uint64
		afterRevision  uint64
		length         int64
		edits          []Edit
	}{
		{beforeRevision: 1, afterRevision: 1, length: -1},
		{beforeRevision: 1, afterRevision: 2, length: 0},
		{length: 2, edits: []Edit{{Start: -1}}},
		{length: 2, edits: []Edit{{Start: 3}}},
		{length: 2, edits: []Edit{{Start: 1, OldLength: 2}}},
		{length: 2, edits: []Edit{{Start: 0, OldLength: -1}}},
		{length: 2, edits: []Edit{{Start: 0, NewLength: -1}}},
		{length: math.MaxInt64, edits: []Edit{{Start: 0, NewLength: 1}}},
	}
	for index, test := range invalid {
		if _, err := NewChangeMap(test.beforeRevision, test.afterRevision, test.length, test.edits); !errors.Is(err, ErrInvalidChange) {
			t.Fatalf("invalid %d = %v", index, err)
		}
	}
	identity, err := Identity(3, 5)
	if err != nil || identity.Len() != 0 || identity.AfterLength() != 5 {
		t.Fatalf("Identity = (%+v, %v)", identity, err)
	}
	if _, err := Identity(3, -1); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("invalid identity = %v", err)
	}
	if _, err := identity.Transform(Anchor{Offset: 0}); !errors.Is(err, ErrInvalidAffinity) {
		t.Fatalf("invalid affinity = %v", err)
	}
	for _, offset := range []int64{-1, 6} {
		if _, err := identity.Transform(Anchor{Offset: offset, Affinity: AffinityBefore}); !errors.Is(err, ErrInvalidOffset) {
			t.Fatalf("invalid offset %d = %v", offset, err)
		}
	}
	otherRevision, _ := Identity(4, 5)
	if _, err := identity.Compose(otherRevision); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("revision mismatch = %v", err)
	}
	otherLength, _ := NewChangeMap(3, 4, 6, []Edit{{Start: 0, NewLength: 1}})
	if _, err := identity.Compose(otherLength); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("length mismatch = %v", err)
	}
	badRange := AnchoredRange{Start: Anchor{Offset: 0, Affinity: AffinityBefore}, End: Anchor{Offset: 1}}
	if _, err := identity.TransformRange(badRange); !errors.Is(err, ErrInvalidAffinity) {
		t.Fatalf("range affinity = %v", err)
	}
	badRange = AnchoredRange{Start: Anchor{Offset: 0}, End: Anchor{Offset: 1, Affinity: AffinityAfter}}
	if _, err := identity.TransformRange(badRange); !errors.Is(err, ErrInvalidAffinity) {
		t.Fatalf("start range affinity = %v", err)
	}
}
