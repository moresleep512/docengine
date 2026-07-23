// Package store implements a bounded-memory source store for large documents.
package store

import (
	"errors"
	"io"
	"math"
	"sync"
)

var (
	ErrInvalidRange   = errors.New("store: invalid range")
	ErrInvalidPiece   = errors.New("store: invalid piece")
	ErrInvalidOptions = errors.New("store: invalid options")
	ErrLengthOverflow = errors.New("store: document length overflow")
	ErrUnknownSource  = errors.New("store: unknown source")
)

// DefaultAutoCompactPieces is the zero-value automatic-compaction trigger.
const DefaultAutoCompactPieces int64 = 4096

type SourceID uint8

const (
	SourceBase SourceID = iota + 1
	SourceJournal
)

type Piece struct {
	Source        SourceID
	Offset        int64
	Length        int64
	Newlines      int64
	NewlinesKnown bool
}

type node struct {
	piece         Piece
	priority      uint64
	left, right   *node
	bytes         int64
	newlines      int64
	newlinesKnown bool
	pieceCount    int64
}

type Snapshot struct {
	root    *node
	sources map[SourceID]io.ReaderAt
}

// Options controls structural maintenance. A zero value selects bounded
// defaults. DisableAutoCompact leaves compaction entirely under the caller's
// control.
type Options struct {
	AutoCompactPieces  int64
	DisableAutoCompact bool
}

type Tree struct {
	mu                    sync.RWMutex
	root                  *node
	sources               map[SourceID]io.ReaderAt
	rng                   uint64
	autoCompactPieces     int64
	nextAutoCompactPieces int64
	autoCompactions       uint64
}

// CompactResult reports structural Piece Tree compaction. Byte content and
// existing immutable Snapshots are unchanged.
type CompactResult struct {
	BeforePieces int64
	AfterPieces  int64
}

// Stats is an atomic view of Tree size, cached line metadata, and automatic
// compaction state.
type Stats struct {
	ByteLength            int64
	PieceCount            int64
	LineBreaks            int64
	LineBreaksKnown       bool
	AutoCompactPieces     int64
	NextAutoCompactPieces int64
	AutoCompactions       uint64
}

func New(base io.ReaderAt, length int64) (*Tree, error) {
	return NewWithOptions(base, length, Options{})
}

// NewWithOptions constructs a Tree whose initial Piece spans length bytes of
// base and whose structural maintenance follows options.
func NewWithOptions(base io.ReaderAt, length int64, options Options) (*Tree, error) {
	return NewWithBasePieceOptions(base, Piece{Source: SourceBase, Length: length}, options)
}

func NewWithBasePiece(base io.ReaderAt, piece Piece) (*Tree, error) {
	return NewWithBasePieceOptions(base, piece, Options{})
}

// NewWithBasePieceOptions is NewWithBasePiece with explicit structural
// maintenance options.
func NewWithBasePieceOptions(base io.ReaderAt, piece Piece, options Options) (*Tree, error) {
	autoCompactPieces, err := resolveOptions(options)
	if err != nil {
		return nil, err
	}
	t := &Tree{
		sources:               map[SourceID]io.ReaderAt{SourceBase: base},
		rng:                   0x9e3779b97f4a7c15,
		autoCompactPieces:     autoCompactPieces,
		nextAutoCompactPieces: autoCompactPieces,
	}
	piece.Source = SourceBase
	piece = normalizePiece(piece)
	if err := validatePiece(piece); err != nil {
		return nil, err
	}
	if piece.Length > 0 && base == nil {
		return nil, ErrUnknownSource
	}
	if piece.Length > 0 {
		t.root = t.makeNode(piece, nil, nil)
	}
	return t, nil
}

func resolveOptions(options Options) (int64, error) {
	if options.DisableAutoCompact {
		if options.AutoCompactPieces != 0 {
			return 0, ErrInvalidOptions
		}
		return 0, nil
	}
	if options.AutoCompactPieces == 0 {
		return DefaultAutoCompactPieces, nil
	}
	if options.AutoCompactPieces < 2 {
		return 0, ErrInvalidOptions
	}
	return options.AutoCompactPieces, nil
}

