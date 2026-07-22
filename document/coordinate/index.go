// Package coordinate provides format-neutral UTF-8 coordinate indexes,
// anchors, and cross-revision change maps.
package coordinate

import (
	"context"
	"errors"
	"io"
	"math"
	"sort"
	"sync"
	"unicode/utf8"
)

const (
	DefaultCheckpointBytes int64 = 64 << 10
	MaximumCheckpointBytes int64 = 64 << 20
	buildReadBufferSize          = 64 << 10
)

var (
	ErrInvalidSource      = errors.New("coordinate: invalid source")
	ErrInvalidOptions     = errors.New("coordinate: invalid options")
	ErrInvalidUTF8        = errors.New("coordinate: source is not UTF-8")
	ErrInvalidOffset      = errors.New("coordinate: invalid byte or rune offset")
	ErrNotRuneBoundary    = errors.New("coordinate: byte offset is not a UTF-8 rune boundary")
	ErrInvalidPosition    = errors.New("coordinate: invalid line or column")
	ErrInvalidContext     = errors.New("coordinate: nil context")
	ErrClosed             = errors.New("coordinate: index closed")
	ErrSourceInconsistent = errors.New("coordinate: source length or content changed")
	ErrInvalidIndex       = errors.New("coordinate: invalid previous index")
	ErrLineageMismatch    = errors.New("coordinate: index lineage mismatch")
)

// Source is an immutable byte source. It must remain readable for the lifetime
// of an Index built with Build.
type Source interface {
	io.ReaderAt
	Len() int64
}

// OwnedSource transfers its lifetime to BuildOwned. BuildOwned closes the
// source if construction fails; Index.Close releases it after success.
type OwnedSource interface {
	Source
	io.Closer
}

// Lineage is an opaque identity shared by indexes derived from the same
// trusted Source history. Pointer identity is intentional.
type Lineage struct{ marker byte }

// NewLineage creates a unique index lineage token.
func NewLineage() *Lineage { return &Lineage{} }

type Options struct {
	CheckpointBytes int64
	Lineage         *Lineage
}

type Stats struct {
	Revision          uint64
	ByteLength        int64
	RuneCount         int64
	LineCount         int64
	CheckpointCount   int
	CheckpointBytes   int64
	ReusedCheckpoints int
	ScannedBytes      int64
}

// Position uses zero-based coordinates. Column counts Unicode code points
// since the most recent LF; CR is ordinary content. ByteOffset and RuneOffset
// may equal their respective document totals to represent EOF.
type Position struct {
	ByteOffset int64
	RuneOffset int64
	Line       int64
	Column     int64
}

type checkpoint struct {
	byteOffset int64
	runeOffset int64
	line       int64
	column     int64
}

type Index struct {
	mu              sync.RWMutex
	source          Source
	release         func() error
	closed          bool
	revision        uint64
	byteLength      int64
	runeCount       int64
	lineCount       int64
	checkpointBytes int64
	checkpoints     []checkpoint
	reused          int
	scannedBytes    int64
	lineage         *Lineage
}

func Build(ctx context.Context, source Source, revision uint64, options Options) (*Index, error) {
	return build(ctx, source, revision, options, nil)
}

func BuildOwned(ctx context.Context, source OwnedSource, revision uint64, options Options) (*Index, error) {
	if source == nil {
		return nil, ErrInvalidSource
	}
	index, err := build(ctx, source, revision, options, source.Close)
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return index, nil
}

// Rebuild creates an index for the exact document state produced by changes
// from previous. It reuses only the checkpoint prefix that precedes every edit
// and scans the remaining new source. The caller must keep source immutable and
// readable for the returned Index lifetime.
func Rebuild(ctx context.Context, source Source, previous *Index, changes ChangeMap) (*Index, error) {
	return rebuild(ctx, source, previous, changes, nil)
}

// RebuildOwned is Rebuild with ownership transfer for the new source. The
// previous Index retains its own independent source lifetime.
func RebuildOwned(ctx context.Context, source OwnedSource, previous *Index, changes ChangeMap) (*Index, error) {
	if source == nil {
		return nil, ErrInvalidSource
	}
	index, err := rebuild(ctx, source, previous, changes, source.Close)
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return index, nil
}

