package coordinate

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestQueryWindowCacheHitsEvictsAndCloses(t *testing.T) {
	source := &testSource{body: []byte("abcdefghijklmnop")}
	index, err := Build(context.Background(), source, 7, Options{CheckpointBytes: 2, CacheBytes: 6})
	if err != nil {
		t.Fatal(err)
	}
	buildReads := source.readCallCount()

	if _, err := index.ByteToPosition(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	firstReads := source.readCallCount()
	if firstReads != buildReads+1 {
		t.Fatalf("first query reads = %d, build=%d", firstReads, buildReads)
	}
	if _, err := index.RuneToByte(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := source.readCallCount(); got != firstReads {
		t.Fatalf("cache hit performed source read: %d -> %d", firstReads, got)
	}
	firstStats := index.Stats()
	if firstStats.CacheHits != 1 || firstStats.CacheMisses != 1 ||
		firstStats.CacheEntries != 1 || firstStats.CacheBytes != 6 ||
		firstStats.MaximumCacheBytes != 6 {
		t.Fatalf("first cache stats = %+v", firstStats)
	}
	index.storeWindow(0, []byte("other!"))
	if duplicate := index.Stats(); duplicate.CacheEntries != 1 || duplicate.CacheBytes != 6 {
		t.Fatalf("duplicate store changed cache = %+v", duplicate)
	}

	if _, err := index.ByteToPosition(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	evicted := index.Stats()
	if evicted.CacheEntries != 1 || evicted.CacheBytes != 6 ||
		evicted.CacheMisses != 2 {
		t.Fatalf("evicted cache stats = %+v", evicted)
	}
	beforeReload := source.readCallCount()
	if _, err := index.ByteToPosition(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := source.readCallCount(); got != beforeReload+1 {
		t.Fatalf("evicted window was not reloaded: %d -> %d", beforeReload, got)
	}

	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	closed := index.Stats()
	if closed.CacheEntries != 0 || closed.CacheBytes != 0 ||
		closed.CacheHits != 1 || closed.CacheMisses != 3 {
		t.Fatalf("closed cache stats = %+v", closed)
	}
}

func TestQueryWindowCacheOptionsOversizeAndCancellation(t *testing.T) {
	for _, options := range []Options{
		{CacheBytes: -1},
		{CacheBytes: MaximumCacheBytes + 1},
		{DisableCache: true, CacheBytes: 1},
	} {
		if _, err := Build(context.Background(), &testSource{}, 0, options); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("options %+v = %v", options, err)
		}
	}
	disabled, err := Build(context.Background(), &testSource{body: []byte("abc")}, 0, Options{CheckpointBytes: 1, DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats := disabled.Stats(); stats.MaximumCacheBytes != 0 {
		t.Fatalf("disabled cache = %+v", stats)
	}
	defer disabled.Close()

	source := &testSource{body: []byte("abcdefgh")}
	oversize, err := Build(context.Background(), source, 0, Options{CheckpointBytes: 4, CacheBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer oversize.Close()
	if _, err := oversize.ByteToPosition(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := oversize.ByteToPosition(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if stats := oversize.Stats(); stats.CacheEntries != 0 || stats.CacheBytes != 0 ||
		stats.CacheHits != 0 || stats.CacheMisses != 2 {
		t.Fatalf("oversize cache stats = %+v", stats)
	}

	cachedSource := &testSource{body: []byte("abcdefgh")}
	cached, err := Build(context.Background(), cachedSource, 0, Options{CheckpointBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Close()
	if _, err := cached.ByteToPosition(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := cached.Stats()
	if _, err := cached.ByteToPosition(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cached query = %v", err)
	}
	after := cached.Stats()
	if after.CacheHits != before.CacheHits || after.CacheMisses != before.CacheMisses {
		t.Fatalf("canceled query touched cache: before=%+v after=%+v", before, after)
	}
}

func TestQueryWindowCacheConcurrentExactBudget(t *testing.T) {
	source := &testSource{body: []byte("abcdefghijklmnopqrstuvwxyz")}
	index, err := Build(context.Background(), source, 1, Options{CheckpointBytes: 2, CacheBytes: 12})
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	var wait sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		wait.Add(1)
		go func(offset int64) {
			defer wait.Done()
			for iteration := 0; iteration < 100; iteration++ {
				if _, queryErr := index.ByteToPosition(context.Background(), offset); queryErr != nil {
					t.Errorf("query %d: %v", offset, queryErr)
					return
				}
			}
		}(int64(worker%13)*2 + 1)
	}
	wait.Wait()
	if stats := index.Stats(); stats.CacheBytes > stats.MaximumCacheBytes ||
		stats.CacheEntries == 0 || stats.CacheHits == 0 || stats.CacheMisses == 0 {
		t.Fatalf("concurrent cache stats = %+v", stats)
	}
}
