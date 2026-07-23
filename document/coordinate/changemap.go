package coordinate

import (
	"context"
	"errors"
	"math"
)

const (
	// MaximumEdits bounds one immutable ChangeMap, including a composed map.
	MaximumEdits = 1 << 20
	// MaximumTransformSteps bounds edit-by-anchor work in one batch call.
	MaximumTransformSteps = 16 << 20
)

var (
	ErrInvalidChange    = errors.New("coordinate: invalid change")
	ErrInvalidAffinity  = errors.New("coordinate: invalid anchor affinity")
	ErrInvalidRange     = errors.New("coordinate: invalid range")
	ErrInvertedRange    = errors.New("coordinate: transformed range is inverted")
	ErrRevisionMismatch = errors.New("coordinate: revision mismatch")
	ErrLengthMismatch   = errors.New("coordinate: document length mismatch")
	ErrTooComplex       = errors.New("coordinate: complexity limit exceeded")
)

type Affinity uint8

const (
	AffinityBefore Affinity = iota + 1
	AffinityAfter
)

type Anchor struct {
	Offset   int64
	Affinity Affinity
}

type AnchoredRange struct {
	Start Anchor
	End   Anchor
}

// Annotation attaches an opaque host value to a format-neutral anchored
// range. The coordinate package never interprets Value.
type Annotation[T any] struct {
	Range AnchoredRange
	Value T
}

// Edit uses coordinates in the document state produced by all preceding edits
// in the same ChangeMap.
type Edit struct {
	Start     int64
	OldLength int64
	NewLength int64
}

type ChangeMap struct {
	beforeRevision uint64
	afterRevision  uint64
	beforeLength   int64
	afterLength    int64
	edits          []Edit
}

func Identity(revision uint64, length int64) (ChangeMap, error) {
	return NewChangeMap(revision, revision, length, nil)
}

func NewChangeMap(beforeRevision, afterRevision uint64, beforeLength int64, edits []Edit) (ChangeMap, error) {
	if beforeLength < 0 || len(edits) == 0 && beforeRevision != afterRevision {
		return ChangeMap{}, ErrInvalidChange
	}
	if len(edits) > MaximumEdits {
		return ChangeMap{}, ErrTooComplex
	}
	length := beforeLength
	copyOfEdits := append([]Edit(nil), edits...)
	for _, edit := range copyOfEdits {
		if edit.Start < 0 || edit.OldLength < 0 || edit.NewLength < 0 || edit.Start > length || edit.OldLength > length-edit.Start {
			return ChangeMap{}, ErrInvalidChange
		}
		remaining := length - edit.OldLength
		if edit.NewLength > math.MaxInt64-remaining {
			return ChangeMap{}, ErrInvalidChange
		}
		length = remaining + edit.NewLength
	}
	return ChangeMap{
		beforeRevision: beforeRevision, afterRevision: afterRevision,
		beforeLength: beforeLength, afterLength: length, edits: copyOfEdits,
	}, nil
}

func (m ChangeMap) BeforeRevision() uint64 { return m.beforeRevision }
func (m ChangeMap) AfterRevision() uint64  { return m.afterRevision }
func (m ChangeMap) BeforeLength() int64    { return m.beforeLength }
func (m ChangeMap) AfterLength() int64     { return m.afterLength }
func (m ChangeMap) Len() int               { return len(m.edits) }
func (m ChangeMap) Edits() []Edit          { return append([]Edit(nil), m.edits...) }

func (m ChangeMap) Transform(anchor Anchor) (Anchor, error) {
	if anchor.Affinity != AffinityBefore && anchor.Affinity != AffinityAfter {
		return Anchor{}, ErrInvalidAffinity
	}
	if anchor.Offset < 0 || anchor.Offset > m.beforeLength {
		return Anchor{}, ErrInvalidOffset
	}
	offset := anchor.Offset
	for _, edit := range m.edits {
		offset = transformOffset(offset, anchor.Affinity, edit)
	}
	return Anchor{Offset: offset, Affinity: anchor.Affinity}, nil
}

// TransformAnchors applies this map to an anchor batch while preserving input
// order. Validation is atomic: an invalid anchor returns no partial result.
func (m ChangeMap) TransformAnchors(anchors []Anchor) ([]Anchor, error) {
	return m.transformAnchors(nil, anchors)
}

