package document

import (
	"context"
	"errors"

	"github.com/moresleep512/docengine/document/store"
)

// CompactOptions selects compaction that may perform persistence. Piece and
// undo compaction always run; CheckpointJournal additionally saves the current
// revision so the append-only recovery journal can be rebased safely.
type CompactOptions struct {
	CheckpointJournal bool
}

// CompactionResult describes structural reclamation without changing the
// document revision or content.
type CompactionResult struct {
	Metadata            Metadata
	Pieces              store.CompactResult
	UndoBytesBefore     int64
	UndoBytesAfter      int64
	JournalCheckpointed bool
}

// Compact coalesces adjacent Piece Tree fragments and rewrites the undo store
// to contain only live history references. Journal compaction is an explicit
// persistence checkpoint because rewriting an uncommitted WAL in place cannot
// preserve both revision identity and crash atomicity.
func (s *Session) Compact(ctx context.Context, options CompactOptions) (CompactionResult, error) {
	operationContext, finish, err := s.operationContext(ctx)
	if err != nil {
		return CompactionResult{}, err
	}
	defer finish()
	if err := contextError(operationContext); err != nil {
		return CompactionResult{}, err
	}
	var result CompactionResult
	if options.CheckpointJournal {
		s.mu.RLock()
		if s.closed {
			s.mu.RUnlock()
			return result, ErrClosed
		}
		if s.fault != nil {
			fault := s.fault
			s.mu.RUnlock()
			return result, errors.Join(ErrFaulted, fault)
		}
		target := s.revision
		s.mu.RUnlock()
		metadata, err := s.CommitAtLeastContext(operationContext, target)
		result.Metadata = metadata
		if err != nil {
			return result, err
		}
		result.JournalCheckpointed = metadata.CommittedRevision >= target
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return result, ErrClosed
	}
	if s.fault != nil {
		return result, errors.Join(ErrFaulted, s.fault)
	}
	if err := contextError(operationContext); err != nil {
		return result, err
	}
	result.Pieces = s.tree.Compact()
	result.UndoBytesBefore = s.undoStore.bytes()
	refs := collectHistoryRefs(s.undo, s.redo)
	mapping, err := s.undoStore.rewrite(refs)
	if mapping != nil {
		remapHistoryRefs(s.undo, mapping)
		remapHistoryRefs(s.redo, mapping)
	}
	result.UndoBytesAfter = s.undoStore.bytes()
	result.Metadata = s.metadataLocked()
	return result, err
}

func collectHistoryRefs(groups ...[]historyEntry) []textRef {
	var refs []textRef
	for _, entries := range groups {
		for _, entry := range entries {
			for _, operations := range [][]historyOperation{entry.forward, entry.inverse} {
				for _, operation := range operations {
					if operation.insert.length > 0 {
						refs = append(refs, operation.insert)
					}
				}
			}
		}
	}
	return refs
}

func remapHistoryRefs(entries []historyEntry, mapping map[textRef]textRef) {
	for entryIndex := range entries {
		for _, operations := range []*[]historyOperation{&entries[entryIndex].forward, &entries[entryIndex].inverse} {
			for operationIndex := range *operations {
				ref := (*operations)[operationIndex].insert
				if ref.length > 0 {
					(*operations)[operationIndex].insert = mapping[ref]
				}
			}
		}
	}
}
