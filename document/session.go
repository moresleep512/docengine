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

	"github.com/moresleep512/docengine/document/coordinate"
	"github.com/moresleep512/docengine/document/save"
	"github.com/moresleep512/docengine/document/store"
	"github.com/moresleep512/docengine/document/virtual"
	"github.com/moresleep512/docengine/recovery"
)

var (
	ErrRevisionConflict    = errors.New("document: revision conflict")
	ErrInvalidUTF8         = errors.New("document: file is not UTF-8")
	ErrInvalidUTF8Boundary = errors.New("document: edit is not aligned to UTF-8 boundaries")
	ErrInvalidContext      = errors.New("document: nil context")
	ErrClosed              = errors.New("document: session closed")
	ErrNothingToUndo       = errors.New("document: nothing to undo")
	ErrNothingToRedo       = errors.New("document: nothing to redo")
	ErrExternalChange      = errors.New("document: file changed on disk")
	ErrRevisionOverflow    = errors.New("document: revision overflow")
	ErrFaulted             = errors.New("document: session is faulted and read-only")
)

const scanBufferSize = 256 << 10

type EOLStyle string

const (
	EOLLF    EOLStyle = "lf"
	EOLCRLF  EOLStyle = "crlf"
	EOLMixed EOLStyle = "mixed"
)

type Metadata struct {
	Path                string
	ResolvedPath        string
	Name                string
	ByteLength          int64
	Revision            uint64
	CommittedRevision   uint64
	Dirty               bool
	Recovered           bool
	HasBOM              bool
	EOL                 EOLStyle
	DurabilityUncertain bool
	// RecoveryDurabilityUncertain reports a failed Sync of the recovery WAL.
	// The logical document remains readable and editable, but the newest edits
	// may not survive sudden power loss until a later Sync or save succeeds.
	RecoveryDurabilityUncertain bool
	PersistenceFaulted          bool
}

// RecoveryStats is an atomic view of recovery-journal growth and automatic
// checkpoint scheduling. JournalBytes is zero while no journal is open.
type RecoveryStats struct {
	JournalBytes              int64
	MaxJournalBytes           int64
	AutoCheckpointBytes       int64
	NextAutoCheckpointBytes   int64
	AutomaticCheckpoints      uint64
	AutomaticCheckpointQueued bool
}

type RecoveryOpenError struct {
	JournalPath     string
	QuarantinedPath string
	Reason          string
	Err             error
}

func (e *RecoveryOpenError) Error() string {
	return fmt.Sprintf("document: recovery journal %q was quarantined as %q (%s): %v", e.JournalPath, e.QuarantinedPath, e.Reason, e.Err)
}

func (e *RecoveryOpenError) Unwrap() error { return e.Err }

type ReplaceOperation struct {
	Start        int64
	DeleteLength int64
	Insert       string
}

type ApplyResult struct {
	Revision   uint64
	ByteLength int64
	Dirty      bool
	Changes    coordinate.ChangeMap
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
	insertNewlines             int64
}

type stagedOperation struct {
	operation ReplaceOperation
	inserted  []byte
	deleted   []byte
}

const stagingSourceID store.SourceID = 255

type sessionOperations struct {
	absolutePath   func(string) (string, error)
	evalSymlinks   func(string) (string, error)
	openBase       func(string) (*os.File, error)
	stat           func(string) (os.FileInfo, error)
	openRecovery   func(string, recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error)
	readRecovery   func(*recovery.Journal, []byte, int64) (int, error)
	sizeRecovery   func(*recovery.Journal) (int64, error)
	appendRecovery func(*recovery.Journal, uint64, uint64, []recovery.ReplaceOperation) (recovery.BatchAppendResult, error)
	syncRecovery   func(*recovery.Journal) error
	removeRecovery func(string) error
	newTree        func(io.ReaderAt, store.Piece) (*store.Tree, error)
	cloneTree      func(*store.Tree) (*store.Tree, error)
	atomicChecked  func(string, os.FileMode, []byte, func(io.Writer) (int64, error), func() error) (int64, error)
	syncParent     func(string) error
}

var systemSessionOperations = sessionOperations{
	absolutePath: filepath.Abs,
	evalSymlinks: resolvePath,
	openBase:     openBase,
	stat:         os.Stat,
	openRecovery: recovery.Open,
	readRecovery: func(journal *recovery.Journal, buffer []byte, offset int64) (int, error) {
		return journal.ReadAt(buffer, offset)
	},
	sizeRecovery: func(journal *recovery.Journal) (int64, error) { return journal.Size() },
	appendRecovery: func(journal *recovery.Journal, revision, group uint64, operations []recovery.ReplaceOperation) (recovery.BatchAppendResult, error) {
		return journal.AppendBatch(revision, group, operations)
	},
	syncRecovery:   func(journal *recovery.Journal) error { return journal.Sync() },
	removeRecovery: os.Remove,
	newTree:        store.NewWithBasePiece,
	cloneTree: func(source *store.Tree) (*store.Tree, error) {
		return cloneDocumentTree(source), nil
	},
	atomicChecked: save.AtomicChecked,
	syncParent:    save.SyncParent,
}

type fileIdentity struct {
	size        int64
	modTime     int64
	contentHash [32]byte
}

type Session struct {
	mu                         sync.RWMutex
	saveMu                     sync.Mutex
	path                       string
	resolvedPath               string
	mode                       os.FileMode
	base                       *os.File
	journal                    *recovery.Journal
	generation                 *sourceGeneration
	tree                       *store.Tree
	revision                   uint64
	committedRevision          uint64
	undo                       []historyEntry
	redo                       []historyEntry
	undoStore                  *undoStore
	sessionMarker              *sessionMarker
	undoEpoch                  uint64
	pending                    []pendingOperation
	dirty                      bool
	recovered                  bool
	hasBOM                     bool
	eol                        EOLStyle
	recoveryDir                string
	sessionDir                 string
	config                     SessionConfig
	fingerprint                recovery.Fingerprint
	diskIdentity               fileIdentity
	durabilityUncertain        bool
	fault                      error
	closed                     bool
	commitHook                 func(string)
	stopSync                   chan struct{}
	syncDone                   chan struct{}
	operations                 sessionOperations
	events                     *eventHub
	changeHistory              *changeHistory
	coordinateLineage          *coordinate.Lineage
	closeDone                  chan struct{}
	closeErr                   error
	nextPersistenceID          uint64
	journalSyncErr             error
	journalBytes               int64
	nextJournalCheckpoint      int64
	automaticCheckpoints       uint64
	checkpointRequest          chan uint64
	automaticCheckpointPending bool
}

func Open(path string, options OpenOptions) (*Session, error) {
	return OpenContext(context.Background(), path, options)
}

func OpenContext(ctx context.Context, path string, options OpenOptions) (*Session, error) {
	return openSessionContext(ctx, path, options, systemSessionOperations)
}

func openSession(path string, options OpenOptions, operations sessionOperations) (*Session, error) {
	return openSessionContext(context.Background(), path, options, operations)
}

