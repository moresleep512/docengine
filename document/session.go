// Package document coordinates the piece tree, recovery WAL, transactional
// history, leased source generations, and atomic persistence.
package document

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/moresleep512/docengine/document/save"
	"github.com/moresleep512/docengine/document/store"
	"github.com/moresleep512/docengine/recovery"
)

var (
	ErrRevisionConflict = errors.New("document: revision conflict")
	ErrInvalidUTF8      = errors.New("document: file is not UTF-8")
	ErrClosed           = errors.New("document: session closed")
	ErrNothingToUndo    = errors.New("document: nothing to undo")
	ErrNothingToRedo    = errors.New("document: nothing to redo")
	ErrExternalChange   = errors.New("document: file changed on disk")
	ErrRevisionOverflow = errors.New("document: revision overflow")
)

type EOLStyle string

const (
	EOLLF    EOLStyle = "lf"
	EOLCRLF  EOLStyle = "crlf"
	EOLMixed EOLStyle = "mixed"
)

type OpenOptions struct {
	RecoveryDir string
	SessionDir  string
}

type Metadata struct {
	Path              string
	Name              string
	ByteLength        int64
	Revision          uint64
	CommittedRevision uint64
	Dirty             bool
	Recovered         bool
	HasBOM            bool
	EOL               EOLStyle
}

type ReplaceOperation struct {
	Start        int64
	DeleteLength int64
	Insert       string
}

type ApplyResult struct {
	Revision   uint64
	ByteLength int64
	Dirty      bool
}

type historyOperation struct {
	start, deleteLength int64
	insert              textRef
}

type historyEntry struct {
	forward []historyOperation
	inverse []historyOperation
}

type pendingOperation struct {
	revision                   uint64
	group                      uint64
	start, deleteLength        int64
	insertOffset, insertLength int64
}

type stagedOperation struct {
	operation ReplaceOperation
	inserted  []byte
	deleted   []byte
}

const stagingSourceID store.SourceID = 255

type fileIdentity struct {
	size    int64
	modTime int64
}

type Session struct {
	mu                sync.RWMutex
	saveMu            sync.Mutex
	path              string
	mode              os.FileMode
	base              *os.File
	journal           *recovery.Journal
	generation        *sourceGeneration
	tree              *store.Tree
	revision          uint64
	committedRevision uint64
	undo              []historyEntry
	redo              []historyEntry
	undoStore         *undoStore
	undoEpoch         uint64
	pending           []pendingOperation
	dirty             bool
	recovered         bool
	hasBOM            bool
	eol               EOLStyle
	recoveryDir       string
	sessionDir        string
	fingerprint       recovery.Fingerprint
	diskIdentity      fileIdentity
	closed            bool
	commitHook        func(string)
	stopSync          chan struct{}
	syncDone          chan struct{}
}

func Open(path string, options OpenOptions) (*Session, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	base, err := openBase(absolute)
	if err != nil {
		return nil, err
	}
	info, err := base.Stat()
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = base.Close()
		return nil, errors.New("document: path is not a regular file")
	}
	sampleLength := min(info.Size(), 64<<10)
	sample := make([]byte, sampleLength)
	_, _ = base.ReadAt(sample, 0)
	hasBOM := len(sample) >= 3 && bytes.Equal(sample[:3], []byte{0xef, 0xbb, 0xbf})
	textSample, baseOffset := sample, int64(0)
	if hasBOM {
		textSample, baseOffset = sample[3:], 3
	}
	if !utf8.Valid(textSample) {
		_ = base.Close()
		return nil, ErrInvalidUTF8
	}
	if options.RecoveryDir == "" {
		options.RecoveryDir = filepath.Join(os.TempDir(), "docengine", "recovery")
	}
	if options.SessionDir == "" {
		options.SessionDir = filepath.Join(os.TempDir(), "docengine", "sessions", randomSuffix())
	}
	undo, err := openUndoStore(options.SessionDir)
	if err != nil {
		_ = base.Close()
		return nil, err
	}
	fingerprint := recovery.FingerprintFor(absolute, info)
	journal, replay, err := openMatchingJournal(options.RecoveryDir, fingerprint)
	if err != nil {
		_ = undo.close()
		_ = base.Close()
		return nil, err
	}
	tree, err := store.NewWithBasePiece(base, store.Piece{Source: store.SourceBase, Offset: baseOffset, Length: info.Size() - baseOffset})
	if err != nil {
		if journal != nil {
			_ = journal.Close()
		}
		_ = undo.close()
		_ = base.Close()
		return nil, err
	}
	if journal != nil {
		tree.SetSource(store.SourceJournal, journal)
	}
	generation := newSourceGeneration(base, journal)
	session := &Session{
		path: absolute, mode: info.Mode(), base: base, journal: journal, generation: generation, tree: tree,
		undoStore: undo, hasBOM: hasBOM, eol: detectEOL(textSample), recoveryDir: options.RecoveryDir,
		sessionDir: options.SessionDir, fingerprint: fingerprint, diskIdentity: identityFor(info),
		stopSync: make(chan struct{}), syncDone: make(chan struct{}),
	}
	if journal != nil {
		if err := session.replay(replay); err != nil {
			generation.retireAndWait(false)
			_ = undo.close()
			return nil, err
		}
		if replay.Truncated {
			if err := journal.RepairTail(replay.ValidBytes); err != nil {
				generation.retireAndWait(false)
				_ = undo.close()
				return nil, err
			}
		}
	}
	go session.syncLoop()
	return session, nil
}

