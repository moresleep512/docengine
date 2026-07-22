package coordinate

import (
	"errors"
	"math"
)

var (
	ErrInvalidChange    = errors.New("coordinate: invalid change")
	ErrInvalidAffinity  = errors.New("coordinate: invalid anchor affinity")
	ErrInvalidRange     = errors.New("coordinate: invalid range")
	ErrInvertedRange    = errors.New("coordinate: transformed range is inverted")
	ErrRevisionMismatch = errors.New("coordinate: revision mismatch")
	ErrLengthMismatch   = errors.New("coordinate: document length mismatch")
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
		for index := range result {
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

func (m ChangeMap) Compose(next ChangeMap) (ChangeMap, error) {
	if m.afterRevision != next.beforeRevision {
		return ChangeMap{}, ErrRevisionMismatch
	}
	if m.afterLength != next.beforeLength {
		return ChangeMap{}, ErrLengthMismatch
	}
	edits := make([]Edit, 0, len(m.edits)+len(next.edits))
	edits = append(edits, m.edits...)
	edits = append(edits, next.edits...)
	return ChangeMap{
		beforeRevision: m.beforeRevision, afterRevision: next.afterRevision,
		beforeLength: m.beforeLength, afterLength: next.afterLength, edits: edits,
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
