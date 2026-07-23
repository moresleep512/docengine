package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/moresleep512/docengine/document/virtual"
)

func TestVirtualPagerPinsRevisionAndSnapshotLifetime(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "alpha\nbeta")
	pager, err := session.VirtualPager(context.Background(), virtual.Options{
		TargetPageBytes: 4, MaximumPageBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	stats := pager.Stats()
	if stats.Revision != 0 || stats.Generation != 0 || stats.ByteLength != 10 {
		t.Fatalf("unexpected initial stats: %+v", stats)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{
		Start: 0, DeleteLength: 5, Insert: "new",
	}}); err != nil {
		t.Fatal(err)
	}
	window, err := pager.WindowByByte(context.Background(), virtual.ByteWindowRequest{
		Revision: stats.Revision, Generation: stats.Generation, Offset: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	var content string
	for _, page := range window.Pages {
		content += string(page.Content)
	}
	if content != "alpha\n" {
		t.Fatalf("old pager read changed content %q", content)
	}
	current, err := session.VirtualPager(context.Background(), virtual.Options{
		TargetPageBytes: 4, MaximumPageBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if current.Stats().Revision != 1 {
		t.Fatalf("current revision = %d", current.Stats().Revision)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}

	closed := make(chan error, 1)
	go func() { closed <- session.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Session.Close returned before Pager released its lease: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestVirtualPagerContextAndClosedSession(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "")
	if _, err := session.VirtualPager(nil, virtual.Options{}); !errors.Is(err, virtual.ErrInvalidContext) {
		t.Fatalf("nil context error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.VirtualPager(ctx, virtual.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.VirtualPager(context.Background(), virtual.Options{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Session error = %v", err)
	}
}

func TestVirtualPagerBuildFailureReleasesSnapshotLease(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abcdefgh")
	defer session.Close()

	session.generation.mu.Lock()
	before := session.generation.refs
	session.generation.mu.Unlock()
	if _, err := session.VirtualPager(context.Background(), virtual.Options{
		TargetPageBytes: 3,
	}); !errors.Is(err, virtual.ErrInvalidOptions) {
		t.Fatalf("invalid options error = %v", err)
	}
	session.generation.mu.Lock()
	afterInvalid := session.generation.refs
	session.generation.mu.Unlock()
	if afterInvalid != before {
		t.Fatalf("invalid build leaked generation reference: before=%d after=%d", before, afterInvalid)
	}

	cancelDuringBuild := &countingCancelContext{cancelAt: 3}
	if _, err := session.VirtualPager(cancelDuringBuild, virtual.Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-build cancellation error = %v", err)
	}
	session.generation.mu.Lock()
	afterCancel := session.generation.refs
	session.generation.mu.Unlock()
	if afterCancel != before {
		t.Fatalf("cancelled build leaked generation reference: before=%d after=%d", before, afterCancel)
	}
}

func TestSaveAndCloseSerializeWithoutInvalidatingRetiredPager(t *testing.T) {
	session, path, _ := openAtomicTestSession(t, "base")
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{
		Start: 4, Insert: "!",
	}}); err != nil {
		t.Fatal(err)
	}
	pager, err := session.VirtualPager(context.Background(), virtual.Options{
		TargetPageBytes: 4, MaximumPageBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	journalPath := session.journal.Path()

	saveAtSnapshot := make(chan struct{})
	releaseSave := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSave) }) }
	t.Cleanup(release)
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			close(saveAtSnapshot)
			<-releaseSave
		}
	}
	saveDone := make(chan error, 1)
	go func() {
		_, saveErr := session.Save()
		saveDone <- saveErr
	}()
	select {
	case <-saveAtSnapshot:
	case <-time.After(5 * time.Second):
		t.Fatal("Save did not reach snapshot hook")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- session.Close() }()
	release()
	select {
	case err := <-saveDone:
		if err != nil {
			t.Fatalf("Save = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Save remained blocked")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a Pager from the retired generation")
	}

	stats := pager.Stats()
	window, err := pager.WindowByByte(context.Background(), virtual.ByteWindowRequest{
		Revision: stats.Revision, Generation: stats.Generation, Offset: 0,
	})
	if err != nil {
		t.Fatalf("retired Pager read after Session.Close: %v", err)
	}
	var content string
	for _, page := range window.Pages {
		content += string(page.Content)
	}
	if content != "base!" {
		t.Fatalf("retired Pager content = %q", content)
	}
	if disk, err := os.ReadFile(path); err != nil || string(disk) != "base!" {
		t.Fatalf("saved disk content = (%q, %v)", disk, err)
	}
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("retired generation journal removed before Pager.Close: %v", err)
	}
	if err := pager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired generation journal remains after Pager.Close: %v", err)
	}
}

func TestVirtualPagerRemainsPinnedAcrossHistoryAndCheckpoint(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{
		Start: 1, Insert: "X",
	}}); err != nil {
		t.Fatal(err)
	}
	pager, err := session.VirtualPager(context.Background(), virtual.Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{
		Start: 4, Insert: "Y",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Redo(); err != nil {
		t.Fatal(err)
	}
	if result, err := session.Compact(context.Background(), CompactOptions{
		CheckpointJournal: true,
	}); err != nil || !result.JournalCheckpointed {
		t.Fatalf("checkpoint Compact = (%+v, %v)", result, err)
	}

	assertPagerText(t, pager, "aXbc")
	assertSessionText(t, session, "aXbcY")
	if stats := pager.Stats(); stats.Revision != 1 {
		t.Fatalf("pinned Pager revision = %d", stats.Revision)
	}
}

func TestRecoveredSessionBuildsExactVirtualPager(t *testing.T) {
	session, path, recoveryDir := openAtomicTestSession(t, "alpha")
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{
		Start: 5, Insert: "\nbeta",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, OpenOptions{
		RecoveryDir: recoveryDir,
		SessionDir:  filepath.Join(filepath.Dir(path), "recovered-session"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if metadata := reopened.Metadata(); metadata.Revision != 1 || !metadata.Recovered {
		t.Fatalf("recovered metadata = %+v", metadata)
	}
	pager, err := reopened.VirtualPager(context.Background(), virtual.Options{
		TargetPageBytes: 4, MaximumPageBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	assertPagerText(t, pager, "alpha\nbeta")
}

func assertPagerText(t testing.TB, pager *virtual.Pager, want string) {
	t.Helper()
	stats := pager.Stats()
	window, err := pager.WindowByByte(context.Background(), virtual.ByteWindowRequest{
		Revision: stats.Revision, Generation: stats.Generation, Offset: 0,
		Budget: virtual.Budget{
			Bytes: max(int64(len(want)), stats.MaximumPageBytes),
			Pages: max(len(want)+1, 1), Fragments: max(len(want)+1, 1), Measure: 1,
		},
		After: int64(len(want)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, page := range window.Pages {
		got += string(page.Content)
	}
	if got != want {
		t.Fatalf("Pager text = %q, want %q", got, want)
	}
}