func openSessionContext(ctx context.Context, path string, options OpenOptions, operations sessionOperations) (*Session, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config, err := resolveOpenOptions(options)
	if err != nil {
		return nil, err
	}
	absolute, err := operations.absolutePath(path)
	if err != nil {
		return nil, err
	}
	resolved, err := operations.evalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("document: resolve path: %w", err)
	}
	resolved, err = operations.absolutePath(resolved)
	if err != nil {
		return nil, err
	}
	base, err := operations.openBase(resolved)
	if err != nil {
		return nil, fmt.Errorf("document: open base: %w", err)
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
	scan, err := scanOpenedBase(ctx, base, resolved, info, operations.stat)
	if err != nil {
		_ = base.Close()
		return nil, fmt.Errorf("document: scan base: %w", err)
	}
	var marker *sessionMarker
	if config.SessionDirOwnership == DirectoryOwned {
		marker, err = openOwnedSessionMarker(config.SessionDir)
		if err != nil {
			_ = base.Close()
			return nil, err
		}
	}
	undo, err := openUndoStore(config.SessionDir, config.Limits.UndoBytes)
	if err != nil {
		_ = base.Close()
		return nil, errors.Join(err, marker.close(), cleanupOwnedDirectories(config, false))
	}
	fingerprint := recovery.FingerprintFor(resolved, scan.size, scan.contentHash)
	journal, replay, err := openMatchingJournalWith(config.RecoveryDir, fingerprint, operations.openRecovery)
	if err != nil {
		return nil, errors.Join(err, undo.close(), marker.close(), base.Close(), cleanupOwnedDirectories(config, true))
	}
	tree, err := operations.newTree(base, store.Piece{Source: store.SourceBase, Offset: scan.baseOffset, Length: scan.size - scan.baseOffset, Newlines: scan.newlines, NewlinesKnown: true})
	if err != nil {
		var closeErr error
		if journal != nil {
			closeErr = journal.Close()
		}
		return nil, errors.Join(err, closeErr, undo.close(), marker.close(), base.Close(), cleanupOwnedDirectories(config, true))
	}
	if journal != nil {
		tree.SetSource(store.SourceJournal, journal)
	}
	generation := newSourceGeneration(base, journal)
	if config.RecoveryDirOwnership == DirectoryOwned {
		generation.setJournalCleanupDirectory(config.RecoveryDir)
	}
	session := &Session{
		path: absolute, resolvedPath: resolved, mode: info.Mode(), base: base, journal: journal, generation: generation, tree: tree,
		undoStore: undo, sessionMarker: marker, hasBOM: scan.hasBOM, eol: scan.eol, recoveryDir: config.RecoveryDir,
		sessionDir: config.SessionDir, config: config, fingerprint: fingerprint, diskIdentity: identityFor(info, scan.contentHash),
		stopSync: make(chan struct{}), syncDone: make(chan struct{}), checkpointRequest: make(chan uint64, 1), operations: operations,
		events: newEventHub(config.Limits.EventHistory), coordinateLineage: coordinate.NewLineage(), closeDone: make(chan struct{}),
	}
	if journal != nil {
		if err := session.replay(replay); err != nil {
			return nil, errors.Join(err, generation.retireAndWait(false), undo.close(), marker.close(), cleanupOwnedDirectories(config, true))
		}
		if replay.Truncated {
			if err := journal.RepairTail(replay.ValidBytes); err != nil {
				return nil, errors.Join(err, generation.retireAndWait(false), undo.close(), marker.close(), cleanupOwnedDirectories(config, true))
			}
		}
		session.journalBytes, err = operations.sizeRecovery(journal)
		if err != nil {
			return nil, errors.Join(err, generation.retireAndWait(false), undo.close(), marker.close(), cleanupOwnedDirectories(config, true))
		}
	}
	session.resetJournalCheckpointLocked()
	session.changeHistory = newChangeHistory(config.Limits.ChangeHistory, session.revision, session.tree.Len())
	session.publishEventLocked(EventOpened, ChangeOriginNone, coordinate.ChangeMap{}, nil)
	if session.recovered {
		session.publishEventLocked(EventRecovered, ChangeOriginNone, coordinate.ChangeMap{}, nil)
	}
	session.maybeScheduleJournalCheckpointLocked()
	go session.syncLoop()
	return session, nil
}

func openMatchingJournal(dir string, fingerprint recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error) {
	return openMatchingJournalWith(dir, fingerprint, recovery.Open)
}

func openMatchingJournalWith(dir string, fingerprint recovery.Fingerprint, openRecovery func(string, recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error)) (*recovery.Journal, recovery.ReplayResult, error) {
	return openMatchingJournalWithQuarantine(dir, fingerprint, openRecovery, quarantineRecovery)
}

func openMatchingJournalWithQuarantine(
	dir string,
	fingerprint recovery.Fingerprint,
	openRecovery func(string, recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error),
	quarantine func(string, string, error) error,
) (*recovery.Journal, recovery.ReplayResult, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, recovery.ReplayResult{}, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, journalPrefix(fingerprint)+"*.docengine-journal-v2"))
	if err != nil {
		return nil, recovery.ReplayResult{}, err
	}
	sort.Slice(paths, func(i, j int) bool {
		left, _ := os.Stat(paths[i])
		right, _ := os.Stat(paths[j])
		return left != nil && right != nil && left.ModTime().After(right.ModTime())
	})
	if len(paths) == 0 {
		return nil, recovery.ReplayResult{}, nil
	}
	matching := make([]string, 0, len(paths))
	var probeErrors []error
	for _, path := range paths {
		stored, probeErr := recovery.ReadFingerprint(path)
		if probeErr != nil {
			probeErrors = append(probeErrors, quarantine(path, "invalid-header", probeErr))
			continue
		}
		if stored == fingerprint {
			matching = append(matching, path)
		}
	}
	if len(probeErrors) != 0 {
		return nil, recovery.ReplayResult{}, errors.Join(probeErrors...)
	}
	if len(matching) > 1 {
		quarantineErrors := make([]error, 0, len(paths))
		for _, path := range matching {
			quarantineErrors = append(quarantineErrors, quarantine(path, "ambiguous", errors.New("multiple recovery journals exist")))
		}
		return nil, recovery.ReplayResult{}, errors.Join(quarantineErrors...)
	}
	if len(matching) == 0 {
		quarantineErrors := make([]error, 0, len(paths))
		for _, path := range paths {
			quarantineErrors = append(quarantineErrors, quarantine(path, "base-mismatch", recovery.ErrStaleJournal))
		}
		return nil, recovery.ReplayResult{}, errors.Join(quarantineErrors...)
	}
	path := matching[0]
	var retiredErrors []error
	for _, candidate := range paths {
		if candidate == path {
			continue
		}
		retired := quarantine(candidate, "retired-base", recovery.ErrStaleJournal)
		var recoveryErr *RecoveryOpenError
		if !errors.As(retired, &recoveryErr) || recoveryErr.QuarantinedPath == "" {
			retiredErrors = append(retiredErrors, retired)
		}
	}
	if len(retiredErrors) != 0 {
		return nil, recovery.ReplayResult{}, errors.Join(retiredErrors...)
	}
	journal, replay, openErr := openRecovery(path, fingerprint)
	if openErr != nil {
		return nil, recovery.ReplayResult{}, quarantine(path, "open-failed", openErr)
	}
	return journal, replay, nil
}