func openMatchingJournal(dir string, fingerprint recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, recovery.ReplayResult{}, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, journalPrefix(fingerprint)+"*.docengine-journal"))
	if err != nil {
		return nil, recovery.ReplayResult{}, err
	}
	sort.Slice(paths, func(i, j int) bool {
		left, _ := os.Stat(paths[i])
		right, _ := os.Stat(paths[j])
		return left != nil && right != nil && left.ModTime().After(right.ModTime())
	})
	for _, path := range paths {
		stored, probeErr := recovery.ReadFingerprint(path)
		if probeErr != nil || stored != fingerprint {
			_ = os.Rename(path, fmt.Sprintf("%s.stale-%d", path, time.Now().UnixNano()))
			continue
		}
		journal, replay, openErr := recovery.Open(path, fingerprint)
		return journal, replay, openErr
	}
	return nil, recovery.ReplayResult{}, nil
}

func (s *Session) replay(result recovery.ReplayResult) error {
	var activeGroup uint64
	var active historyEntry
	roots := map[uint64]store.Snapshot{0: s.tree.Snapshot()}
	for position, frame := range result.Frames {
		if position == 0 && frame.Revision > 0 {
			s.committedRevision = frame.Revision - 1
		}
		if frame.Revision <= s.revision {
			return errors.New("document: non-monotonic journal revision")
		}
		switch frame.Kind {
		case recovery.FrameReplace:
			deleted, err := readTreeRange(s.tree, frame.Start, frame.DeleteLength)
			if err != nil {
				return err
			}
			inserted := make([]byte, frame.InsertLength)
			if frame.InsertLength > 0 {
				n, readErr := s.journal.ReadAt(inserted, frame.PayloadOffset)
				if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(inserted)) {
					return readErr
				}
				if n != len(inserted) {
					return io.ErrUnexpectedEOF
				}
			}
			forwardRef, err := s.historyText(inserted)
			if err != nil {
				return err
			}
			inverseRef, err := s.historyText(deleted)
			if err != nil {
				return err
			}
			piece := store.Piece{}
			if frame.InsertLength > 0 {
				piece = store.Piece{Source: store.SourceJournal, Offset: frame.PayloadOffset, Length: frame.InsertLength, Newlines: int64(bytes.Count(inserted, []byte{'\n'})), NewlinesKnown: true}
			}
			_, after, err := s.tree.ReplacePiece(frame.Start, frame.DeleteLength, piece)
			if err != nil {
				return fmt.Errorf("replay revision %d: %w", frame.Revision, err)
			}
			roots[frame.Revision] = after
			if activeGroup != frame.TargetRevision && len(active.forward) > 0 {
				s.undo = append(s.undo, active)
				active = historyEntry{}
			}
			activeGroup = frame.TargetRevision
			active.forward = append(active.forward, historyOperation{start: frame.Start, deleteLength: frame.DeleteLength, insert: forwardRef})
			active.inverse = append([]historyOperation{{start: frame.Start, deleteLength: frame.InsertLength, insert: inverseRef}}, active.inverse...)
			s.pending = append(s.pending, pendingOperation{revision: frame.Revision, group: frame.TargetRevision, start: frame.Start, deleteLength: frame.DeleteLength, insertOffset: frame.PayloadOffset, insertLength: frame.InsertLength})
		case recovery.FrameRoot: // legacy v1 recovery compatibility
			target, ok := roots[frame.TargetRevision]
			if !ok {
				return fmt.Errorf("document: missing target revision %d", frame.TargetRevision)
			}
			s.tree.Restore(target)
			active, s.undo, s.redo = historyEntry{}, nil, nil
		}
		s.revision = frame.Revision
	}
	if len(active.forward) > 0 {
		s.undo = append(s.undo, active)
	}
	if len(result.Frames) > 0 {
		s.dirty, s.recovered = true, true
	}
	return nil
}

