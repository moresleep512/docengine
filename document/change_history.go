package document

import (
	"errors"
	"fmt"

	"github.com/moresleep512/docengine/document/coordinate"
)

var (
	// ErrChangeHistoryExpired reports that at least one requested revision is
	// older than the earliest retained Session boundary.
	ErrChangeHistoryExpired = errors.New("document: change history expired")
	// ErrRevisionUnavailable reports a future revision or a revision inside an
	// atomic batch, neither of which is an observable Session state.
	ErrRevisionUnavailable = errors.New("document: revision is not an available session boundary")
)

const maximumAnchorTransformSteps = 16 << 20

// ChangeHistoryError reports the requested and retained revision windows while
// preserving ErrChangeHistoryExpired or ErrRevisionUnavailable via Unwrap.
type ChangeHistoryError struct {
	FromRevision    uint64
	ToRevision      uint64
	OldestRevision  uint64
	CurrentRevision uint64
	Err             error
}

func (e *ChangeHistoryError) Error() string {
	return fmt.Sprintf("document: revisions %d -> %d outside retained window %d -> %d: %v", e.FromRevision, e.ToRevision, e.OldestRevision, e.CurrentRevision, e.Err)
}

func (e *ChangeHistoryError) Unwrap() error { return e.Err }

// ChangeHistoryStats describes the currently retained, contiguous ChangeMap
// window. OldestRevision and CurrentRevision are both queryable boundaries.
type ChangeHistoryStats struct {
	OldestRevision  uint64
	CurrentRevision uint64
	Entries         int
	Limit           int
}

type changeHistory struct {
	entries         []coordinate.ChangeMap
	start           int
	count           int
	baseRevision    uint64
	baseLength      int64
	currentRevision uint64
	currentLength   int64
}

func newChangeHistory(limit int, revision uint64, length int64) *changeHistory {
	return &changeHistory{
		entries:      make([]coordinate.ChangeMap, limit),
		baseRevision: revision, baseLength: length,
		currentRevision: revision, currentLength: length,
	}
}

func (h *changeHistory) append(change coordinate.ChangeMap) {
	if change.Len() == 0 {
		if change.BeforeRevision() != h.currentRevision || change.BeforeLength() != h.currentLength ||
			change.AfterRevision() != h.currentRevision || change.AfterLength() != h.currentLength {
			h.reset(change.AfterRevision(), change.AfterLength())
		}
		return
	}
	if change.BeforeRevision() != h.currentRevision || change.BeforeLength() != h.currentLength ||
		change.AfterRevision() <= change.BeforeRevision() {
		h.reset(change.BeforeRevision(), change.BeforeLength())
	}
	if h.count == len(h.entries) {
		evicted := h.entries[h.start]
		h.baseRevision, h.baseLength = evicted.AfterRevision(), evicted.AfterLength()
		h.entries[h.start] = change
		h.start = (h.start + 1) % len(h.entries)
	} else {
		index := (h.start + h.count) % len(h.entries)
		h.entries[index] = change
		h.count++
	}
	h.currentRevision, h.currentLength = change.AfterRevision(), change.AfterLength()
}

func (h *changeHistory) reset(revision uint64, length int64) {
	clear(h.entries)
	h.start, h.count = 0, 0
	h.baseRevision, h.baseLength = revision, length
	h.currentRevision, h.currentLength = revision, length
}