func (t *Tree) SetSource(id SourceID, source io.ReaderAt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if source == nil {
		delete(t.sources, id)
		return
	}
	t.sources[id] = source
}

func (t *Tree) Len() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return nodeBytes(t.root)
}

func (t *Tree) LineBreaks() (int64, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.root == nil {
		return 0, true
	}
	return t.root.newlines, t.root.newlinesKnown
}

func (t *Tree) PieceCount() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return nodePieceCount(t.root)
}

func (t *Tree) Stats() Stats {
	t.mu.RLock()
	defer t.mu.RUnlock()
	stats := Stats{
		ByteLength:            nodeBytes(t.root),
		PieceCount:            nodePieceCount(t.root),
		LineBreaks:            nodeNewlines(t.root),
		LineBreaksKnown:       nodeNewlinesKnown(t.root),
		AutoCompactPieces:     t.autoCompactPieces,
		NextAutoCompactPieces: t.nextAutoCompactPieces,
		AutoCompactions:       t.autoCompactions,
	}
	return stats
}

func (t *Tree) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return Snapshot{root: t.root, sources: cloneSources(t.sources)}
}

// Compact coalesces logically adjacent pieces that reference contiguous bytes
// from the same source. It never reads or rewrites source content and therefore
// remains bounded by the number of pieces rather than document size.
func (t *Tree) Compact() CompactResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	result := t.compactLocked()
	t.scheduleNextAutoCompactLocked(result.AfterPieces)
	return result
}

func (t *Tree) compactLocked() CompactResult {
	result := CompactResult{BeforePieces: nodePieceCount(t.root)}
	if result.BeforePieces < 2 {
		result.AfterPieces = result.BeforePieces
		return result
	}
	pieces := make([]Piece, 0, result.BeforePieces)
	_ = walk(t.root, func(piece Piece) error {
		last := len(pieces) - 1
		if last >= 0 && pieces[last].Source == piece.Source && pieces[last].Offset+pieces[last].Length == piece.Offset {
			pieces[last].Length += piece.Length
			if pieces[last].NewlinesKnown && piece.NewlinesKnown {
				pieces[last].Newlines += piece.Newlines
			} else {
				pieces[last].Newlines, pieces[last].NewlinesKnown = 0, false
			}
			return nil
		}
		pieces = append(pieces, piece)
		return nil
	})
	var root *node
	for _, piece := range pieces {
		root = merge(root, t.makeNode(piece, nil, nil))
	}
	t.root = root
	result.AfterPieces = int64(len(pieces))
	return result
}

func (t *Tree) Restore(snapshot Snapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.root = snapshot.root
	t.sources = cloneSources(snapshot.sources)
	// Restore remains O(1) and preserves the exact Snapshot root. If the
	// restored tree is already over threshold, the next real edit performs the
	// maintenance pass instead of inheriting a stale backoff schedule.
	t.nextAutoCompactPieces = t.autoCompactPieces
}

// ReplacePiece replaces a logical byte range and returns immutable snapshots
// before and after the change. A zero-length piece represents deletion.
func (t *Tree) ReplacePiece(start, deleteLength int64, replacement Piece) (Snapshot, Snapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	length := nodeBytes(t.root)
	if start < 0 || deleteLength < 0 || start > length || deleteLength > length-start {
		return Snapshot{}, Snapshot{}, ErrInvalidRange
	}
	replacement = normalizePiece(replacement)
	if err := validatePiece(replacement); err != nil {
		return Snapshot{}, Snapshot{}, err
	}
	if replacement.Length > 0 && t.sources[replacement.Source] == nil {
		return Snapshot{}, Snapshot{}, ErrUnknownSource
	}
	remaining := length - deleteLength
	if replacement.Length > math.MaxInt64-remaining {
		return Snapshot{}, Snapshot{}, ErrLengthOverflow
	}
	before := Snapshot{root: t.root, sources: cloneSources(t.sources)}
	if deleteLength == 0 && replacement.Length == 0 {
		return before, before, nil
	}
	left, tail := t.split(t.root, start)
	_, right := t.split(tail, deleteLength)
	var middle *node
	if replacement.Length > 0 {
		middle = t.makeNode(replacement, nil, nil)
	}
	t.root = merge(merge(left, middle), right)
	t.maybeAutoCompactLocked()
	after := Snapshot{root: t.root, sources: cloneSources(t.sources)}
	return before, after, nil
}