func (s *Session) Metadata() Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadataLocked()
}

func (s *Session) metadataLocked() Metadata {
	return Metadata{Path: s.path, Name: filepath.Base(s.path), ByteLength: s.tree.Len(), Revision: s.revision, CommittedRevision: s.committedRevision, Dirty: s.dirty, Recovered: s.recovered, HasBOM: s.hasBOM, EOL: s.eol}
}

func (s *Session) ReadAt(p []byte, off int64) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	return s.tree.ReadAt(p, off)
}

func (s *Session) Snapshot() (uint64, SnapshotLease, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, nil, ErrClosed
	}
	return s.revision, s.generation.acquire(s.tree.Snapshot()), nil
}

func (s *Session) ApplyBatch(ctx context.Context, expectedRevision uint64, operations []ReplaceOperation) (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ApplyResult{}, ErrClosed
	}
	if expectedRevision != s.revision {
		return ApplyResult{}, ErrRevisionConflict
	}
	if len(operations) == 0 {
		return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: s.dirty}, nil
	}
	if len(operations) > 256 {
		return ApplyResult{}, errors.New("document: transaction batch too large")
	}
	if err := s.applyOperationsLocked(ctx, operations, true); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: s.dirty}, nil
}

func (s *Session) applyOperationsLocked(ctx context.Context, operations []ReplaceOperation, recordHistory bool) error {
	if s.revision > math.MaxUint64-uint64(len(operations)) {
		return ErrRevisionOverflow
	}
	group := s.revision + 1
	epoch := s.undoEpoch
	staged, err := s.stageOperationsLocked(ctx, operations)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ensureJournalLocked(); err != nil {
		return err
	}
	recoveryOperations := make([]recovery.ReplaceOperation, len(staged))
	for index, operation := range staged {
		recoveryOperations[index] = recovery.ReplaceOperation{
			Start: operation.operation.Start, DeleteLength: operation.operation.DeleteLength, Inserted: operation.inserted,
		}
	}
	appendResult, err := s.journal.AppendReplaceBatch(s.revision+1, group, recoveryOperations)
	if err != nil {
		return err
	}
	finalTree, err := cloneDocumentTree(s.tree)
	if err != nil {
		_ = s.journal.RepairTail(appendResult.FrameOffset)
		return err
	}
	for index, operation := range staged {
		piece := store.Piece{}
		if len(operation.inserted) > 0 {
			piece = store.Piece{
				Source: store.SourceJournal, Offset: appendResult.PayloadOffsets[index], Length: int64(len(operation.inserted)),
				Newlines: int64(bytes.Count(operation.inserted, []byte{'\n'})), NewlinesKnown: true,
			}
		}
		if _, _, replaceErr := finalTree.ReplacePiece(operation.operation.Start, operation.operation.DeleteLength, piece); replaceErr != nil {
			repairErr := s.journal.RepairTail(appendResult.FrameOffset)
			return errors.Join(replaceErr, repairErr)
		}
	}

	entry := historyEntry{}
	if recordHistory {
		for _, operation := range staged {
			forwardRef, historyErr := s.historyText(operation.inserted)
			if historyErr != nil {
				return errors.Join(historyErr, s.journal.RepairTail(appendResult.FrameOffset))
			}
			inverseRef, historyErr := s.historyText(operation.deleted)
			if historyErr != nil {
				return errors.Join(historyErr, s.journal.RepairTail(appendResult.FrameOffset))
			}
			entry.forward = append(entry.forward, historyOperation{
				start: operation.operation.Start, deleteLength: operation.operation.DeleteLength, insert: forwardRef,
			})
			entry.inverse = append([]historyOperation{{
				start: operation.operation.Start, deleteLength: int64(len(operation.inserted)), insert: inverseRef,
			}}, entry.inverse...)
		}
	}

	s.tree = finalTree
	firstRevision := s.revision + 1
	for index, operation := range staged {
		revision := firstRevision + uint64(index)
		s.pending = append(s.pending, pendingOperation{
			revision: revision, group: group, start: operation.operation.Start, deleteLength: operation.operation.DeleteLength,
			insertOffset: appendResult.PayloadOffsets[index], insertLength: int64(len(operation.inserted)),
		})
	}
	s.revision += uint64(len(staged))
	s.dirty = s.revision > s.committedRevision
	if recordHistory && epoch == s.undoEpoch {
		s.undo = append(s.undo, entry)
		s.redo = nil
	}
	return nil
}

