package document

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	docsave "github.com/moresleep512/docengine/document/save"
	"github.com/moresleep512/docengine/recovery"
)

func TestJournalHardLimitRejectsBatchAtomically(t *testing.T) {
	batchBytes, err := recovery.BatchEncodedSize(1, 1, []recovery.ReplaceOperation{{Inserted: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(96) + batchBytes
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir: filepath.Join(dir, "recovery"),
		SessionDir:  filepath.Join(dir, "session"),
		Limits:      SessionLimits{MaxJournalBytes: limit},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	before := session.RecoveryStats()
	if before.JournalBytes != limit || before.MaxJournalBytes != limit || before.AutoCheckpointBytes != 0 {
		t.Fatalf("stats after first batch = %+v", before)
	}
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "y"}}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("over-limit ApplyBatch = %v", err)
	}
	after := session.RecoveryStats()
	if after != before || session.Metadata().Revision != 1 || compactSessionContent(t, session) != "ax" {
		t.Fatalf("failed batch changed state: stats=%+v metadata=%+v content=%q", after, session.Metadata(), compactSessionContent(t, session))
	}
	if metadata, err := session.Save(); err != nil || metadata.Dirty {
		t.Fatalf("Save = (%+v, %v)", metadata, err)
	}
	if stats := session.RecoveryStats(); stats.JournalBytes != 96 {
		t.Fatalf("post-save stats = %+v", stats)
	}
}

func TestAutomaticJournalCheckpointRebasesConcurrentEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir:                filepath.Join(dir, "recovery"),
		SessionDir:                 filepath.Join(dir, "session"),
		JournalSyncInterval:        time.Hour,
		AutoCheckpointJournalBytes: MinimumJournalBytes,
		Limits:                     SessionLimits{MaxJournalBytes: 4 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var inserted atomic.Bool
	session.commitHook = func(stage string) {
		if stage == "snapshot" && inserted.CompareAndSwap(false, true) {
			if _, applyErr := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "2"}}); applyErr != nil {
				panic(applyErr)
			}
		}
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "1"}}); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 10*time.Second, func() bool {
		metadata := session.Metadata()
		stats := session.RecoveryStats()
		return metadata.Revision == 2 && metadata.CommittedRevision == 2 && !metadata.Dirty &&
			stats.AutomaticCheckpoints == 2 && !stats.AutomaticCheckpointQueued
	})
	session.commitHook = nil
	if body, err := os.ReadFile(path); err != nil || string(body) != "a12" {
		t.Fatalf("disk = %q, %v", body, err)
	}
	stats := session.RecoveryStats()
	if stats.JournalBytes != 96 || stats.MaxJournalBytes != 4<<20 ||
		stats.AutoCheckpointBytes != MinimumJournalBytes || stats.NextAutoCheckpointBytes != MinimumJournalBytes {
		t.Fatalf("final stats = %+v", stats)
	}
}

func TestAutomaticJournalCheckpointFailureBacksOff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir:                filepath.Join(dir, "recovery"),
		SessionDir:                 filepath.Join(dir, "session"),
		JournalSyncInterval:        time.Hour,
		AutoCheckpointJournalBytes: MinimumJournalBytes,
		Limits:                     SessionLimits{MaxJournalBytes: 4 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 32, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()
	sentinel := errors.New("automatic checkpoint stat")
	session.operations.stat = func(string) (os.FileInfo, error) { return nil, sentinel }
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "1"}}); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, subscription.Events(), EventSaveFailed, sentinel)
	waitForCondition(t, 10*time.Second, func() bool {
		return !session.RecoveryStats().AutomaticCheckpointQueued
	})
	failed := session.RecoveryStats()
	if failed.AutomaticCheckpoints != 0 || failed.NextAutoCheckpointBytes != failed.JournalBytes+MinimumJournalBytes ||
		failed.AutomaticCheckpointQueued {
		t.Fatalf("failed checkpoint stats = %+v", failed)
	}
	session.operations.stat = os.Stat
	for revision, insert := range []string{"2", "3"} {
		if _, err := session.ApplyBatch(context.Background(), uint64(revision+1), []ReplaceOperation{{
			Start: int64(revision + 2), Insert: insert,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if metadata, stats := session.Metadata(), session.RecoveryStats(); metadata.CommittedRevision != 0 || stats.AutomaticCheckpointQueued {
		t.Fatalf("checkpoint retried before backoff threshold: metadata=%+v stats=%+v", metadata, stats)
	}
	if _, err := session.ApplyBatch(context.Background(), 3, []ReplaceOperation{{Start: 4, Insert: "4"}}); err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, 10*time.Second, func() bool {
		return session.Metadata().CommittedRevision == 4 && session.RecoveryStats().AutomaticCheckpoints == 1
	})
}

func TestCloseWaitsForActiveAutomaticCheckpointWithoutDeadlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		RecoveryDir:                filepath.Join(dir, "recovery"),
		SessionDir:                 filepath.Join(dir, "session"),
		JournalSyncInterval:        time.Hour,
		AutoCheckpointJournalBytes: MinimumJournalBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	captured, proceed := make(chan struct{}), make(chan struct{})
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			close(captured)
			<-proceed
		}
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "x"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-captured:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic checkpoint did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before active checkpoint was released: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(proceed)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked behind automatic checkpoint")
	}
	if stats := session.RecoveryStats(); stats.AutomaticCheckpointQueued {
		t.Fatalf("closed Session retains checkpoint work: %+v", stats)
	}
}

