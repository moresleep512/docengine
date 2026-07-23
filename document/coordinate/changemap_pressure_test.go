package coordinate

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestComposeAllValidatesOnceAndPreservesSequentialOrder(t *testing.T) {
	first, err := NewChangeMap(1, 2, 6, []Edit{{Start: 1, OldLength: 2, NewLength: 1}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewChangeMap(2, 3, first.AfterLength(), []Edit{{Start: 4, NewLength: 2}})
	if err != nil {
		t.Fatal(err)
	}
	third, err := NewChangeMap(3, 5, second.AfterLength(), []Edit{{Start: 0, OldLength: 1}})
	if err != nil {
		t.Fatal(err)
	}
	composed, err := first.ComposeAll(second, third)
	if err != nil {
		t.Fatal(err)
	}
	if composed.BeforeRevision() != 1 || composed.AfterRevision() != 5 ||
		composed.BeforeLength() != 6 || composed.AfterLength() != third.AfterLength() ||
		composed.Len() != 3 {
		t.Fatalf("composed metadata = %+v", composed)
	}
	for _, anchor := range []Anchor{
		{Offset: 0, Affinity: AffinityBefore},
		{Offset: 1, Affinity: AffinityAfter},
		{Offset: 6, Affinity: AffinityAfter},
	} {
		throughFirst, _ := first.Transform(anchor)
		throughSecond, _ := second.Transform(throughFirst)
		want, _ := third.Transform(throughSecond)
		got, transformErr := composed.Transform(anchor)
		if transformErr != nil || got != want {
			t.Fatalf("anchor %+v = (%+v,%v), want %+v", anchor, got, transformErr, want)
		}
	}
	copyOnly, err := composed.ComposeAll()
	if err != nil || copyOnly.Len() != composed.Len() {
		t.Fatalf("empty ComposeAll = (%+v,%v)", copyOnly, err)
	}
	edits := copyOnly.Edits()
	edits[0] = Edit{}
	if composed.Edits()[0] == (Edit{}) {
		t.Fatal("ComposeAll result exposed edit storage")
	}
}

func TestComposeAllRejectsMiddleMismatchAndComplexity(t *testing.T) {
	base, _ := Identity(1, 3)
	revisionMismatch, _ := Identity(2, 3)
	if _, err := base.ComposeAll(revisionMismatch); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("revision mismatch = %v", err)
	}
	lengthMismatch, _ := Identity(1, 4)
	if _, err := base.ComposeAll(lengthMismatch); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("length mismatch = %v", err)
	}
	large := ChangeMap{
		beforeRevision: 1, afterRevision: 2,
		beforeLength: 0, afterLength: 0,
		edits: make([]Edit, MaximumEdits),
	}
	one := ChangeMap{
		beforeRevision: 2, afterRevision: 3,
		beforeLength: 0, afterLength: 0,
		edits: []Edit{{}},
	}
	if _, err := large.ComposeAll(one); !errors.Is(err, ErrTooComplex) {
		t.Fatalf("complex composition = %v", err)
	}
	if _, err := NewChangeMap(0, 1, 0, make([]Edit, MaximumEdits+1)); !errors.Is(err, ErrTooComplex) {
		t.Fatalf("oversized map = %v", err)
	}
}