// TransformAnchorsContext is TransformAnchors with cancellation between
// bounded units of edit-by-anchor work.
func (m ChangeMap) TransformAnchorsContext(ctx context.Context, anchors []Anchor) ([]Anchor, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.transformAnchors(ctx, anchors)
}

func (m ChangeMap) transformAnchors(ctx context.Context, anchors []Anchor) ([]Anchor, error) {
	if transformTooComplex(len(m.edits), len(anchors), 1) {
		return nil, ErrTooComplex
	}
	result := append([]Anchor(nil), anchors...)
	for _, anchor := range result {
		if anchor.Affinity != AffinityBefore && anchor.Affinity != AffinityAfter {
			return nil, ErrInvalidAffinity
		}
		if anchor.Offset < 0 || anchor.Offset > m.beforeLength {
			return nil, ErrInvalidOffset
		}
	}
	for _, edit := range m.edits {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for index := range result {
			if ctx != nil && index&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			result[index].Offset = transformOffset(result[index].Offset, result[index].Affinity, edit)
		}
	}
	return result, nil
}

func (m ChangeMap) TransformRange(value AnchoredRange) (AnchoredRange, error) {
	if value.Start.Offset > value.End.Offset {
		return AnchoredRange{}, ErrInvalidRange
	}
	start, err := m.Transform(value.Start)
	if err != nil {
		return AnchoredRange{}, err
	}
	end, err := m.Transform(value.End)
	if err != nil {
		return AnchoredRange{}, err
	}
	if start.Offset > end.Offset {
		return AnchoredRange{}, ErrInvertedRange
	}
	return AnchoredRange{Start: start, End: end}, nil
}

// TransformRanges applies this map to a range batch while preserving input
// order. Validation is atomic: invalid input or an inverted transformed range
// returns no partial result.
func (m ChangeMap) TransformRanges(values []AnchoredRange) ([]AnchoredRange, error) {
	return m.transformRanges(nil, values)
}

// TransformRangesContext is TransformRanges with cancellation and the same
// all-or-nothing validation contract.
func (m ChangeMap) TransformRangesContext(ctx context.Context, values []AnchoredRange) ([]AnchoredRange, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.transformRanges(ctx, values)
}

func (m ChangeMap) transformRanges(ctx context.Context, values []AnchoredRange) ([]AnchoredRange, error) {
	if values == nil {
		return nil, nil
	}
	if transformTooComplex(len(m.edits), len(values), 2) {
		return nil, ErrTooComplex
	}
	anchors := make([]Anchor, 0, len(values)*2)
	for _, value := range values {
		if value.Start.Offset > value.End.Offset {
			return nil, ErrInvalidRange
		}
		anchors = append(anchors, value.Start, value.End)
	}
	var (
		transformed []Anchor
		err         error
	)
	if ctx == nil {
		transformed, err = m.TransformAnchors(anchors)
	} else {
		transformed, err = m.TransformAnchorsContext(ctx, anchors)
	}
	if err != nil {
		return nil, err
	}
	result := make([]AnchoredRange, len(values))
	for index := range result {
		result[index] = AnchoredRange{Start: transformed[index*2], End: transformed[index*2+1]}
		if result[index].Start.Offset > result[index].End.Offset {
			return nil, ErrInvertedRange
		}
	}
	return result, nil
}

// TransformAnnotations applies this map to opaque host annotations without
// interpreting or copying their values beyond normal Go assignment.
func TransformAnnotations[T any](m ChangeMap, values []Annotation[T]) ([]Annotation[T], error) {
	return transformAnnotations(nil, m, values)
}

// TransformAnnotationsContext transforms opaque annotations with bounded,
// cancellable ChangeMap work.
func TransformAnnotationsContext[T any](ctx context.Context, m ChangeMap, values []Annotation[T]) ([]Annotation[T], error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return transformAnnotations(ctx, m, values)
}