func (s *Session) stageOperationsLocked(ctx context.Context, operations []ReplaceOperation) ([]stagedOperation, error) {
	stagedTree, err := cloneDocumentTree(s.tree)
	if err != nil {
		return nil, err
	}
	var stagingBytes []byte
	result := make([]stagedOperation, 0, len(operations))
	for _, op := range operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !utf8.ValidString(op.Insert) || len(op.Insert) > 1<<20 {
			return nil, errors.New("document: invalid or oversized insertion")
		}
		length := stagedTree.Len()
		if op.Start < 0 || op.DeleteLength < 0 || op.Start > length || op.DeleteLength > length-op.Start {
			return nil, store.ErrInvalidRange
		}
		deleted, err := readTreeRange(stagedTree, op.Start, op.DeleteLength)
		if err != nil {
			return nil, err
		}
		inserted := []byte(op.Insert)
		offset := len(stagingBytes)
		stagingBytes = append(stagingBytes, inserted...)
		stagedTree.SetSource(stagingSourceID, bytes.NewReader(stagingBytes))
		piece := store.Piece{}
		if len(inserted) > 0 {
			piece = store.Piece{
				Source: stagingSourceID, Offset: int64(offset), Length: int64(len(inserted)),
				Newlines: int64(bytes.Count(inserted, []byte{'\n'})), NewlinesKnown: true,
			}
		}
		if _, _, err := stagedTree.ReplacePiece(op.Start, op.DeleteLength, piece); err != nil {
			return nil, err
		}
		result = append(result, stagedOperation{operation: op, inserted: inserted, deleted: deleted})
	}
	return result, nil
}

func cloneDocumentTree(source *store.Tree) (*store.Tree, error) {
	clone, err := store.New(nil, 0)
	if err != nil {
		return nil, err
	}
	clone.Restore(source.Snapshot())
	return clone, nil
}

func (s *Session) Undo() (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.undo) == 0 {
		return ApplyResult{}, ErrNothingToUndo
	}
	entry := s.undo[len(s.undo)-1]
	operations, err := s.materializeHistory(entry.inverse)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := s.applyOperationsLocked(context.Background(), operations, false); err != nil {
		return ApplyResult{}, err
	}
	s.undo = s.undo[:len(s.undo)-1]
	s.redo = append(s.redo, entry)
	return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: true}, nil
}

func (s *Session) Redo() (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.redo) == 0 {
		return ApplyResult{}, ErrNothingToRedo
	}
	entry := s.redo[len(s.redo)-1]
	operations, err := s.materializeHistory(entry.forward)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := s.applyOperationsLocked(context.Background(), operations, false); err != nil {
		return ApplyResult{}, err
	}
	s.redo = s.redo[:len(s.redo)-1]
	s.undo = append(s.undo, entry)
	return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: true}, nil
}

