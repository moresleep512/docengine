package document

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moresleep512/docengine/recovery"
)

func TestRecoveryGrowthFaultBoundaries(t *testing.T) {
	sentinel := errors.New("injected recovery growth fault")

	t.Run("restored journal size", func(t *testing.T) {
		session, path, recoveryDir := openAtomicTestSession(t, "a")
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		config := session.Config()
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		operations := systemSessionOperations
		operations.sizeRecovery = func(*recovery.Journal) (int64, error) { return 0, sentinel }
		opened, err := openSession(path, OpenOptions{
			RecoveryDir: recoveryDir,
			SessionDir:  filepath.Join(filepath.Dir(config.SessionDir), "size-failure"),
		}, operations)
		if opened != nil {
			_ = opened.Close()
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("Open = %v", err)
		}
		reopened, err := Open(path, OpenOptions{
			RecoveryDir: recoveryDir,
			SessionDir:  filepath.Join(filepath.Dir(config.SessionDir), "size-retry"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if compactSessionContent(t, reopened) != "ax" {
			t.Fatalf("retry content = %q", compactSessionContent(t, reopened))
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("new journal size cleanup", func(t *testing.T) {
		session, _, recoveryDir := openAtomicTestSession(t, "a")
		defer session.Close()
		session.operations.sizeRecovery = func(*recovery.Journal) (int64, error) { return 0, sentinel }
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); !errors.Is(err, sentinel) {
			t.Fatalf("ApplyBatch = %v", err)
		}
		if session.journal != nil {
			t.Fatal("failed new journal was attached")
		}
		journals, err := filepath.Glob(filepath.Join(recoveryDir, "*.docengine-journal-v2"))
		if err != nil || len(journals) != 0 {
			t.Fatalf("new journal cleanup = %v, %v", journals, err)
		}
	})

	t.Run("new journal unexpected existing size", func(t *testing.T) {
		session, _, _ := openAtomicTestSession(t, "a")
		defer session.Close()
		session.config.Limits.MaxJournalBytes = 1_000
		session.operations.sizeRecovery = func(*recovery.Journal) (int64, error) { return 1_000, nil }
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("ApplyBatch = %v", err)
		}
		if session.Metadata().Revision != 0 || session.journal == nil || session.RecoveryStats().JournalBytes != 1_000 {
			t.Fatalf("unexpected-size state: metadata=%+v stats=%+v journal=%v",
				session.Metadata(), session.RecoveryStats(), session.journal)
		}
	})

	t.Run("prepared append", func(t *testing.T) {
		session, proceed, saved := startConcurrentFaultSave(t)
		defer session.Close()
		oldJournal := session.journal
		session.operations.appendRecovery = func(journal *recovery.Journal, revision, group uint64, operations []recovery.ReplaceOperation) (recovery.BatchAppendResult, error) {
			if journal != oldJournal {
				return recovery.BatchAppendResult{}, sentinel
			}
			return journal.AppendBatch(revision, group, operations)
		}
		close(proceed)
		if err := <-saved; !errors.Is(err, sentinel) {
			t.Fatalf("Save = %v", err)
		}
		if session.Fault() != nil {
			t.Fatalf("pre-commit append failure faulted Session: %v", session.Fault())
		}
	})

	t.Run("prepared journal collision", func(t *testing.T) {
		session, proceed, saved := startConcurrentFaultSave(t)
		defer session.Close()
		session.operations.openRecovery = func(path string, fingerprint recovery.Fingerprint) (*recovery.Journal, recovery.ReplayResult, error) {
			journal, _, err := recovery.Open(path, fingerprint)
			if err != nil {
				return nil, recovery.ReplayResult{}, err
			}
			if _, err := journal.AppendBatch(2, 2, []recovery.ReplaceOperation{{}}); err != nil {
				_ = journal.Close()
				return nil, recovery.ReplayResult{}, err
			}
			replay, err := journal.Replay()
			return journal, replay, err
		}
		close(proceed)
		if err := <-saved; err == nil || !strings.Contains(err.Error(), "was not empty") {
			t.Fatalf("Save = %v", err)
		}
		if session.Fault() != nil {
			t.Fatalf("pre-commit collision faulted Session: %v", session.Fault())
		}
	})

	t.Run("prepared removal already absent", func(t *testing.T) {
		session, proceed, saved := startConcurrentFaultSave(t)
		defer session.Close()
		oldJournal := session.journal
		session.operations.syncRecovery = func(journal *recovery.Journal) error {
			if journal != oldJournal {
				return sentinel
			}
			return journal.Sync()
		}
		session.operations.removeRecovery = func(path string) error {
			_ = os.Remove(path)
			return os.ErrNotExist
		}
		close(proceed)
		if err := <-saved; !errors.Is(err, sentinel) {
			t.Fatalf("Save = %v", err)
		}
		session.operations.syncRecovery = func(journal *recovery.Journal) error { return journal.Sync() }
	})

	t.Run("atomic replacement contract", func(t *testing.T) {
		session, path, _ := openAtomicTestSession(t, "a")
		defer session.Close()
		if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
		session.operations.atomicChecked = func(string, os.FileMode, []byte, func(io.Writer) (int64, error), func() error) (int64, error) {
			return 0, nil
		}
		if _, err := session.Save(); err == nil || !strings.Contains(err.Error(), "skipped its final identity check") {
			t.Fatalf("Save = %v", err)
		}
		if session.Fault() != nil {
			t.Fatalf("contract failure faulted Session: %v", session.Fault())
		}
		if body, err := os.ReadFile(path); err != nil || string(body) != "a" {
			t.Fatalf("disk = %q, %v", body, err)
		}
	})
}

func TestOpenMatchingJournalRetiredCandidateQuarantineFailure(t *testing.T) {
	dir := t.TempDir()
	fingerprint := recovery.Fingerprint{PathHash: [32]byte{1}, ContentHash: [32]byte{2}}
	stale := fingerprint
	stale.ContentHash = [32]byte{3}
	for index, candidate := range []recovery.Fingerprint{fingerprint, stale} {
		path := filepath.Join(dir, journalPrefix(fingerprint)+"."+string(rune('a'+index))+".docengine-journal-v2")
		journal, _, err := recovery.Open(path, candidate)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := errors.New("retire stale journal")
	journal, _, err := openMatchingJournalWithQuarantine(dir, fingerprint, recovery.Open, func(path, reason string, cause error) error {
		if reason == "retired-base" {
			return sentinel
		}
		return quarantineRecovery(path, reason, cause)
	})
	if journal != nil {
		_ = journal.Close()
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("openMatchingJournalWithQuarantine = %v", err)
	}
}

func TestNextCheckpointThresholdSaturates(t *testing.T) {
	if got := nextCheckpointThreshold(100, 50); got != 150 {
		t.Fatalf("normal threshold = %d", got)
	}
	if got := nextCheckpointThreshold(math.MaxInt64-2, 3); got != math.MaxInt64 {
		t.Fatalf("saturated threshold = %d", got)
	}
}