func TestPreparedJournalIsSyncedBeforeAtomicReplacement(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "a")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "1"}}); err != nil {
		t.Fatal(err)
	}
	oldJournal := session.journal
	var preparedSynced, preparedDirectorySynced atomic.Bool
	session.operations.syncRecovery = func(journal *recovery.Journal) error {
		if journal != oldJournal {
			preparedSynced.Store(true)
		}
		return journal.Sync()
	}
	session.operations.syncParent = func(path string) error {
		preparedDirectorySynced.Store(true)
		return docsave.SyncParent(path)
	}
	session.operations.atomicChecked = func(path string, mode os.FileMode, prefix []byte, write func(io.Writer) (int64, error), check func() error) (int64, error) {
		return docsave.AtomicChecked(path, mode, prefix, write, func() error {
			if err := check(); err != nil {
				return err
			}
			if !preparedSynced.Load() || !preparedDirectorySynced.Load() {
				return errors.New("replacement reached before prepared journal durability")
			}
			return nil
		})
	}
	captured, proceed := make(chan struct{}), make(chan struct{})
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			close(captured)
			<-proceed
		}
	}
	saved := make(chan error, 1)
	go func() {
		_, saveErr := session.Save()
		saved <- saveErr
	}()
	<-captured
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "2"}}); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	session.commitHook = nil
	if !preparedSynced.Load() || !preparedDirectorySynced.Load() {
		t.Fatal("prepared journal file and directory were not synced")
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "a1" {
		t.Fatalf("disk = %q, %v", body, err)
	}
	if metadata := session.Metadata(); metadata.Revision != 2 || metadata.CommittedRevision != 1 || !metadata.Dirty {
		t.Fatalf("metadata = %+v", metadata)
	}
	if compactSessionContent(t, session) != "a12" || session.RecoveryStats().JournalBytes < MinimumJournalBytes {
		t.Fatalf("rebased state = %q, %+v", compactSessionContent(t, session), session.RecoveryStats())
	}
}

func TestPreparedJournalSyncFailurePreventsReplacement(t *testing.T) {
	session, path, recoveryDir := openAtomicTestSession(t, "a")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "1"}}); err != nil {
		t.Fatal(err)
	}
	oldJournal := session.journal
	sentinel := errors.New("prepared journal sync")
	session.operations.syncRecovery = func(journal *recovery.Journal) error {
		if journal != oldJournal {
			return sentinel
		}
		return journal.Sync()
	}
	captured, proceed := make(chan struct{}), make(chan struct{})
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			close(captured)
			<-proceed
		}
	}
	saved := make(chan error, 1)
	go func() {
		_, saveErr := session.Save()
		saved <- saveErr
	}()
	<-captured
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "2"}}); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-saved; !errors.Is(err, sentinel) {
		t.Fatalf("Save = %v", err)
	}
	session.commitHook = nil
	session.operations.syncRecovery = func(journal *recovery.Journal) error { return journal.Sync() }
	if body, err := os.ReadFile(path); err != nil || string(body) != "a" {
		t.Fatalf("disk changed before journal sync: %q, %v", body, err)
	}
	if session.Fault() != nil || session.Metadata().CommittedRevision != 0 || compactSessionContent(t, session) != "a12" {
		t.Fatalf("state after pre-commit failure: metadata=%+v fault=%v content=%q", session.Metadata(), session.Fault(), compactSessionContent(t, session))
	}
	journals, err := filepath.Glob(filepath.Join(recoveryDir, "*.docengine-journal-v2"))
	if err != nil || len(journals) != 1 || journals[0] != oldJournal.Path() {
		t.Fatalf("prepared journal cleanup = %v, %v", journals, err)
	}
}

func TestPreparedJournalDirectorySyncFailurePreventsReplacement(t *testing.T) {
	sentinel := errors.New("prepared journal directory sync")
	tests := []struct {
		name string
		err  error
	}{
		{name: "plain", err: sentinel},
		{name: "durability wrapper", err: &docsave.DurabilityError{Path: "journal", Err: sentinel}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, path, _ := openAtomicTestSession(t, "a")
			defer session.Close()
			if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 1, Insert: "1"}}); err != nil {
				t.Fatal(err)
			}
			captured, proceed := make(chan struct{}), make(chan struct{})
			session.commitHook = func(stage string) {
				if stage == "snapshot" {
					close(captured)
					<-proceed
				}
			}
			saved := make(chan error, 1)
			go func() {
				_, saveErr := session.Save()
				saved <- saveErr
			}()
			<-captured
			if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 2, Insert: "2"}}); err != nil {
				t.Fatal(err)
			}
			session.operations.syncParent = func(string) error { return test.err }
			close(proceed)
			saveErr := <-saved
			if !errors.Is(saveErr, sentinel) {
				t.Fatalf("Save = %v", saveErr)
			}
			var durability *docsave.DurabilityError
			if errors.As(saveErr, &durability) {
				t.Fatalf("pre-replacement journal error reported document commit: %v", saveErr)
			}
			session.commitHook = nil
			session.operations.syncParent = docsave.SyncParent
			if body, err := os.ReadFile(path); err != nil || string(body) != "a" {
				t.Fatalf("disk changed before journal directory sync: %q, %v", body, err)
			}
			if session.Fault() != nil || session.Metadata().CommittedRevision != 0 || compactSessionContent(t, session) != "a12" {
				t.Fatalf("state after directory-sync failure: metadata=%+v fault=%v content=%q",
					session.Metadata(), session.Fault(), compactSessionContent(t, session))
			}
		})
	}
}

func waitForCondition(t testing.TB, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func waitForEvent(t testing.TB, events <-chan SessionEvent, kind EventKind, cause error) SessionEvent {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Kind == kind {
				if cause != nil && !errors.Is(event.Cause, cause) {
					t.Fatalf("event cause = %v, want %v", event.Cause, cause)
				}
				return event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for event %v", kind)
		}
	}
}