func quarantineRecovery(path, reason string, cause error) error {
	return quarantineRecoveryWith(path, reason, cause, os.Rename)
}

func quarantineRecoveryWith(path, reason string, cause error, rename func(string, string) error) error {
	quarantined := fmt.Sprintf("%s.quarantine-%s-%d", path, reason, time.Now().UnixNano())
	if err := rename(path, quarantined); err != nil {
		return &RecoveryOpenError{JournalPath: path, Reason: reason, Err: errors.Join(cause, err)}
	}
	return &RecoveryOpenError{JournalPath: path, QuarantinedPath: quarantined, Reason: reason, Err: cause}
}

func (s *Session) replay(result recovery.ReplayResult) error {
	for batchPosition, batch := range result.Batches {
		if batchPosition == 0 {
			if batch.FirstRevision == 0 {
				return errors.New("document: zero journal revision")
			}
			s.committedRevision = batch.FirstRevision - 1
			s.revision = s.committedRevision
		}
		if batch.FirstRevision != s.revision+1 || batch.Group == 0 || len(batch.Operations) == 0 {
			return errors.New("document: non-monotonic journal batch")
		}
		entry := historyEntry{}
		for operationPosition, operation := range batch.Operations {
			revision := batch.FirstRevision + uint64(operationPosition)
			deleted, err := readTreeRange(s.tree, operation.Start, operation.DeleteLength)
			if err != nil {
				return err
			}
			if err := validateUTF8ReplacementBoundaries(s.tree, operation.Start, operation.DeleteLength); err != nil {
				return fmt.Errorf("replay revision %d: %w", revision, err)
			}
			inserted := make([]byte, operation.InsertLength)
			if operation.InsertLength > 0 {
				n, readErr := s.operations.readRecovery(s.journal, inserted, operation.PayloadOffset)
				if readErr != nil && !(errors.Is(readErr, io.EOF) && n == len(inserted)) {
					return readErr
				}
				if n != len(inserted) {
					return io.ErrUnexpectedEOF
				}
			}
			if !utf8.Valid(inserted) {
				return fmt.Errorf("replay revision %d: %w", revision, ErrInvalidUTF8)
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
			if operation.InsertLength > 0 {
				piece = store.Piece{Source: store.SourceJournal, Offset: operation.PayloadOffset, Length: operation.InsertLength, Newlines: int64(bytes.Count(inserted, []byte{'\n'})), NewlinesKnown: true}
			}
			_, _, err = s.tree.ReplacePiece(operation.Start, operation.DeleteLength, piece)
			if err != nil {
				return fmt.Errorf("replay revision %d: %w", revision, err)
			}
			entry.forward = append(entry.forward, historyOperation{start: operation.Start, deleteLength: operation.DeleteLength, insert: forwardRef})
			entry.inverse = append([]historyOperation{{start: operation.Start, deleteLength: operation.InsertLength, insert: inverseRef}}, entry.inverse...)
			s.pending = append(s.pending, pendingOperation{
				revision: revision, group: batch.Group, start: operation.Start, deleteLength: operation.DeleteLength,
				insertOffset: operation.PayloadOffset, insertLength: operation.InsertLength,
				insertNewlines: int64(bytes.Count(inserted, []byte{'\n'})),
			})
			s.revision = revision
		}
		s.undo = append(s.undo, entry)
	}
	if len(result.Batches) > 0 {
		s.dirty, s.recovered = true, true
	}
	return nil
}

func (s *Session) Metadata() Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadataLocked()
}

// Config returns the immutable, fully resolved resource and directory policy
// used by this Session. It remains available after Close.
func (s *Session) Config() SessionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// RecoveryStats returns recovery-journal growth and checkpoint scheduling
// state without touching the filesystem. It remains available after Close.
func (s *Session) RecoveryStats() RecoveryStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recoveryStatsLocked()
}

// Subscribe creates a nonblocking, ordered Session event stream. Historical
// replay and live publication are joined atomically with respect to Session
// transitions.
func (s *Session) Subscribe(options SubscribeOptions) (*Subscription, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}
	return s.events.subscribe(options)
}

func (s *Session) publishEventLocked(kind EventKind, origin ChangeOrigin, changes coordinate.ChangeMap, cause error) {
	s.events.publish(SessionEvent{
		Kind: kind, Origin: origin, Metadata: s.metadataLocked(), Changes: changes, Cause: cause,
	})
}

func (s *Session) recordJournalSyncResultLocked(journal *recovery.Journal, syncErr error) {
	if journal != s.journal {
		return
	}
	if syncErr != nil {
		firstFailure := s.journalSyncErr == nil
		s.journalSyncErr = syncErr
		if firstFailure {
			s.publishEventLocked(EventJournalSyncFailed, ChangeOriginNone, coordinate.ChangeMap{}, syncErr)
		}
		return
	}
	if s.journalSyncErr != nil {
		s.journalSyncErr = nil
		s.publishEventLocked(EventJournalSyncRestored, ChangeOriginNone, coordinate.ChangeMap{}, nil)
	}
}

func (s *Session) metadataLocked() Metadata {
	return Metadata{Path: s.path, ResolvedPath: s.resolvedPath, Name: filepath.Base(s.path), ByteLength: s.tree.Len(), Revision: s.revision, CommittedRevision: s.committedRevision, Dirty: s.dirty, Recovered: s.recovered, HasBOM: s.hasBOM, EOL: s.eol, DurabilityUncertain: s.durabilityUncertain, RecoveryDurabilityUncertain: s.journalSyncErr != nil, PersistenceFaulted: s.fault != nil}
}

func (s *Session) recoveryStatsLocked() RecoveryStats {
	return RecoveryStats{
		JournalBytes:              s.journalBytes,
		MaxJournalBytes:           s.config.Limits.MaxJournalBytes,
		AutoCheckpointBytes:       s.config.AutoCheckpointJournalBytes,
		NextAutoCheckpointBytes:   s.nextJournalCheckpoint,
		AutomaticCheckpoints:      s.automaticCheckpoints,
		AutomaticCheckpointQueued: s.automaticCheckpointPending,
	}
}

func (s *Session) resetJournalCheckpointLocked() {
	s.nextJournalCheckpoint = s.config.AutoCheckpointJournalBytes
}

func (s *Session) maybeScheduleJournalCheckpointLocked() {
	threshold := s.config.AutoCheckpointJournalBytes
	if threshold == 0 || s.closed || s.fault != nil || s.journal == nil ||
		s.journalBytes < s.nextJournalCheckpoint || s.automaticCheckpointPending {
		return
	}
	s.nextJournalCheckpoint = nextCheckpointThreshold(s.journalBytes, threshold)
	s.automaticCheckpointPending = true
	s.checkpointRequest <- s.revision
}