func (h *changeHistory) between(fromRevision, toRevision uint64) (coordinate.ChangeMap, error) {
	if fromRevision < h.baseRevision || toRevision < h.baseRevision {
		return coordinate.ChangeMap{}, h.rangeError(fromRevision, toRevision, ErrChangeHistoryExpired)
	}
	if fromRevision > h.currentRevision || toRevision > h.currentRevision {
		return coordinate.ChangeMap{}, h.rangeError(fromRevision, toRevision, ErrRevisionUnavailable)
	}
	fromLength, fromOK := h.boundaryLength(fromRevision)
	_, toOK := h.boundaryLength(toRevision)
	if !fromOK || !toOK {
		return coordinate.ChangeMap{}, h.rangeError(fromRevision, toRevision, ErrRevisionUnavailable)
	}
	if fromRevision == toRevision {
		return coordinate.Identity(fromRevision, fromLength)
	}
	low, high, reverse := fromRevision, toRevision, false
	if low > high {
		low, high, reverse = high, low, true
	}
	lowLength, _ := h.boundaryLength(low)
	result, _ := coordinate.Identity(low, lowLength)
	cursor := low
	for index := 0; index < h.count && cursor < high; index++ {
		change := h.entry(index)
		if change.AfterRevision() <= cursor {
			continue
		}
		if change.BeforeRevision() != cursor || change.AfterRevision() > high {
			return coordinate.ChangeMap{}, h.rangeError(fromRevision, toRevision, ErrRevisionUnavailable)
		}
		var err error
		result, err = result.Compose(change)
		if err != nil {
			return coordinate.ChangeMap{}, h.rangeError(fromRevision, toRevision, ErrRevisionUnavailable)
		}
		cursor = change.AfterRevision()
	}
	if reverse {
		return result.Invert(), nil
	}
	return result, nil
}

func (h *changeHistory) rangeError(fromRevision, toRevision uint64, err error) error {
	return &ChangeHistoryError{
		FromRevision: fromRevision, ToRevision: toRevision,
		OldestRevision: h.baseRevision, CurrentRevision: h.currentRevision, Err: err,
	}
}

func (h *changeHistory) boundaryLength(revision uint64) (int64, bool) {
	if revision == h.baseRevision {
		return h.baseLength, true
	}
	for index := 0; index < h.count; index++ {
		change := h.entry(index)
		if revision == change.AfterRevision() {
			return change.AfterLength(), true
		}
	}
	return 0, false
}

func (h *changeHistory) entry(index int) coordinate.ChangeMap {
	return h.entries[(h.start+index)%len(h.entries)]
}

func (h *changeHistory) stats() ChangeHistoryStats {
	return ChangeHistoryStats{
		OldestRevision: h.baseRevision, CurrentRevision: h.currentRevision,
		Entries: h.count, Limit: len(h.entries),
	}
}

func (h *changeHistory) clone() *changeHistory {
	clone := newChangeHistory(len(h.entries), h.baseRevision, h.baseLength)
	clone.currentRevision, clone.currentLength = h.currentRevision, h.currentLength
	clone.count = h.count
	for index := 0; index < h.count; index++ {
		clone.entries[index] = h.entry(index)
	}
	return clone
}

// ChangeHistoryStats returns the retained revision window. It remains
// available after Close.
func (s *Session) ChangeHistoryStats() ChangeHistoryStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.changeHistory.stats()
}

// ChangesBetween composes the retained maps between two observable Session
// boundaries. Reverse queries return the inverse map. It remains available
// after Close because it does not access document Sources.
func (s *Session) ChangesBetween(fromRevision, toRevision uint64) (coordinate.ChangeMap, error) {
	s.mu.RLock()
	history := s.changeHistory.clone()
	s.mu.RUnlock()
	return history.between(fromRevision, toRevision)
}

// TransformAnchors applies the retained map between two revisions to an anchor
// batch. Input order is preserved and invalid input returns no partial result.
func (s *Session) TransformAnchors(fromRevision, toRevision uint64, anchors []coordinate.Anchor) ([]coordinate.Anchor, error) {
	s.mu.RLock()
	if len(anchors) > s.config.Limits.MaxAnchorBatch {
		s.mu.RUnlock()
		return nil, ErrLimitExceeded
	}
	history := s.changeHistory.clone()
	s.mu.RUnlock()
	changes, err := history.between(fromRevision, toRevision)
	if err != nil {
		return nil, err
	}
	if changes.Len() > 0 && len(anchors) > maximumAnchorTransformSteps/changes.Len() {
		return nil, ErrLimitExceeded
	}
	return changes.TransformAnchors(anchors)
}