func (t *Tree) maybeAutoCompactLocked() {
	if t.autoCompactPieces == 0 {
		return
	}
	pieces := nodePieceCount(t.root)
	if pieces < t.autoCompactPieces {
		t.nextAutoCompactPieces = t.autoCompactPieces
		return
	}
	if pieces < t.nextAutoCompactPieces {
		return
	}
	result := t.compactLocked()
	t.autoCompactions++
	t.scheduleNextAutoCompactLocked(result.AfterPieces)
}

func (t *Tree) scheduleNextAutoCompactLocked(pieces int64) {
	if t.autoCompactPieces == 0 {
		t.nextAutoCompactPieces = 0
		return
	}
	if pieces < t.autoCompactPieces {
		t.nextAutoCompactPieces = t.autoCompactPieces
		return
	}
	if pieces > math.MaxInt64-t.autoCompactPieces {
		t.nextAutoCompactPieces = math.MaxInt64
		return
	}
	t.nextAutoCompactPieces = pieces + t.autoCompactPieces
}

func validatePiece(piece Piece) error {
	if piece.Length == 0 {
		return nil
	}
	if piece.Length < 0 || piece.Offset < 0 || piece.Offset > math.MaxInt64-piece.Length {
		return ErrInvalidPiece
	}
	if piece.NewlinesKnown && (piece.Newlines < 0 || piece.Newlines > piece.Length) {
		return ErrInvalidPiece
	}
	return nil
}

func normalizePiece(piece Piece) Piece {
	if !piece.NewlinesKnown {
		piece.Newlines = 0
	}
	return piece
}

func (t *Tree) ReadAt(p []byte, off int64) (int, error) {
	return t.Snapshot().ReadAt(p, off)
}

func (s Snapshot) Len() int64 { return nodeBytes(s.root) }

