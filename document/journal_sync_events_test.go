package document

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moresleep512/docengine/recovery"
)

func TestJournalSyncEventsPublishOnlyStateTransitions(t *testing.T) {
	session := openJournalSyncEventSession(t, time.Hour)
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
	t.Cleanup(func() { _ = subscription.Close() })
	sentinel := errors.New("sync")
	session.mu.Lock()
	session.recordJournalSyncResultLocked(nil, sentinel) // Stale result is ignored.
	session.recordJournalSyncResultLocked(session.journal, sentinel)
	session.recordJournalSyncResultLocked(session.journal, sentinel)
	session.mu.Unlock()
	failed := receiveEvent(t, subscription.Events())
	if failed.Kind != EventJournalSyncFailed || !errors.Is(failed.Cause, sentinel) || !failed.Metadata.RecoveryDurabilityUncertain {
		t.Fatalf("failed event = %+v", failed)
	}
	select {
	case event := <-subscription.Events():
		t.Fatalf("repeated failure published %+v", event)
	default:
	}
	session.mu.Lock()
	session.recordJournalSyncResultLocked(session.journal, nil)
	session.recordJournalSyncResultLocked(session.journal, nil)
	session.mu.Unlock()
	restored := receiveEvent(t, subscription.Events())
	if restored.Kind != EventJournalSyncRestored || restored.Cause != nil || restored.Metadata.RecoveryDurabilityUncertain {
		t.Fatalf("restored event = %+v", restored)
	}
	select {
	case event := <-subscription.Events():
		t.Fatalf("repeated success published %+v", event)
	default:
	}
	if err := errors.Join(subscription.Close(), session.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestSyncLoopPublishesFailureAndRestoration(t *testing.T) {
	session := openJournalSyncEventSession(t, 5*time.Millisecond)
	sentinel := errors.New("background sync")
	var calls atomic.Int32
	session.operations.syncRecovery = func(journal *recovery.Journal) error {
		if calls.Add(1) == 1 {
			return sentinel
		}
		return journal.Sync()
	}
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	if changed := receiveEvent(t, subscription.Events()); changed.Kind != EventChanged {
		t.Fatalf("changed event = %+v", changed)
	}
	failed := receiveEvent(t, subscription.Events())
	restored := receiveEvent(t, subscription.Events())
	if failed.Kind != EventJournalSyncFailed || !errors.Is(failed.Cause, sentinel) || !failed.Metadata.RecoveryDurabilityUncertain ||
		restored.Kind != EventJournalSyncRestored || restored.Metadata.RecoveryDurabilityUncertain {
		t.Fatalf("sync-loop events = (%+v, %+v)", failed, restored)
	}
	if calls.Load() < 2 {
		t.Fatalf("sync calls = %d", calls.Load())
	}
	if err := errors.Join(subscription.Close(), session.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestCloseReturnsFinalJournalSyncFailure(t *testing.T) {
	session := openJournalSyncEventSession(t, time.Hour)
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 4, FutureOnly: true})
	t.Cleanup(func() { _ = subscription.Close() })
	sentinel := errors.New("final sync")
	session.operations.syncRecovery = func(*recovery.Journal) error { return sentinel }
	if err := session.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("Close = %v", err)
	}
	failed := receiveEvent(t, subscription.Events())
	closed := receiveEvent(t, subscription.Events())
	if failed.Kind != EventJournalSyncFailed || !errors.Is(failed.Cause, sentinel) || !failed.Metadata.RecoveryDurabilityUncertain ||
		closed.Kind != EventClosed || !errors.Is(closed.Cause, sentinel) || !closed.Metadata.RecoveryDurabilityUncertain {
		t.Fatalf("close sync events = (%+v, %+v)", failed, closed)
	}
	if _, ok := <-subscription.Events(); ok {
		t.Fatal("subscription remains open")
	}
}

func TestSaveRestoresFailedJournalDurabilityState(t *testing.T) {
	// This test attributes the recovery transition specifically to Save. Keep
	// the independent background sync loop out of its event ordering.
	session := openJournalSyncEventSession(t, time.Hour)
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
	t.Cleanup(func() { _ = subscription.Close() })
	sentinel := errors.New("prior sync")
	session.mu.Lock()
	session.recordJournalSyncResultLocked(session.journal, sentinel)
	session.mu.Unlock()
	if _, err := session.Save(); err != nil {
		t.Fatal(err)
	}
	events := drainEvents(subscription.Events(), 5, t)
	want := []EventKind{EventJournalSyncFailed, EventSaveStarted, EventSaveProgress, EventJournalSyncRestored, EventSaved}
	for index := range want {
		if events[index].Kind != want[index] {
			t.Fatalf("event %d = %+v, want %v", index, events[index], want[index])
		}
	}
	session.mu.RLock()
	syncErr := session.journalSyncErr
	session.mu.RUnlock()
	if syncErr != nil {
		t.Fatalf("sync error remains: %v", syncErr)
	}
	if err := errors.Join(subscription.Close(), session.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundJournalSyncMayRestoreWhileSaveIsInFlight(t *testing.T) {
	session := openJournalSyncEventSession(t, time.Hour)
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = subscription.Close() })

	session.mu.RLock()
	oldJournal := session.journal
	session.mu.RUnlock()
	saveAtWrite := make(chan struct{})
	allowWrite := make(chan struct{})
	var saveAtWriteOnce, allowWriteOnce sync.Once
	releaseWrite := func() { allowWriteOnce.Do(func() { close(allowWrite) }) }
	defer releaseWrite()

	originalAtomic := session.operations.atomicChecked
	session.operations.atomicChecked = func(
		path string,
		mode os.FileMode,
		prefix []byte,
		writeContent func(io.Writer) (int64, error),
		beforeReplace func() error,
	) (int64, error) {
		return originalAtomic(path, mode, prefix, func(writer io.Writer) (int64, error) {
			saveAtWriteOnce.Do(func() { close(saveAtWrite) })
			<-allowWrite
			return writeContent(writer)
		}, beforeReplace)
	}

	sentinel := errors.New("prior sync")
	session.mu.Lock()
	session.recordJournalSyncResultLocked(oldJournal, sentinel)
	session.mu.Unlock()
	failed := receiveEvent(t, subscription.Events())
	if failed.Kind != EventJournalSyncFailed ||
		!failed.Metadata.RecoveryDurabilityUncertain ||
		!errors.Is(failed.Cause, sentinel) {
		t.Fatalf("failed event = %+v", failed)
	}

	saveDone := make(chan error, 1)
	go func() {
		_, saveErr := session.Save()
		saveDone <- saveErr
	}()
	waitForJournalSignal(t, saveAtWrite, "Save content write")
	started := receiveEvent(t, subscription.Events())
	if started.Kind != EventSaveStarted || started.Persistence.OperationID == 0 {
		t.Fatalf("started event = %+v", started)
	}

	// Model a syncLoop iteration that captured oldJournal before Save replaced
	// it and completes while Save is still writing.
	if err := session.operations.syncRecovery(oldJournal); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.recordJournalSyncResultLocked(oldJournal, nil)
	session.mu.Unlock()
	restored := receiveEvent(t, subscription.Events())
	if restored.Kind != EventJournalSyncRestored ||
		restored.Metadata.CommittedRevision != 0 ||
		restored.Metadata.RecoveryDurabilityUncertain {
		t.Fatalf("background restored event = %+v", restored)
	}

	releaseWrite()
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	progress := receiveEvent(t, subscription.Events())
	saved := receiveEvent(t, subscription.Events())
	if progress.Kind != EventSaveProgress ||
		progress.Persistence.OperationID != started.Persistence.OperationID ||
		saved.Kind != EventSaved || saved.Metadata.CommittedRevision != 1 ||
		saved.Metadata.RecoveryDurabilityUncertain ||
		saved.Persistence.OperationID != started.Persistence.OperationID {
		t.Fatalf("post-restore save events = (%+v, %+v)", progress, saved)
	}
	select {
	case event := <-subscription.Events():
		t.Fatalf("duplicate restoration or save event = %+v", event)
	default:
	}
	if err := errors.Join(subscription.Close(), session.Close()); err != nil {
		t.Fatal(err)
	}
}

func openJournalSyncEventSession(t testing.TB, interval time.Duration) *Session {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir:         filepath.Join(dir, "recovery"),
		SessionDir:          filepath.Join(dir, "session"),
		JournalSyncInterval: interval,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func waitForJournalSignal(t testing.TB, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