func (s *Session) materializeHistory(source []historyOperation) ([]ReplaceOperation, error) {
	result := make([]ReplaceOperation, 0, len(source))
	for _, operation := range source {
		insert, err := s.undoStore.read(operation.insert)
		if err != nil {
			return nil, err
		}
		result = append(result, ReplaceOperation{Start: operation.start, DeleteLength: operation.deleteLength, Insert: insert})
	}
	return result, nil
}

func (s *Session) historyText(value []byte) (textRef, error) {
	ref, err := s.undoStore.append(value)
	if err == nil {
		return ref, nil
	}
	if !errors.Is(err, errUndoQuota) {
		return textRef{}, err
	}
	s.undo, s.redo = nil, nil
	s.undoEpoch++
	if err := s.undoStore.reset(); err != nil {
		return textRef{}, err
	}
	ref, err = s.undoStore.append(value)
	if errors.Is(err, errUndoQuota) {
		// A single history value can exceed the quota (for example, deleting a
		// very large range). The edit remains valid but intentionally has no
		// undo entry after the epoch change above.
		return textRef{}, nil
	}
	return ref, err
}

func (s *Session) ensureJournalLocked() error {
	if s.journal != nil {
		return nil
	}
	path := filepath.Join(s.recoveryDir, journalPrefix(s.fingerprint)+"."+randomSuffix()+".docengine-journal")
	journal, _, err := recovery.Open(path, s.fingerprint)
	if err != nil {
		return err
	}
	s.journal = journal
	s.generation.attachJournal(journal)
	s.tree.SetSource(store.SourceJournal, journal)
	return nil
}

func (s *Session) Save() (Metadata, error) { return s.CommitAtLeast(0) }