func build(ctx context.Context, source Source, revision uint64, options Options, release func() error) (*Index, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if source == nil {
		return nil, ErrInvalidSource
	}
	length := source.Len()
	if length < 0 {
		return nil, ErrInvalidSource
	}
	checkpointBytes := options.CheckpointBytes
	if checkpointBytes == 0 {
		checkpointBytes = DefaultCheckpointBytes
	}
	if checkpointBytes < 0 || checkpointBytes > MaximumCheckpointBytes {
		return nil, ErrInvalidOptions
	}
	index := &Index{
		source:          source,
		release:         release,
		revision:        revision,
		byteLength:      length,
		checkpointBytes: checkpointBytes,
		checkpoints:     []checkpoint{{}},
		lineage:         options.Lineage,
	}
	if err := index.scan(ctx); err != nil {
		return nil, err
	}
	return index, nil
}

func rebuild(ctx context.Context, source Source, previous *Index, changes ChangeMap, release func() error) (*Index, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if source == nil {
		return nil, ErrInvalidSource
	}
	if previous == nil {
		return nil, ErrInvalidIndex
	}
	length := source.Len()
	if length < 0 {
		return nil, ErrInvalidSource
	}
	checkpointBytes, scanStart, checkpoints, err := incrementalSeed(previous, changes, length)
	if err != nil {
		return nil, err
	}
	index := &Index{
		source: source, release: release, revision: changes.afterRevision,
		byteLength: length, checkpointBytes: checkpointBytes, checkpoints: checkpoints,
		reused: len(checkpoints), lineage: previousLineage(previous),
	}
	if err := index.scanFrom(ctx, checkpoints[len(checkpoints)-1], scanStart); err != nil {
		return nil, err
	}
	return index, nil
}

func previousLineage(previous *Index) *Lineage {
	previous.mu.RLock()
	defer previous.mu.RUnlock()
	return previous.lineage
}

func incrementalSeed(previous *Index, changes ChangeMap, afterLength int64) (int64, int64, []checkpoint, error) {
	previous.mu.RLock()
	defer previous.mu.RUnlock()
	if previous.closed {
		return 0, 0, nil, ErrClosed
	}
	if previous.revision != changes.beforeRevision {
		return 0, 0, nil, ErrRevisionMismatch
	}
	if previous.byteLength != changes.beforeLength || afterLength != changes.afterLength {
		return 0, 0, nil, ErrLengthMismatch
	}
	if previous.checkpointBytes <= 0 || previous.checkpointBytes > MaximumCheckpointBytes || len(previous.checkpoints) == 0 || previous.checkpoints[0] != (checkpoint{}) {
		return 0, 0, nil, ErrInvalidIndex
	}
	stablePrefix := changes.beforeLength
	for _, edit := range changes.edits {
		if edit.Start < stablePrefix {
			stablePrefix = edit.Start
		}
	}
	last := sort.Search(len(previous.checkpoints), func(index int) bool {
		return previous.checkpoints[index].byteOffset > stablePrefix
	}) - 1
	if last < 0 {
		return 0, 0, nil, ErrInvalidIndex
	}
	checkpoints := append([]checkpoint(nil), previous.checkpoints[:last+1]...)
	scanStart := checkpoints[len(checkpoints)-1].byteOffset
	if scanStart < 0 || scanStart > stablePrefix || scanStart > afterLength {
		return 0, 0, nil, ErrInvalidIndex
	}
	return previous.checkpointBytes, scanStart, checkpoints, nil
}

func (i *Index) scan(ctx context.Context) error {
	return i.scanFrom(ctx, checkpoint{}, 0)
}

