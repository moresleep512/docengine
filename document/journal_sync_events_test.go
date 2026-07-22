package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moresleep512/docengine/recovery"
)

func TestJournalSyncEventsPublishOnlyStateTransitions(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
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
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session"),
		JournalSyncInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("background sync")
	var calls atomic.Int32
	session.operations.syncRecovery = func(journal *recovery.Journal) error {
		if calls.Add(1) == 1 {
			return sentinel
		}
		return journal.Sync()
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
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
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 4, FutureOnly: true})
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
	session, _, _ := openAtomicTestSession(t, "a")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	subscription, _ := session.Subscribe(SubscribeOptions{Buffer: 8, FutureOnly: true})
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