func TestChangeMapBatchWorkLimitsAndCancellation(t *testing.T) {
	edits := make([]Edit, 1024)
	change, err := NewChangeMap(1, 2, 1, edits)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]Anchor, MaximumTransformSteps/len(edits)+1)
	for index := range tooMany {
		tooMany[index] = Anchor{Affinity: AffinityBefore}
	}
	if got, err := change.TransformAnchors(tooMany); got != nil || !errors.Is(err, ErrTooComplex) {
		t.Fatalf("anchor work limit = (%v,%v)", len(got), err)
	}
	rangeLimit := make([]AnchoredRange, MaximumTransformSteps/len(edits)/2+1)
	for index := range rangeLimit {
		rangeLimit[index] = AnchoredRange{
			Start: Anchor{Affinity: AffinityBefore},
			End:   Anchor{Affinity: AffinityAfter},
		}
	}
	if got, err := change.TransformRanges(rangeLimit); got != nil || !errors.Is(err, ErrTooComplex) {
		t.Fatalf("range work limit = (%v,%v)", len(got), err)
	}

	if _, err := change.TransformAnchorsContext(nil, nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil anchor context = %v", err)
	}
	if _, err := change.TransformRangesContext(nil, nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil range context = %v", err)
	}
	if _, err := TransformAnnotationsContext[int](nil, change, nil); !errors.Is(err, ErrInvalidContext) {
		t.Fatalf("nil annotation context = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := change.TransformAnchorsContext(canceled, nil); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled anchors = (%v,%v)", got, err)
	}
	if got, err := change.TransformRangesContext(canceled, nil); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled ranges = (%v,%v)", got, err)
	}
	if got, err := TransformAnnotationsContext[int](canceled, change, nil); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled annotations = (%v,%v)", got, err)
	}

	anchors := make([]Anchor, 4096)
	for index := range anchors {
		anchors[index] = Anchor{Offset: 1, Affinity: AffinityAfter}
	}
	polling := &pollCancelContext{cancelAfter: 4}
	if got, err := change.TransformAnchorsContext(polling, anchors); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-transform cancellation = (%v,%v), polls=%d", len(got), err, polling.polls.Load())
	}
	betweenEdits := &pollCancelContext{cancelAfter: 2}
	if got, err := change.TransformAnchorsContext(betweenEdits, anchors[:1]); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("between-edit cancellation = (%v,%v), polls=%d", len(got), err, betweenEdits.polls.Load())
	}
	if !transformTooComplex(MaximumTransformSteps, 1, 2) ||
		transformTooComplex(0, MaximumTransformSteps, 2) ||
		transformTooComplex(1, 0, 2) {
		t.Fatal("transform complexity arithmetic is not saturated")
	}

	small, _ := NewChangeMap(1, 2, 1, []Edit{{Start: 1, NewLength: 1}})
	values := []AnchoredRange{{
		Start: Anchor{Offset: 0, Affinity: AffinityBefore},
		End:   Anchor{Offset: 1, Affinity: AffinityAfter},
	}}
	if got, err := small.TransformRangesContext(context.Background(), values); err != nil || len(got) != 1 || got[0].End.Offset != 2 {
		t.Fatalf("context ranges = (%+v,%v)", got, err)
	}
	annotations := []Annotation[int]{{Range: values[0], Value: 7}}
	if got, err := TransformAnnotationsContext(context.Background(), small, annotations); err != nil || len(got) != 1 || got[0].Value != 7 || got[0].Range.End.Offset != 2 {
		t.Fatalf("context annotations = (%+v,%v)", got, err)
	}
}

func TestStableSuffixSequentialBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		before     int64
		edits      []Edit
		old, after int64
	}{
		{name: "identity", before: 10, old: 0, after: 0},
		{name: "insert at start", before: 10, edits: []Edit{{Start: 0, NewLength: 3}}, old: 0, after: 3},
		{name: "replace middle", before: 10, edits: []Edit{{Start: 2, OldLength: 3, NewLength: 1}}, old: 5, after: 3},
		{name: "later edit before suffix", before: 10, edits: []Edit{{Start: 6, OldLength: 1}, {Start: 1, NewLength: 2}}, old: 7, after: 8},
		{name: "later edit in suffix", before: 10, edits: []Edit{{Start: 2, OldLength: 1}, {Start: 5, OldLength: 2, NewLength: 1}}, old: 8, after: 6},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			afterRevision := uint64(1)
			if len(test.edits) == 0 {
				afterRevision = 0
			}
			change, err := NewChangeMap(0, afterRevision, test.before, test.edits)
			if err != nil {
				t.Fatal(err)
			}
			old, after, ok := change.stableSuffix()
			if !ok || old != test.old || after != test.after {
				t.Fatalf("stableSuffix = (%d,%d,%v), want (%d,%d,true)", old, after, ok, test.old, test.after)
			}
		})
	}
	if _, _, ok := (ChangeMap{
		beforeLength: 1, afterLength: 1,
		edits: []Edit{{Start: -1}},
	}).stableSuffix(); ok {
		t.Fatal("invalid map reported stable suffix")
	}
	if _, _, ok := (ChangeMap{
		beforeLength: 1, afterLength: 1,
		edits: []Edit{{NewLength: int64(^uint64(0) >> 1)}},
	}).stableSuffix(); ok {
		t.Fatal("overflowing map reported stable suffix")
	}
	if _, _, ok := (ChangeMap{beforeLength: 1, afterLength: 2}).stableSuffix(); ok {
		t.Fatal("length-mismatched map reported stable suffix")
	}
}

type pollCancelContext struct {
	polls       atomic.Int32
	cancelAfter int32
}

func (*pollCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*pollCancelContext) Done() <-chan struct{}       { return nil }
func (*pollCancelContext) Value(any) any               { return nil }
func (c *pollCancelContext) Err() error {
	if c.polls.Add(1) >= c.cancelAfter {
		return context.Canceled
	}
	return nil
}