func nextCheckpointThreshold(current, threshold int64) int64 {
	if current > math.MaxInt64-threshold {
		return math.MaxInt64
	}
	return current + threshold
}

// Fault returns the cause that placed this Session into its permanent
// read-only state. It returns nil for a healthy Session.
func (s *Session) Fault() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fault
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

// CoordinateIndex builds a bounded-query UTF-8 coordinate index for one
// immutable Session revision. The returned Index owns its Snapshot lease and
// must be closed by the caller.
func (s *Session) CoordinateIndex(ctx context.Context, options coordinate.Options) (*coordinate.Index, error) {
	revision, snapshot, err := s.Snapshot()
	if err != nil {
		return nil, err
	}
	options.Lineage = s.coordinateLineage
	return coordinate.BuildOwned(ctx, snapshot, revision, options)
}

// VirtualPager builds a format-neutral logical Page and Fragment pager for one
// immutable Session revision. The returned Pager owns its Snapshot lease and
// must be closed by the caller.
func (s *Session) VirtualPager(ctx context.Context, options virtual.Options) (*virtual.Pager, error) {
	if ctx == nil {
		return nil, virtual.ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	revision, snapshot, err := s.Snapshot()
	if err != nil {
		return nil, err
	}
	return virtual.BuildOwned(ctx, snapshot, revision, options)
}

// RebuildCoordinateIndex derives the current revision's index from a previous
// Session index and the exact ChangeMap chain between them. The new index keeps
// its own Snapshot lease; the previous index remains independently usable.
func (s *Session) RebuildCoordinateIndex(ctx context.Context, previous *coordinate.Index, changes coordinate.ChangeMap) (*coordinate.Index, error) {
	if ctx == nil {
		return nil, coordinate.ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if previous == nil {
		return nil, coordinate.ErrInvalidIndex
	}
	if !previous.BelongsTo(s.coordinateLineage) {
		return nil, coordinate.ErrLineageMismatch
	}
	revision, snapshot, err := s.Snapshot()
	if err != nil {
		return nil, err
	}
	if revision != changes.AfterRevision() {
		return nil, errors.Join(coordinate.ErrRevisionMismatch, snapshot.Close())
	}
	return coordinate.RebuildOwned(ctx, snapshot, previous, changes)
}

// RefreshCoordinateIndex rebuilds the current index from a previous index made
// by this Session and the retained ChangeMap chain between revisions.
func (s *Session) RefreshCoordinateIndex(ctx context.Context, previous *coordinate.Index) (*coordinate.Index, error) {
	if ctx == nil {
		return nil, coordinate.ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if previous == nil {
		return nil, coordinate.ErrInvalidIndex
	}
	if !previous.BelongsTo(s.coordinateLineage) {
		return nil, coordinate.ErrLineageMismatch
	}
	previousStats := previous.Stats()
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, ErrClosed
	}
	history := s.changeHistory.clone()
	snapshot := s.generation.acquire(s.tree.Snapshot())
	s.mu.RUnlock()
	changes, err := history.between(previousStats.Revision, history.currentRevision)
	if err != nil {
		return nil, errors.Join(err, snapshot.Close())
	}
	return coordinate.RebuildOwned(ctx, snapshot, previous, changes)
}

func (s *Session) ApplyBatch(ctx context.Context, expectedRevision uint64, operations []ReplaceOperation) (ApplyResult, error) {
	if ctx == nil {
		return ApplyResult{}, ErrInvalidContext
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ApplyResult{}, ErrClosed
	}
	if s.fault != nil {
		return ApplyResult{}, errors.Join(ErrFaulted, s.fault)
	}
	if expectedRevision != s.revision {
		return ApplyResult{}, ErrRevisionConflict
	}
	if len(operations) == 0 {
		changes, _ := coordinate.Identity(s.revision, s.tree.Len()) // Session lengths are always nonnegative.
		return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: s.dirty, Changes: changes}, nil
	}
	if len(operations) > s.config.Limits.MaxBatchOperations {
		return ApplyResult{}, fmt.Errorf("%w: transaction has %d operations, maximum is %d", ErrLimitExceeded, len(operations), s.config.Limits.MaxBatchOperations)
	}
	changes, err := s.applyOperationsLocked(ctx, operations, true)
	if err != nil {
		return ApplyResult{}, err
	}
	s.changeHistory.append(changes)
	s.publishEventLocked(EventChanged, ChangeOriginApply, changes, nil)
	return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: s.dirty, Changes: changes}, nil
}

func (s *Session) applyOperationsLocked(ctx context.Context, operations []ReplaceOperation, recordHistory bool) (coordinate.ChangeMap, error) {
	if s.revision > math.MaxUint64-uint64(len(operations)) {
		return coordinate.ChangeMap{}, ErrRevisionOverflow
	}
	beforeRevision, beforeLength := s.revision, s.tree.Len()
	group := s.revision + 1
	epoch := s.undoEpoch
	staged, err := s.stageOperationsLocked(ctx, operations)
	if err != nil {
		return coordinate.ChangeMap{}, err
	}
	edits := make([]coordinate.Edit, len(staged))
	for index, operation := range staged {
		edits[index] = coordinate.Edit{
			Start: operation.operation.Start, OldLength: operation.operation.DeleteLength, NewLength: int64(len(operation.inserted)),
		}
	}
	// Staging has already proved every range and intermediate length valid.
	changes, _ := coordinate.NewChangeMap(beforeRevision, beforeRevision+uint64(len(staged)), beforeLength, edits)
	if err := ctx.Err(); err != nil {
		return coordinate.ChangeMap{}, err
	}
	recoveryOperations := make([]recovery.ReplaceOperation, len(staged))
	for index, operation := range staged {
		recoveryOperations[index] = recovery.ReplaceOperation{
			Start: operation.operation.Start, DeleteLength: operation.operation.DeleteLength, Inserted: operation.inserted,
		}
	}
	// Staging and the revision-overflow check above establish every invariant
	// required by BatchEncodedSize.
	batchBytes, _ := recovery.BatchEncodedSize(s.revision+1, group, recoveryOperations)
	anticipatedBytes := s.journalBytes
	if s.journal == nil {
		anticipatedBytes = recovery.EmptyJournalBytes
	}
	if err := checkJournalQuota(anticipatedBytes, batchBytes, s.config.Limits.MaxJournalBytes); err != nil {
		return coordinate.ChangeMap{}, err
	}
	if err := s.ensureJournalLocked(); err != nil {
		return coordinate.ChangeMap{}, err
	}
	if err := checkJournalQuota(s.journalBytes, batchBytes, s.config.Limits.MaxJournalBytes); err != nil {
		return coordinate.ChangeMap{}, err
	}
	appendResult, err := s.operations.appendRecovery(s.journal, s.revision+1, group, recoveryOperations)
	if err != nil {
		return coordinate.ChangeMap{}, err
	}
	finalTree, err := s.operations.cloneTree(s.tree)
	if err != nil {
		_ = s.journal.RepairTail(appendResult.BatchOffset)
		return coordinate.ChangeMap{}, err
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
			repairErr := s.journal.RepairTail(appendResult.BatchOffset)
			return coordinate.ChangeMap{}, errors.Join(replaceErr, repairErr)
		}
	}

	entry := historyEntry{}
	if recordHistory {
		for _, operation := range staged {
			forwardRef, historyErr := s.historyText(operation.inserted)
			if historyErr != nil {
				return coordinate.ChangeMap{}, errors.Join(historyErr, s.journal.RepairTail(appendResult.BatchOffset))
			}
			inverseRef, historyErr := s.historyText(operation.deleted)
			if historyErr != nil {
				return coordinate.ChangeMap{}, errors.Join(historyErr, s.journal.RepairTail(appendResult.BatchOffset))
			}
			entry.forward = append(entry.forward, historyOperation{
				start: operation.operation.Start, deleteLength: operation.operation.DeleteLength, insert: forwardRef,
			})
			entry.inverse = append([]historyOperation{{
				start: operation.operation.Start, deleteLength: int64(len(operation.inserted)), insert: inverseRef,
			}}, entry.inverse...)
		}
	}

	s.journalBytes = appendResult.EndOffset
	s.tree = finalTree
	firstRevision := s.revision + 1
	for index, operation := range staged {
		revision := firstRevision + uint64(index)
		s.pending = append(s.pending, pendingOperation{
			revision: revision, group: group, start: operation.operation.Start, deleteLength: operation.operation.DeleteLength,
			insertOffset: appendResult.PayloadOffsets[index], insertLength: int64(len(operation.inserted)),
			insertNewlines: int64(bytes.Count(operation.inserted, []byte{'\n'})),
		})
	}
	s.revision += uint64(len(staged))
	s.dirty = s.revision > s.committedRevision
	if recordHistory && epoch == s.undoEpoch {
		s.undo = append(s.undo, entry)
		s.redo = nil
	}
	s.maybeScheduleJournalCheckpointLocked()
	return changes, nil
}