func (s Snapshot) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, ErrInvalidRange
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= s.Len() {
		return 0, io.EOF
	}
	wanted := len(p)
	if remain := s.Len() - off; int64(wanted) > remain {
		wanted = int(remain)
	}
	n, err := readNode(s.root, s.sources, off, p[:wanted])
	if err != nil {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (s Snapshot) WriteTo(w io.Writer) (int64, error) {
	var total int64
	err := walk(s.root, func(piece Piece) error {
		source := s.sources[piece.Source]
		if source == nil {
			return ErrUnknownSource
		}
		n, err := io.CopyN(w, io.NewSectionReader(source, piece.Offset, piece.Length), piece.Length)
		total += n
		return err
	})
	return total, err
}

func (t *Tree) split(current *node, pos int64) (*node, *node) {
	if current == nil {
		return nil, nil
	}
	leftBytes := nodeBytes(current.left)
	if pos < leftBytes {
		left, rightOfLeft := t.split(current.left, pos)
		return left, cloneNode(current, rightOfLeft, current.right)
	}
	pieceEnd := leftBytes + current.piece.Length
	if pos > pieceEnd {
		leftOfRight, right := t.split(current.right, pos-pieceEnd)
		return cloneNode(current, current.left, leftOfRight), right
	}
	if pos == leftBytes {
		return current.left, cloneNode(current, nil, current.right)
	}
	if pos == pieceEnd {
		return cloneNode(current, current.left, nil), current.right
	}

	leftLength := pos - leftBytes
	rightLength := current.piece.Length - leftLength
	leftPiece := current.piece
	leftPiece.Length = leftLength
	leftPiece.NewlinesKnown = false
	leftPiece.Newlines = 0
	rightPiece := current.piece
	rightPiece.Offset += leftLength
	rightPiece.Length = rightLength
	rightPiece.NewlinesKnown = false
	rightPiece.Newlines = 0
	// Both fragments replace current in the treap and therefore inherit its
	// priority. Generating fresh priorities here can make a fragment outrank an
	// ancestor while split unwinds, violating the treap heap invariant.
	leftNode := recalc(&node{piece: leftPiece, priority: current.priority})
	rightNode := recalc(&node{piece: rightPiece, priority: current.priority})
	return merge(current.left, leftNode), merge(rightNode, current.right)
}

func merge(left, right *node) *node {
	if left == nil {
		return right
	}
	if right == nil {
		return left
	}
	if left.priority >= right.priority {
		return cloneNode(left, left.left, merge(left.right, right))
	}
	return cloneNode(right, merge(left, right.left), right.right)
}

func (t *Tree) makeNode(piece Piece, left, right *node) *node {
	t.rng ^= t.rng >> 12
	t.rng ^= t.rng << 25
	t.rng ^= t.rng >> 27
	priority := t.rng * 0x2545f4914f6cdd1d
	return recalc(&node{piece: piece, priority: priority, left: left, right: right})
}

func cloneNode(n, left, right *node) *node {
	if n == nil {
		return nil
	}
	copy := *n
	copy.left, copy.right = left, right
	return recalc(&copy)
}

func recalc(n *node) *node {
	if n == nil {
		return nil
	}
	n.bytes = nodeBytes(n.left) + n.piece.Length + nodeBytes(n.right)
	n.pieceCount = nodePieceCount(n.left) + 1 + nodePieceCount(n.right)
	n.newlines = nodeNewlines(n.left) + n.piece.Newlines + nodeNewlines(n.right)
	n.newlinesKnown = nodeNewlinesKnown(n.left) && n.piece.NewlinesKnown && nodeNewlinesKnown(n.right)
	return n
}

func nodeBytes(n *node) int64 {
	if n == nil {
		return 0
	}
	return n.bytes
}

func nodePieceCount(n *node) int64 {
	if n == nil {
		return 0
	}
	return n.pieceCount
}

func nodeNewlines(n *node) int64 {
	if n == nil {
		return 0
	}
	return n.newlines
}

func nodeNewlinesKnown(n *node) bool { return n == nil || n.newlinesKnown }

func cloneSources(in map[SourceID]io.ReaderAt) map[SourceID]io.ReaderAt {
	out := make(map[SourceID]io.ReaderAt, len(in))
	for id, source := range in {
		out[id] = source
	}
	return out
}

func readNode(n *node, sources map[SourceID]io.ReaderAt, off int64, dst []byte) (int, error) {
	if n == nil || len(dst) == 0 {
		return 0, nil
	}
	written := 0
	leftBytes := nodeBytes(n.left)
	if off < leftBytes {
		count, err := readNode(n.left, sources, off, dst)
		written += count
		if err != nil && !errors.Is(err, io.EOF) {
			return written, err
		}
		off = 0
	} else {
		off -= leftBytes
	}
	if written == len(dst) {
		return written, nil
	}
	if off < n.piece.Length {
		available := n.piece.Length - off
		want := int64(len(dst) - written)
		if want > available {
			want = available
		}
		source := sources[n.piece.Source]
		if source == nil {
			return written, ErrUnknownSource
		}
		count, err := source.ReadAt(dst[written:written+int(want)], n.piece.Offset+off)
		written += count
		if err != nil && !(errors.Is(err, io.EOF) && int64(count) == want) {
			return written, err
		}
		off = 0
	} else {
		off -= n.piece.Length
	}
	if written == len(dst) {
		return written, nil
	}
	count, err := readNode(n.right, sources, off, dst[written:])
	written += count
	return written, err
}

func walk(n *node, visit func(Piece) error) error {
	if n == nil {
		return nil
	}
	if err := walk(n.left, visit); err != nil {
		return err
	}
	if err := visit(n.piece); err != nil {
		return err
	}
	return walk(n.right, visit)
}