func transformAnnotations[T any](ctx context.Context, m ChangeMap, values []Annotation[T]) ([]Annotation[T], error) {
	if values == nil {
		return nil, nil
	}
	ranges := make([]AnchoredRange, len(values))
	for index := range values {
		ranges[index] = values[index].Range
	}
	var (
		transformed []AnchoredRange
		err         error
	)
	if ctx == nil {
		transformed, err = m.TransformRanges(ranges)
	} else {
		transformed, err = m.TransformRangesContext(ctx, ranges)
	}
	if err != nil {
		return nil, err
	}
	result := make([]Annotation[T], len(values))
	for index := range values {
		result[index] = Annotation[T]{Range: transformed[index], Value: values[index].Value}
	}
	return result, nil
}

func (m ChangeMap) Compose(next ChangeMap) (ChangeMap, error) {
	return m.ComposeAll(next)
}

// ComposeAll composes an adjacent chain with one validation pass and one edit
// allocation. It avoids the quadratic copying caused by repeatedly composing
// a long retained history.
func (m ChangeMap) ComposeAll(next ...ChangeMap) (ChangeMap, error) {
	total := len(m.edits)
	previousRevision, previousLength := m.afterRevision, m.afterLength
	for _, change := range next {
		if previousRevision != change.beforeRevision {
			return ChangeMap{}, ErrRevisionMismatch
		}
		if previousLength != change.beforeLength {
			return ChangeMap{}, ErrLengthMismatch
		}
		if len(change.edits) > MaximumEdits-total {
			return ChangeMap{}, ErrTooComplex
		}
		total += len(change.edits)
		previousRevision, previousLength = change.afterRevision, change.afterLength
	}
	edits := make([]Edit, 0, total)
	edits = append(edits, m.edits...)
	for _, change := range next {
		edits = append(edits, change.edits...)
	}
	return ChangeMap{
		beforeRevision: m.beforeRevision, afterRevision: previousRevision,
		beforeLength: m.beforeLength, afterLength: previousLength, edits: edits,
	}, nil
}

func (m ChangeMap) Invert() ChangeMap {
	edits := make([]Edit, len(m.edits))
	for index := range m.edits {
		original := m.edits[len(m.edits)-1-index]
		edits[index] = Edit{Start: original.Start, OldLength: original.NewLength, NewLength: original.OldLength}
	}
	return ChangeMap{
		beforeRevision: m.afterRevision, afterRevision: m.beforeRevision,
		beforeLength: m.afterLength, afterLength: m.beforeLength, edits: edits,
	}
}

// stableSuffix returns corresponding boundaries after which the final
// document is guaranteed to contain the untouched suffix of the original
// document. The guarantee follows from the sequential edit contract; no
// content interpretation is involved.
func (m ChangeMap) stableSuffix() (int64, int64, bool) {
	oldStart, currentStart := int64(0), int64(0)
	currentLength := m.beforeLength
	for _, edit := range m.edits {
		if edit.Start < 0 || edit.OldLength < 0 || edit.NewLength < 0 ||
			edit.Start > currentLength || edit.OldLength > currentLength-edit.Start {
			return 0, 0, false
		}
		end := edit.Start + edit.OldLength
		if end <= currentStart {
			delta := edit.NewLength - edit.OldLength
			currentStart += delta
		} else {
			if end > currentStart {
				oldStart += end - currentStart
			}
			currentStart = edit.Start + edit.NewLength
		}
		remaining := currentLength - edit.OldLength
		if edit.NewLength > math.MaxInt64-remaining {
			return 0, 0, false
		}
		currentLength = remaining + edit.NewLength
	}
	if currentLength != m.afterLength ||
		m.beforeLength-oldStart != m.afterLength-currentStart {
		return 0, 0, false
	}
	return oldStart, currentStart, true
}

func transformOffset(offset int64, affinity Affinity, edit Edit) int64 {
	end := edit.Start + edit.OldLength
	if offset < edit.Start {
		return offset
	}
	if edit.OldLength == 0 && offset == edit.Start {
		if affinity == AffinityAfter {
			return edit.Start + edit.NewLength
		}
		return edit.Start
	}
	if offset == edit.Start || offset < end {
		if affinity == AffinityAfter {
			return edit.Start + edit.NewLength
		}
		return edit.Start
	}
	return offset - edit.OldLength + edit.NewLength
}

func transformTooComplex(edits, items, factor int) bool {
	if edits == 0 || items == 0 {
		return false
	}
	if factor > MaximumTransformSteps/edits {
		return true
	}
	perItem := edits * factor
	return items > MaximumTransformSteps/perItem
}