func checkJournalQuota(currentBytes, batchBytes, maximumBytes int64) error {
	if currentBytes <= maximumBytes-batchBytes {
		return nil
	}
	return fmt.Errorf("%w: recovery journal has %d bytes, batch adds %d, maximum is %d",
		ErrLimitExceeded, currentBytes, batchBytes, maximumBytes)
}

func (s *Session) stageOperationsLocked(ctx context.Context, operations []ReplaceOperation) ([]stagedOperation, error) {
	stagedTree, err := s.operations.cloneTree(s.tree)
	if err != nil {
		return nil, err
	}
	var stagingBytes []byte
	result := make([]stagedOperation, 0, len(operations))
	for _, op := range operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !utf8.ValidString(op.Insert) {
			return nil, ErrInvalidUTF8
		}
		if int64(len(op.Insert)) > s.config.Limits.MaxInsertBytes {
			return nil, fmt.Errorf("%w: insertion has %d bytes, maximum is %d", ErrLimitExceeded, len(op.Insert), s.config.Limits.MaxInsertBytes)
		}
		length := stagedTree.Len()
		if op.Start < 0 || op.DeleteLength < 0 || op.Start > length || op.DeleteLength > length-op.Start {
			return nil, store.ErrInvalidRange
		}
		if err := validateUTF8ReplacementBoundaries(stagedTree, op.Start, op.DeleteLength); err != nil {
			return nil, err
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

func cloneDocumentTree(source *store.Tree) *store.Tree {
	clone, _ := store.New(nil, 0) // Empty trees are valid by construction.
	clone.Restore(source.Snapshot())
	return clone
}

func (s *Session) Undo() (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ApplyResult{}, ErrClosed
	}
	if s.fault != nil {
		return ApplyResult{}, errors.Join(ErrFaulted, s.fault)
	}
	if len(s.undo) == 0 {
		return ApplyResult{}, ErrNothingToUndo
	}
	entry := s.undo[len(s.undo)-1]
	operations, err := s.materializeHistory(entry.inverse)
	if err != nil {
		return ApplyResult{}, err
	}
	changes, err := s.applyOperationsLocked(context.Background(), operations, false)
	if err != nil {
		return ApplyResult{}, err
	}
	s.undo = s.undo[:len(s.undo)-1]
	s.redo = append(s.redo, entry)
	s.changeHistory.append(changes)
	s.publishEventLocked(EventChanged, ChangeOriginUndo, changes, nil)
	return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: true, Changes: changes}, nil
}

func (s *Session) Redo() (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ApplyResult{}, ErrClosed
	}
	if s.fault != nil {
		return ApplyResult{}, errors.Join(ErrFaulted, s.fault)
	}
	if len(s.redo) == 0 {
		return ApplyResult{}, ErrNothingToRedo
	}
	entry := s.redo[len(s.redo)-1]
	operations, err := s.materializeHistory(entry.forward)
	if err != nil {
		return ApplyResult{}, err
	}
	changes, err := s.applyOperationsLocked(context.Background(), operations, false)
	if err != nil {
		return ApplyResult{}, err
	}
	s.redo = s.redo[:len(s.redo)-1]
	s.undo = append(s.undo, entry)
	s.changeHistory.append(changes)
	s.publishEventLocked(EventChanged, ChangeOriginRedo, changes, nil)
	return ApplyResult{Revision: s.revision, ByteLength: s.tree.Len(), Dirty: true, Changes: changes}, nil
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
	path := filepath.Join(s.recoveryDir, journalPrefix(s.fingerprint)+"."+randomSuffix()+".docengine-journal-v2")
	journal, _, err := s.operations.openRecovery(path, s.fingerprint)
	if err != nil {
		return err
	}
	size, err := s.operations.sizeRecovery(journal)
	if err != nil {
		return errors.Join(err, journal.Close(), s.operations.removeRecovery(path))
	}
	s.journal = journal
	s.generation.attachJournal(journal)
	s.tree.SetSource(store.SourceJournal, journal)
	s.journalBytes = size
	return nil
}

type preparedJournalRebase struct {
	journal *recovery.Journal
	path    string
	pending []pendingOperation
	bytes   int64
}

