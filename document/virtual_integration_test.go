package document

import (
	"context"
	"errors"
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