func (i *Index) scanFrom(ctx context.Context, state checkpoint, scanStart int64) error {
	buffer := make([]byte, buildReadBufferSize)
	pending := make([]byte, 0, utf8.UTFMax-1)
	readOffset := scanStart
	nextCheckpoint := nextCheckpointAfter(scanStart, i.checkpointBytes)
	for readOffset < i.byteLength {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := min(int64(len(buffer)), i.byteLength-readOffset)
		n, readErr := i.source.ReadAt(buffer[:int(want)], readOffset)
		if n < 0 || int64(n) > want {
			return ErrSourceInconsistent
		}
		data := make([]byte, 0, len(pending)+n)
		data = append(data, pending...)
		data = append(data, buffer[:n]...)
		base := readOffset - int64(len(pending))
		pending = pending[:0]
		cursor := 0
		for cursor < len(data) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !utf8.FullRune(data[cursor:]) {
				pending = append(pending, data[cursor:]...)
				break
			}
			r, size := utf8.DecodeRune(data[cursor:])
			if r == utf8.RuneError && size == 1 {
				return ErrInvalidUTF8
			}
			absolute := base + int64(cursor)
			if absolute >= nextCheckpoint {
				state.byteOffset = absolute
				i.checkpoints = append(i.checkpoints, state)
				nextCheckpoint = nextCheckpointAfter(absolute, i.checkpointBytes)
			}
			state.runeOffset++
			if r == '\n' {
				state.line++
				state.column = 0
			} else {
				state.column++
			}
			cursor += size
		}
		readOffset += int64(n)
		if readErr != nil && !(errors.Is(readErr, io.EOF) && readOffset == i.byteLength) {
			return readErr
		}
		if n == 0 && readOffset < i.byteLength {
			return io.ErrUnexpectedEOF
		}
	}
	if len(pending) != 0 {
		return ErrInvalidUTF8
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state.byteOffset = i.byteLength
	if last := i.checkpoints[len(i.checkpoints)-1]; last.byteOffset != state.byteOffset {
		i.checkpoints = append(i.checkpoints, state)
	} else {
		i.checkpoints[len(i.checkpoints)-1] = state
	}
	i.runeCount = state.runeOffset
	i.lineCount = state.line + 1
	i.scannedBytes = i.byteLength - scanStart
	return nil
}

func (i *Index) Stats() Stats {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return Stats{
		Revision: i.revision, ByteLength: i.byteLength, RuneCount: i.runeCount,
		LineCount: i.lineCount, CheckpointCount: len(i.checkpoints), CheckpointBytes: i.checkpointBytes,
		ReusedCheckpoints: i.reused, ScannedBytes: i.scannedBytes,
	}
}

// BelongsTo reports whether this Index was built or derived with lineage. It
// remains available after Close and does not expose the stored token.
func (i *Index) BelongsTo(lineage *Lineage) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return lineage != nil && i.lineage == lineage
}