func (s *Session) discardPreparedJournal(prepared *preparedJournalRebase, remove bool) error {
	if prepared == nil || prepared.journal == nil {
		return nil
	}
	closeErr := prepared.journal.Close()
	prepared.journal = nil
	if !remove {
		return closeErr
	}
	removeErr := s.operations.removeRecovery(prepared.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

// prepareJournalRebaseLocked builds and syncs the recovery journal that will
// belong to the replacement base. The caller holds s.mu, preventing an edit
// from landing only in the old journal after this snapshot is taken.
func (s *Session) prepareJournalRebaseLocked(fingerprint recovery.Fingerprint, targetRevision uint64) (*preparedJournalRebase, error) {
	newer := make([]pendingOperation, 0, len(s.pending))
	for _, operation := range s.pending {
		if operation.revision > targetRevision {
			newer = append(newer, operation)
		}
	}
	path := filepath.Join(s.recoveryDir, journalPrefix(fingerprint)+"."+randomSuffix()+".docengine-journal-v2")
	journal, replay, err := s.operations.openRecovery(path, fingerprint)
	if err != nil {
		return nil, err
	}
	prepared := &preparedJournalRebase{journal: journal, path: path}
	fail := func(cause error) (*preparedJournalRebase, error) {
		return nil, errors.Join(cause, s.discardPreparedJournal(prepared, true))
	}
	if replay.Truncated || len(replay.Batches) != 0 {
		return fail(errors.New("document: new recovery journal was not empty"))
	}
	size, err := s.operations.sizeRecovery(journal)
	if err != nil {
		return fail(err)
	}
	prepared.bytes = size
	for first := 0; first < len(newer); {
		last := first + 1
		for last < len(newer) && newer[last].group == newer[first].group {
			last++
		}
		batch := make([]recovery.ReplaceOperation, last-first)
		for index := first; index < last; index++ {
			operation := newer[index]
			inserted := make([]byte, operation.insertLength)
			if operation.insertLength > 0 {
				n, readErr := s.operations.readRecovery(s.journal, inserted, operation.insertOffset)
				if readErr != nil && !(errors.Is(readErr, io.EOF) && int64(n) == operation.insertLength) {
					return fail(readErr)
				}
				if int64(n) != operation.insertLength {
					return fail(io.ErrUnexpectedEOF)
				}
			}
			batch[index-first] = recovery.ReplaceOperation{
				Start: operation.start, DeleteLength: operation.deleteLength, Inserted: inserted,
			}
		}
		// These operations are a subset of the already quota-checked current
		// journal, so their rebased encoding cannot exceed MaxJournalBytes.
		appendResult, appendErr := s.operations.appendRecovery(journal, newer[first].revision, newer[first].group, batch)
		if appendErr != nil {
			return fail(appendErr)
		}
		prepared.bytes = appendResult.EndOffset
		for index := first; index < last; index++ {
			operation := newer[index]
			operation.insertOffset = appendResult.PayloadOffsets[index-first]
			prepared.pending = append(prepared.pending, operation)
		}
		first = last
	}
	if err := s.operations.syncRecovery(journal); err != nil {
		return fail(err)
	}
	if err := s.operations.syncParent(path); err != nil {
		var durability *save.DurabilityError
		if errors.As(err, &durability) {
			err = durability.Err
		}
		return fail(fmt.Errorf("document: sync prepared recovery directory: %w", err))
	}
	return prepared, nil
}

func (s *Session) Save() (Metadata, error) { return s.CommitAtLeast(0) }

// CommitAtLeast atomically persists a snapshot whose revision is at least the
// requested revision. New edits continue in the current generation while the
// snapshot is streamed.
func (s *Session) CommitAtLeast(expectedRevision uint64) (result Metadata, resultErr error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	var persistence PersistenceProgress
	var persistenceMetadata Metadata
	persistenceStarted, persistenceFinished := false, false
	defer func() {
		if !persistenceStarted || persistenceFinished || resultErr == nil {
			return
		}
		s.mu.RLock()
		metadata := s.metadataLocked()
		persistence.Committed = s.committedRevision >= persistence.TargetRevision
		s.mu.RUnlock()
		s.publishPersistenceEvent(EventSaveFailed, metadata, persistence, resultErr)
	}()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Metadata{}, ErrClosed
	}
	if s.fault != nil {
		metadata, fault := s.metadataLocked(), s.fault
		s.mu.Unlock()
		return metadata, errors.Join(ErrFaulted, fault)
	}
	if expectedRevision > s.revision {
		s.mu.Unlock()
		return Metadata{}, ErrRevisionConflict
	}
	if !s.dirty || expectedRevision <= s.committedRevision && expectedRevision != 0 {
		if s.durabilityUncertain {
			path := s.resolvedPath
			persistence, persistenceMetadata = s.beginPersistenceLocked(s.committedRevision, s.diskIdentity.size)
			persistenceStarted = true
			s.mu.Unlock()
			if err := s.operations.syncParent(path); err != nil {
				s.mu.RLock()
				metadata := s.metadataLocked()
				s.mu.RUnlock()
				return metadata, err
			}
			s.mu.Lock()
			s.durabilityUncertain = false
			metadata := s.metadataLocked()
			persistence.CompletedBytes, persistence.Committed = persistence.TotalBytes, true
			s.publishPersistenceEvent(EventSaved, metadata, persistence, nil)
			persistenceFinished = true
			s.mu.Unlock()
			return metadata, nil
		}
		metadata := s.metadataLocked()
		s.mu.Unlock()
		return metadata, nil
	}
	targetRevision := s.revision
	lease := s.generation.acquire(s.tree.Snapshot())
	path, mode, expectedIdentity := s.resolvedPath, s.mode, s.diskIdentity
	prefix := []byte(nil)
	if s.hasBOM {
		prefix = []byte{0xef, 0xbb, 0xbf}
	}
	if lease.Len() > math.MaxInt64-int64(len(prefix)) {
		_ = lease.Close()
		s.mu.Unlock()
		return Metadata{}, store.ErrLengthOverflow
	}
	persistence, persistenceMetadata = s.beginPersistenceLocked(targetRevision, lease.Len()+int64(len(prefix)))
	persistenceStarted = true
	s.mu.Unlock()
	defer lease.Close()
	if s.commitHook != nil {
		s.commitHook("snapshot")
	}

	quickInfo, err := s.operations.stat(path)
	if err != nil {
		return Metadata{}, err
	}
	if quickInfo.Size() != expectedIdentity.size {
		return Metadata{}, ErrExternalChange
	}
	if quickInfo.ModTime().UnixNano() != expectedIdentity.modTime {
		current, scanErr := scanDiskIdentity(context.Background(), path, s.operations)
		if scanErr != nil {
			return Metadata{}, scanErr
		}
		if current.size != expectedIdentity.size || current.contentHash != expectedIdentity.contentHash {
			return Metadata{}, ErrExternalChange
		}
	}
	hasher := sha256.New()
	_, _ = hasher.Write(prefix)
	var (
		newContentHash [32]byte
		newFingerprint recovery.Fingerprint
		prepared       *preparedJournalRebase
		boundaryLocked bool
	)
	checkIdentity := func() error {
		s.mu.Lock()
		boundaryLocked = true
		unlock := func(err error) error {
			boundaryLocked = false
			s.mu.Unlock()
			return err
		}
		current, scanErr := scanDiskIdentity(context.Background(), path, s.operations)
		if scanErr != nil {
			return unlock(scanErr)
		}
		if current.size != expectedIdentity.size || current.contentHash != expectedIdentity.contentHash {
			return unlock(ErrExternalChange)
		}
		copy(newContentHash[:], hasher.Sum(nil))
		newFingerprint = recovery.FingerprintFor(path, persistence.TotalBytes, newContentHash)
		var prepareErr error
		prepared, prepareErr = s.prepareJournalRebaseLocked(newFingerprint, targetRevision)
		if prepareErr != nil {
			return unlock(prepareErr)
		}
		// Keep s.mu held through the atomic replacement. No edit can land only
		// in the old journal after the synced replacement journal was prepared.
		return nil
	}
	writeContent := func(writer io.Writer) (int64, error) {
		progressWriter := newSaveProgressWriter(writer, int64(len(prefix)), persistence.TotalBytes, func(completed int64) {
			persistence.CompletedBytes = completed
			s.publishPersistenceEvent(EventSaveProgress, persistenceMetadata, persistence, nil)
		})
		written, writeErr := lease.WriteTo(io.MultiWriter(progressWriter, hasher))
		if writeErr == nil {
			progressWriter.finish()
		}
		return written, writeErr
	}
	total, atomicErr := s.operations.atomicChecked(path, mode, prefix, writeContent, checkIdentity)
	var durabilityErr *save.DurabilityError
	if atomicErr != nil && !errors.As(atomicErr, &durabilityErr) {
		if boundaryLocked {
			discardErr := s.discardPreparedJournal(prepared, true)
			s.mu.Unlock()
			boundaryLocked = false
			return Metadata{}, errors.Join(atomicErr, discardErr)
		}
		return Metadata{}, atomicErr
	}
	if !boundaryLocked {
		return Metadata{}, errors.New("document: atomic replacement skipped its final identity check")
	}
	defer s.mu.Unlock()
	commitFailure := func(cause error) (Metadata, error) {
		cause = errors.Join(cause, s.discardPreparedJournal(prepared, false))
		s.committedRevision = targetRevision
		s.diskIdentity = fileIdentity{size: total, contentHash: newContentHash}
		s.dirty = s.revision > s.committedRevision
		s.durabilityUncertain = durabilityErr != nil
		s.fault = cause
		return s.metadataLocked(), errors.Join(ErrFaulted, cause, atomicErr)
	}
	info, err := s.operations.stat(path)
	if err != nil {
		return commitFailure(err)
	}
	if info.Size() != total {
		return commitFailure(errors.New("document: committed file length mismatch"))
	}
	newBase, err := s.operations.openBase(path)
	if err != nil {
		return commitFailure(err)
	}
	newTree, err := s.operations.newTree(newBase, store.Piece{Source: store.SourceBase, Offset: boolOffset(s.hasBOM, 3), Length: info.Size() - boolOffset(s.hasBOM, 3)})
	if err != nil {
		_ = newBase.Close()
		return commitFailure(err)
	}
	newGeneration := newSourceGeneration(newBase, nil)
	if s.config.RecoveryDirOwnership == DirectoryOwned {
		newGeneration.setJournalCleanupDirectory(s.recoveryDir)
	}
	remaining := []pendingOperation(nil)
	journalBytes := int64(0)
	if prepared != nil {
		newGeneration.attachJournal(prepared.journal)
		newTree.SetSource(store.SourceJournal, prepared.journal)
		prepared.journal = nil // ownership transferred to newGeneration
		remaining = prepared.pending
		journalBytes = prepared.bytes
		for _, operation := range remaining {
			piece := store.Piece{}
			if operation.insertLength > 0 {
				piece = store.Piece{
					Source: store.SourceJournal, Offset: operation.insertOffset, Length: operation.insertLength,
					Newlines: operation.insertNewlines, NewlinesKnown: true,
				}
			}
			if _, _, replaceErr := newTree.ReplacePiece(operation.start, operation.deleteLength, piece); replaceErr != nil {
				_ = newGeneration.retireAndWait(false)
				return commitFailure(replaceErr)
			}
		}
	}
	oldGeneration := s.generation
	s.base, s.generation, s.tree = newBase, newGeneration, newTree
	s.journal = newGeneration.journal
	s.pending = remaining
	s.journalBytes = journalBytes
	s.committedRevision = targetRevision
	s.fingerprint = newFingerprint
	s.diskIdentity = identityFor(info, newContentHash)
	s.durabilityUncertain = durabilityErr != nil
	s.dirty = s.revision > s.committedRevision
	s.resetJournalCheckpointLocked()
	if !s.dirty {
		s.recovered = false
	}
	s.recordJournalSyncResultLocked(s.journal, nil)
	s.maybeScheduleJournalCheckpointLocked()
	oldGeneration.retire(true)
	metadata := s.metadataLocked()
	persistence.CompletedBytes, persistence.Committed = persistence.TotalBytes, true
	s.publishPersistenceEvent(EventSaved, metadata, persistence, atomicErr)
	persistenceFinished = true
	return metadata, atomicErr
}

func (s *Session) Close() error {
	// Save may install a replacement source generation after its immutable
	// snapshot has been streamed. Stop background checkpoint scheduling before
	// waiting for saveMu so the checkpoint worker cannot deadlock behind Close.
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.RLock()
		err := s.closeErr
		s.mu.RUnlock()
		return err
	}
	s.closed = true
	close(s.stopSync)
	s.mu.Unlock()
	<-s.syncDone
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	s.automaticCheckpointPending = false
	dirty, generation := s.dirty, s.generation
	var syncErr error
	if s.journal != nil {
		syncErr = s.operations.syncRecovery(s.journal)
		s.recordJournalSyncResultLocked(s.journal, syncErr)
	}
	s.mu.Unlock()
	err := errors.Join(syncErr, generation.retireAndWait(!dirty))
	err = errors.Join(err, s.undoStore.close())
	err = errors.Join(err, s.sessionMarker.close())
	err = errors.Join(err, cleanupOwnedDirectories(s.config, !dirty))
	s.mu.Lock()
	s.publishEventLocked(EventClosed, ChangeOriginNone, coordinate.ChangeMap{}, err)
	s.mu.Unlock()
	s.events.close()
	s.mu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

func (s *Session) syncLoop() {
	defer close(s.syncDone)
	ticker := time.NewTicker(s.config.JournalSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.RLock()
			journal := s.journal
			s.mu.RUnlock()
			if journal != nil {
				syncErr := s.operations.syncRecovery(journal)
				s.mu.Lock()
				s.recordJournalSyncResultLocked(journal, syncErr)
				s.mu.Unlock()
			}
		case target := <-s.checkpointRequest:
			metadata, _ := s.CommitAtLeast(target)
			s.mu.Lock()
			s.automaticCheckpointPending = false
			if metadata.CommittedRevision >= target {
				s.automaticCheckpoints++
			}
			s.maybeScheduleJournalCheckpointLocked()
			s.mu.Unlock()
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

func validateUTF8ReplacementBoundaries(tree *store.Tree, start, deleteLength int64) error {
	length := tree.Len()
	end := start + deleteLength
	for index, offset := range [...]int64{start, end} {
		if index == 1 && offset == start {
			continue
		}
		if offset == 0 || offset == length {
			continue
		}
		value, err := readTreeRange(tree, offset, 1)
		if err != nil {
			return err
		}
		if !utf8.RuneStart(value[0]) {
			return ErrInvalidUTF8Boundary
		}
	}
	return nil
}

type baseScan struct {
	size        int64
	contentHash [32]byte
	hasBOM      bool
	baseOffset  int64
	newlines    int64
	eol         EOLStyle
}

type readStatFile interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
}

type utf8StreamValidator struct {
	pending []byte
}

func (v *utf8StreamValidator) Write(value []byte) bool {
	data := append(append([]byte(nil), v.pending...), value...)
	v.pending = v.pending[:0]
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			v.pending = append(v.pending, data...)
			return true
		}
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			return false
		}
		data = data[size:]
	}
	return true
}