// CommitAtLeast atomically persists a snapshot whose revision is at least the
// requested revision. New edits continue in the current generation while the
// snapshot is streamed.
func (s *Session) CommitAtLeast(expectedRevision uint64) (Metadata, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Metadata{}, ErrClosed
	}
	if expectedRevision > s.revision {
		s.mu.Unlock()
		return Metadata{}, ErrRevisionConflict
	}
	if !s.dirty || expectedRevision <= s.committedRevision && expectedRevision != 0 {
		metadata := s.metadataLocked()
		s.mu.Unlock()
		return metadata, nil
	}
	targetRevision := s.revision
	lease := s.generation.acquire(s.tree.Snapshot())
	path, mode, expectedIdentity := s.path, s.mode, s.diskIdentity
	prefix := []byte(nil)
	if s.hasBOM {
		prefix = []byte{0xef, 0xbb, 0xbf}
	}
	s.mu.Unlock()
	defer lease.Close()
	if s.commitHook != nil {
		s.commitHook("snapshot")
	}

	checkIdentity := func() error {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		if identityFor(info) != expectedIdentity {
			return ErrExternalChange
		}
		return nil
	}
	if err := checkIdentity(); err != nil {
		return Metadata{}, err
	}
	if _, err := save.AtomicChecked(path, mode, prefix, lease.WriteTo, checkIdentity); err != nil {
		return Metadata{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(path)
	if err != nil {
		return Metadata{}, err
	}
	newFingerprint := recovery.FingerprintFor(path, info)
	newBase, err := openBase(path)
	if err != nil {
		return Metadata{}, err
	}
	newTree, err := store.NewWithBasePiece(newBase, store.Piece{Source: store.SourceBase, Offset: boolOffset(s.hasBOM, 3), Length: info.Size() - boolOffset(s.hasBOM, 3)})
	if err != nil {
		_ = newBase.Close()
		return Metadata{}, err
	}
	newGeneration := newSourceGeneration(newBase, nil)
	remaining := make([]pendingOperation, 0)
	if s.revision > targetRevision {
		journalPath := filepath.Join(s.recoveryDir, journalPrefix(newFingerprint)+"."+randomSuffix()+".docengine-journal")
		newJournal, _, openErr := recovery.Open(journalPath, newFingerprint)
		if openErr != nil {
			newGeneration.retireAndWait(true)
			return Metadata{}, openErr
		}
		newGeneration.attachJournal(newJournal)
		newTree.SetSource(store.SourceJournal, newJournal)
		newer := make([]pendingOperation, 0, len(s.pending))
		for _, operation := range s.pending {
			if operation.revision > targetRevision {
				newer = append(newer, operation)
			}
		}
		for first := 0; first < len(newer); {
			last := first + 1
			for last < len(newer) && newer[last].group == newer[first].group {
				last++
			}
			batch := make([]recovery.ReplaceOperation, last-first)
			payloads := make([][]byte, last-first)
			for index := first; index < last; index++ {
				operation := newer[index]
				inserted := make([]byte, operation.insertLength)
				if operation.insertLength > 0 {
					n, readErr := s.journal.ReadAt(inserted, operation.insertOffset)
					if readErr != nil && !(errors.Is(readErr, io.EOF) && int64(n) == operation.insertLength) {
						newGeneration.retireAndWait(true)
						return Metadata{}, readErr
					}
				}
				batch[index-first] = recovery.ReplaceOperation{
					Start: operation.start, DeleteLength: operation.deleteLength, Inserted: inserted,
				}
				payloads[index-first] = inserted
			}
			appendResult, appendErr := newJournal.AppendReplaceBatch(newer[first].revision, newer[first].group, batch)
			if appendErr != nil {
				newGeneration.retireAndWait(true)
				return Metadata{}, appendErr
			}
			for index := first; index < last; index++ {
				inserted := payloads[index-first]
				piece := store.Piece{}
				if len(inserted) > 0 {
					piece = store.Piece{
						Source: store.SourceJournal, Offset: appendResult.PayloadOffsets[index-first], Length: int64(len(inserted)),
						Newlines: int64(bytes.Count(inserted, []byte{'\n'})), NewlinesKnown: true,
					}
				}
				operation := newer[index]
				if _, _, replaceErr := newTree.ReplacePiece(operation.start, operation.deleteLength, piece); replaceErr != nil {
					newGeneration.retireAndWait(true)
					return Metadata{}, replaceErr
				}
				operation.insertOffset = appendResult.PayloadOffsets[index-first]
				remaining = append(remaining, operation)
			}
			first = last
		}
	}
	oldGeneration := s.generation
	s.base, s.generation, s.tree = newBase, newGeneration, newTree
	s.journal = newGeneration.journal
	s.pending = remaining
	s.committedRevision = targetRevision
	s.fingerprint = newFingerprint
	s.diskIdentity = identityFor(info)
	s.dirty = s.revision > s.committedRevision
	if !s.dirty {
		s.recovered = false
	}
	oldGeneration.retire(true)
	return s.metadataLocked(), nil
}

func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stopSync)
	s.mu.Unlock()
	<-s.syncDone
	s.mu.Lock()
	dirty, generation := s.dirty, s.generation
	if s.journal != nil {
		_ = s.journal.Sync()
	}
	s.mu.Unlock()
	err := generation.retireAndWait(!dirty)
	return errors.Join(err, s.undoStore.close())
}

func (s *Session) syncLoop() {
	defer close(s.syncDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.RLock()
			journal := s.journal
			s.mu.RUnlock()
			if journal != nil {
				_ = journal.Sync()
			}
		case <-s.stopSync:
			return
		}
	}
}

func readTreeRange(tree *store.Tree, start, length int64) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}
	buffer := make([]byte, length)
	n, err := tree.ReadAt(buffer, start)
	if err != nil && !(errors.Is(err, io.EOF) && int64(n) == length) {
		return nil, err
	}
	return buffer[:n], nil
}

func detectEOL(sample []byte) EOLStyle {
	crlf := bytes.Count(sample, []byte("\r\n"))
	lf := bytes.Count(sample, []byte("\n")) - crlf
	if crlf > 0 && lf > 0 {
		return EOLMixed
	}
	if crlf > 0 {
		return EOLCRLF
	}
	return EOLLF
}

func journalPrefix(fingerprint recovery.Fingerprint) string {
	hash := sha256.Sum256(fingerprint.PathHash[:])
	return hex.EncodeToString(hash[:16])
}

func randomSuffix() string {
	buffer := make([]byte, 8)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

func identityFor(info os.FileInfo) fileIdentity {
	return fileIdentity{size: info.Size(), modTime: info.ModTime().UnixNano()}
}

func boolOffset(condition bool, value int64) int64 {
	if condition {
		return value
	}
	return 0
}
