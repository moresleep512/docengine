package document

import (
	"errors"
	"io"
	"os"
	"sync"

	"github.com/moresleep512/docengine/document/store"
	"github.com/moresleep512/docengine/recovery"
)

// SnapshotLease keeps every source used by a snapshot alive until Close.
// Callers must release the lease when they finish reading or saving a snapshot.
type SnapshotLease interface {
	io.ReaderAt
	Len() int64
	WriteTo(io.Writer) (int64, error)
	Close() error
}

type sourceGeneration struct {
	mu                sync.Mutex
	cond              *sync.Cond
	base              *os.File
	journal           *recovery.Journal
	journalPath       string
	journalCleanupDir string
	refs              int
	retired           bool
	removeJournal     bool
	closeErr          error
}

func (g *sourceGeneration) setJournalCleanupDirectory(path string) {
	g.mu.Lock()
	g.journalCleanupDir = path
	g.mu.Unlock()
}

func newSourceGeneration(base *os.File, journal *recovery.Journal) *sourceGeneration {
	g := &sourceGeneration{base: base, journal: journal, refs: 1}
	if journal != nil {
		g.journalPath = journal.Path()
	}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *sourceGeneration) attachJournal(journal *recovery.Journal) {
	g.mu.Lock()
	g.journal = journal
	g.journalPath = journal.Path()
	g.mu.Unlock()
}

func (g *sourceGeneration) acquire(snapshot store.Snapshot) SnapshotLease {
	g.mu.Lock()
	g.refs++
	g.mu.Unlock()
	return &snapshotLease{snapshot: snapshot, generation: g}
}

func (g *sourceGeneration) retire(removeJournal bool) {
	g.mu.Lock()
	g.removeJournal = g.removeJournal || removeJournal
	if !g.retired {
		g.retired = true
		g.refs-- // release the session owner's reference
	}
	g.closeIfUnusedLocked()
	g.mu.Unlock()
}

func (g *sourceGeneration) retireAndWait(removeJournal bool) error {
	g.mu.Lock()
	g.removeJournal = g.removeJournal || removeJournal
	if !g.retired {
		g.retired = true
		g.refs--
	}
	g.closeIfUnusedLocked()
	for g.refs > 0 {
		g.cond.Wait()
	}
	err := g.closeErr
	g.mu.Unlock()
	return err
}

func (g *sourceGeneration) release() error {
	g.mu.Lock()
	if g.refs > 0 {
		g.refs--
	}
	g.closeIfUnusedLocked()
	err := g.closeErr
	g.mu.Unlock()
	return err
}

func (g *sourceGeneration) closeIfUnusedLocked() {
	if !g.retired || g.refs != 0 {
		return
	}
	if g.journal != nil {
		g.closeErr = errors.Join(g.closeErr, g.journal.Close())
		g.journal = nil
	}
	if g.base != nil {
		g.closeErr = errors.Join(g.closeErr, g.base.Close())
		g.base = nil
	}
	if g.removeJournal && g.journalPath != "" {
		if err := os.Remove(g.journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.closeErr = errors.Join(g.closeErr, err)
		} else {
			g.journalPath = ""
		}
	}
	if g.removeJournal && g.journalCleanupDir != "" {
		if err := removeEmptyDirectory(g.journalCleanupDir); err != nil {
			g.closeErr = errors.Join(g.closeErr, err)
		} else {
			g.journalCleanupDir = ""
		}
	}
	g.cond.Broadcast()
}

type snapshotLease struct {
	mu         sync.Mutex
	snapshot   store.Snapshot
	generation *sourceGeneration
	closed     bool
}

func (l *snapshotLease) Len() int64 { return l.snapshot.Len() }

func (l *snapshotLease) ReadAt(p []byte, off int64) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}
	return l.snapshot.ReadAt(p, off)
}

func (l *snapshotLease) WriteTo(w io.Writer) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}
	return l.snapshot.WriteTo(w)
}

func (l *snapshotLease) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	generation := l.generation
	l.mu.Unlock()
	return generation.release()
}

// trackedSnapshotLease accounts for every lease handed to a host-facing
// Session API. The underlying generation owns the actual source handles; the
// Session owns the cross-generation resource budget.
type trackedSnapshotLease struct {
	lease   SnapshotLease
	session *Session
	once    sync.Once
	err     error
}

func (l *trackedSnapshotLease) Len() int64 { return l.lease.Len() }

func (l *trackedSnapshotLease) ReadAt(p []byte, off int64) (int, error) {
	return l.lease.ReadAt(p, off)
}

func (l *trackedSnapshotLease) WriteTo(w io.Writer) (int64, error) {
	return l.lease.WriteTo(w)
}

func (l *trackedSnapshotLease) Close() error {
	l.once.Do(func() {
		l.session.releaseSnapshotLease()
		l.err = l.lease.Close()
	})
	return l.err
}