func (v *utf8StreamValidator) Complete() bool { return len(v.pending) == 0 }

type fileChangeCapture func(readStatFile, os.FileInfo) (fileChangeStamp, error)

func scanOpenedBase(ctx context.Context, file readStatFile, path string, initial os.FileInfo, stat func(string) (os.FileInfo, error)) (baseScan, error) {
	return scanOpenedBaseWithChange(ctx, file, path, initial, stat, captureFileChange)
}

func scanOpenedBaseWithChange(ctx context.Context, file readStatFile, path string, initial os.FileInfo, stat func(string) (os.FileInfo, error), capture fileChangeCapture) (baseScan, error) {
	if initial.Size() < 0 {
		return baseScan{}, ErrExternalChange
	}
	initialChange, err := capture(file, initial)
	if err != nil {
		return baseScan{}, err
	}
	hasher := sha256.New()
	validator := utf8StreamValidator{}
	probe := make([]byte, 0, 3)
	bomDecided := false
	hasBOM := false
	var crlf, lf, newlines int64
	previousCR := false
	feedText := func(value []byte) error {
		if !validator.Write(value) {
			return ErrInvalidUTF8
		}
		for _, current := range value {
			if current == '\n' {
				newlines++
				if previousCR {
					crlf++
				} else {
					lf++
				}
			}
			previousCR = current == '\r'
		}
		return nil
	}
	consume := func(value []byte) error {
		_, _ = hasher.Write(value)
		if !bomDecided {
			needed := 3 - len(probe)
			take := min(needed, len(value))
			probe = append(probe, value[:take]...)
			value = value[take:]
			if len(probe) == 3 {
				bomDecided = true
				if bytes.Equal(probe, []byte{0xef, 0xbb, 0xbf}) {
					hasBOM = true
					probe = probe[:0]
				} else if err := feedText(probe); err != nil {
					return err
				}
			}
		}
		return feedText(value)
	}
	buffer := make([]byte, scanBufferSize)
	for offset := int64(0); offset < initial.Size(); {
		if err := ctx.Err(); err != nil {
			return baseScan{}, err
		}
		want := min(int64(len(buffer)), initial.Size()-offset)
		n, readErr := file.ReadAt(buffer[:int(want)], offset)
		if n > 0 {
			if err := consume(buffer[:n]); err != nil {
				return baseScan{}, err
			}
			offset += int64(n)
		}
		if readErr != nil && !(errors.Is(readErr, io.EOF) && offset == initial.Size()) {
			return baseScan{}, readErr
		}
		if n == 0 && offset < initial.Size() {
			return baseScan{}, io.ErrUnexpectedEOF
		}
	}
	if !bomDecided {
		if err := feedText(probe); err != nil {
			return baseScan{}, err
		}
	}
	if !validator.Complete() {
		return baseScan{}, ErrInvalidUTF8
	}
	if err := ctx.Err(); err != nil {
		return baseScan{}, err
	}
	pathInfo, err := stat(path)
	if err != nil {
		return baseScan{}, err
	}
	final, err := file.Stat()
	if err != nil {
		return baseScan{}, err
	}
	finalChange, err := capture(file, final)
	if err != nil {
		return baseScan{}, err
	}
	if !sameFileVersion(initial, final) || !sameFileVersion(final, pathInfo) || !sameFileChange(initialChange, finalChange) {
		return baseScan{}, ErrExternalChange
	}
	style := EOLLF
	if crlf > 0 && lf > 0 {
		style = EOLMixed
	} else if crlf > 0 {
		style = EOLCRLF
	}
	var hash [32]byte
	copy(hash[:], hasher.Sum(nil))
	return baseScan{size: initial.Size(), contentHash: hash, hasBOM: hasBOM, baseOffset: boolOffset(hasBOM, 3), newlines: newlines, eol: style}, nil
}