func (i *Index) ByteToPosition(ctx context.Context, offset int64) (Position, error) {
	if ctx == nil {
		return Position{}, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return Position{}, err
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.closed {
		return Position{}, ErrClosed
	}
	if offset < 0 || offset > i.byteLength {
		return Position{}, ErrInvalidOffset
	}
	cp := i.checkpoints[sort.Search(len(i.checkpoints), func(index int) bool {
		return i.checkpoints[index].byteOffset > offset
	})-1]
	state := cp
	if state.byteOffset == offset {
		return positionFor(state), nil
	}
	data, err := i.readWindow(ctx, cp.byteOffset)
	if err != nil {
		return Position{}, err
	}
	for cursor := 0; ; {
		if err := ctx.Err(); err != nil {
			return Position{}, err
		}
		if state.byteOffset == offset {
			return positionFor(state), nil
		}
		if cursor >= len(data) {
			return Position{}, ErrSourceInconsistent
		}
		absolute := cp.byteOffset + int64(cursor)
		r, size, decodeErr := decodeRune(data[cursor:])
		if decodeErr != nil {
			return Position{}, decodeErr
		}
		if offset < absolute+int64(size) {
			return Position{}, ErrNotRuneBoundary
		}
		advance(&state, r, size)
		cursor += size
	}
}

func (i *Index) RuneToByte(ctx context.Context, offset int64) (int64, error) {
	if ctx == nil {
		return 0, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.closed {
		return 0, ErrClosed
	}
	if offset < 0 || offset > i.runeCount {
		return 0, ErrInvalidOffset
	}
	cp := i.checkpoints[sort.Search(len(i.checkpoints), func(index int) bool {
		return i.checkpoints[index].runeOffset > offset
	})-1]
	if cp.runeOffset == offset {
		return cp.byteOffset, nil
	}
	data, err := i.readWindow(ctx, cp.byteOffset)
	if err != nil {
		return 0, err
	}
	state := cp
	for cursor := 0; ; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if state.runeOffset == offset {
			return state.byteOffset, nil
		}
		if cursor >= len(data) {
			return 0, ErrSourceInconsistent
		}
		r, size, decodeErr := decodeRune(data[cursor:])
		if decodeErr != nil {
			return 0, decodeErr
		}
		advance(&state, r, size)
		cursor += size
	}
}

func (i *Index) PositionToByte(ctx context.Context, line, column int64) (int64, error) {
	if ctx == nil {
		return 0, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.closed {
		return 0, ErrClosed
	}
	if line < 0 || line >= i.lineCount || column < 0 {
		return 0, ErrInvalidPosition
	}
	cp := i.checkpoints[sort.Search(len(i.checkpoints), func(index int) bool {
		candidate := i.checkpoints[index]
		return candidate.line > line || candidate.line == line && candidate.column > column
	})-1]
	if cp.line == line && cp.column == column {
		return cp.byteOffset, nil
	}
	data, err := i.readWindow(ctx, cp.byteOffset)
	if err != nil {
		return 0, err
	}
	state := cp
	for cursor := 0; ; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if state.line == line && state.column == column {
			return state.byteOffset, nil
		}
		if state.line > line || state.line == line && state.column > column {
			return 0, ErrInvalidPosition
		}
		if cursor >= len(data) {
			return 0, ErrInvalidPosition
		}
		r, size, decodeErr := decodeRune(data[cursor:])
		if decodeErr != nil {
			return 0, decodeErr
		}
		advance(&state, r, size)
		cursor += size
	}
}

func (i *Index) LineStart(ctx context.Context, line int64) (int64, error) {
	return i.PositionToByte(ctx, line, 0)
}

func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return nil
	}
	i.closed = true
	if i.release == nil {
		return nil
	}
	release := i.release
	i.release = nil
	return release()
}

func (i *Index) readWindow(ctx context.Context, start int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	window := i.checkpointBytes + utf8.UTFMax
	end := i.byteLength
	if start <= math.MaxInt64-window && start+window < end {
		end = start + window
	}
	length := end - start
	data := make([]byte, int(length))
	for read := 0; read < len(data); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := i.source.ReadAt(data[read:], start+int64(read))
		if n < 0 || n > len(data)-read {
			return nil, ErrSourceInconsistent
		}
		read += n
		if err != nil && !(errors.Is(err, io.EOF) && read == len(data)) {
			return nil, err
		}
		if n == 0 && read < len(data) {
			return nil, io.ErrUnexpectedEOF
		}
	}
	return data, nil
}

func decodeRune(data []byte) (rune, int, error) {
	if len(data) == 0 || !utf8.FullRune(data) {
		return 0, 0, ErrSourceInconsistent
	}
	r, size := utf8.DecodeRune(data)
	if r == utf8.RuneError && size == 1 {
		return 0, 0, ErrInvalidUTF8
	}
	return r, size, nil
}

func advance(state *checkpoint, r rune, size int) {
	state.byteOffset += int64(size)
	state.runeOffset++
	if r == '\n' {
		state.line++
		state.column = 0
	} else {
		state.column++
	}
}

func positionFor(state checkpoint) Position {
	return Position{ByteOffset: state.byteOffset, RuneOffset: state.runeOffset, Line: state.line, Column: state.column}
}

func nextCheckpointAfter(offset, block int64) int64 {
	if offset > math.MaxInt64-block {
		return math.MaxInt64
	}
	return (offset/block + 1) * block
}