func scanDiskIdentity(ctx context.Context, path string, operations sessionOperations) (fileIdentity, error) {
	file, err := operations.openBase(path)
	if err != nil {
		return fileIdentity{}, err
	}
	defer file.Close()
	return scanDiskIdentityOpened(ctx, path, file, operations.stat)
}

func scanDiskIdentityOpened(ctx context.Context, path string, file readStatFile, stat func(string) (os.FileInfo, error)) (fileIdentity, error) {
	return scanDiskIdentityOpenedWithChange(ctx, path, file, stat, captureFileChange)
}

func scanDiskIdentityOpenedWithChange(ctx context.Context, path string, file readStatFile, stat func(string) (os.FileInfo, error), capture fileChangeCapture) (fileIdentity, error) {
	initial, err := file.Stat()
	if err != nil {
		return fileIdentity{}, err
	}
	if !initial.Mode().IsRegular() {
		return fileIdentity{}, ErrExternalChange
	}
	initialChange, err := capture(file, initial)
	if err != nil {
		return fileIdentity{}, err
	}
	hasher := sha256.New()
	buffer := make([]byte, scanBufferSize)
	for offset := int64(0); offset < initial.Size(); {
		if err := ctx.Err(); err != nil {
			return fileIdentity{}, err
		}
		want := min(int64(len(buffer)), initial.Size()-offset)
		n, readErr := file.ReadAt(buffer[:int(want)], offset)
		if n > 0 {
			_, _ = hasher.Write(buffer[:n])
			offset += int64(n)
		}
		if readErr != nil && !(errors.Is(readErr, io.EOF) && offset == initial.Size()) {
			return fileIdentity{}, readErr
		}
		if n == 0 && offset < initial.Size() {
			return fileIdentity{}, io.ErrUnexpectedEOF
		}
	}
	pathInfo, err := stat(path)
	if err != nil {
		return fileIdentity{}, err
	}
	final, err := file.Stat()
	if err != nil {
		return fileIdentity{}, err
	}
	finalChange, err := capture(file, final)
	if err != nil {
		return fileIdentity{}, err
	}
	if !sameFileVersion(initial, final) || !sameFileVersion(final, pathInfo) || !sameFileChange(initialChange, finalChange) {
		return fileIdentity{}, ErrExternalChange
	}
	var hash [32]byte
	copy(hash[:], hasher.Sum(nil))
	return identityFor(final, hash), nil
}

func sameFileVersion(left, right os.FileInfo) bool {
	return left.Size() == right.Size() && left.ModTime().UnixNano() == right.ModTime().UnixNano() && os.SameFile(left, right)
}

type fileChangeStamp struct {
	first, second int64
	available     bool
}

func sameFileChange(left, right fileChangeStamp) bool {
	if left.available != right.available {
		return false
	}
	return !left.available || left.first == right.first && left.second == right.second
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

func identityFor(info os.FileInfo, contentHash [32]byte) fileIdentity {
	return fileIdentity{size: info.Size(), modTime: info.ModTime().UnixNano(), contentHash: contentHash}
}

func boolOffset(condition bool, value int64) int64 {
	if condition {
		return value
	}
	return 0
}
