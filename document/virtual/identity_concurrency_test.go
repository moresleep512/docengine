package virtual

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type errOnlyStepContext struct {
	calls int
}

func (c *errOnlyStepContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *errOnlyStepContext) Done() <-chan struct{}       { return nil }
func (c *errOnlyStepContext) Value(any) any               { return nil }
func (c *errOnlyStepContext) Err() error {
	c.calls++
	if c.calls >= 5 {
		return context.Canceled
	}
	return nil
}

func TestPageKeyIsScopedToIssuingPager(t *testing.T) {
	first, err := Build(context.Background(), testSource("abcdefgh"), 9, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Build(context.Background(), testSource("ABCDEFGH"), 9, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	window, err := first.WindowByByte(context.Background(), ByteWindowRequest{
		Revision: 9, Offset: 0, Budget: Budget{Bytes: 4, Pages: 1, Fragments: 1, Measure: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	key := window.Pages[0].Key
	page, err := first.ReadPage(context.Background(), key)
	if err != nil || string(page.Content) != "abcd" {
		t.Fatalf("issuing Pager ReadPage = (%q, %v)", page.Content, err)
	}
	if _, err := second.ReadPage(context.Background(), key); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("foreign Pager accepted PageKey: %v", err)
	}
	if _, err := second.ReadPage(context.Background(), PageKey{
		Revision: key.Revision, Generation: key.Generation, Index: key.Index,
		Start: key.Start, End: key.End,
	}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("reconstructed PageKey accepted: %v", err)
	}

	if _, err := first.Publish(context.Background(), Publication{
		Revision: 9, IndexedThrough: 8, Complete: true,
		Fragments: []Fragment{{ID: "all", Start: 0, End: 8, Measure: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReadPage(context.Background(), key); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("old generation PageKey error = %v", err)
	}
}

func TestMultiPageWindowCacheIsolationAndCancellation(t *testing.T) {
	pager, err := Build(context.Background(), testSource("abcdefghijkl"), 23, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, CacheBytes: 12,
		Window: Budget{Bytes: 12, Pages: 3, Fragments: 3, Measure: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()

	request := ByteWindowRequest{
		Revision: 23, Offset: 4, Before: 4, After: 8,
		Budget: Budget{Bytes: 12, Pages: 3, Fragments: 3, Measure: 3},
	}
	window, err := pager.WindowByByte(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if window.Bytes != 12 || len(window.Pages) != 3 {
		t.Fatalf("multi-page Window = bytes %d, pages %d", window.Bytes, len(window.Pages))
	}
	for _, page := range window.Pages {
		for index := range page.Content {
			page.Content[index] = 'x'
		}
	}
	reloaded, err := pager.WindowByByte(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt []byte
	for _, page := range reloaded.Pages {
		rebuilt = append(rebuilt, page.Content...)
	}
	if string(rebuilt) != "abcdefghijkl" {
		t.Fatalf("caller mutation changed cached content: %q", rebuilt)
	}
	if stats := pager.Stats(); stats.CacheEntries != 3 || stats.CacheBytes != 12 {
		t.Fatalf("cache accounting = %+v", stats)
	}

	key := reloaded.Pages[0].Key
	cancelAfterAdmission := &errOnlyStepContext{}
	if _, err := pager.ReadPage(cancelAfterAdmission, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached ReadPage cancellation = %v", err)
	}
	if stats := pager.Stats(); stats.ActiveTasks != 0 {
		t.Fatalf("cancelled cached read leaked task: %+v", stats)
	}
}

func TestConcurrentPublishHasExactlyOneAtomicWinner(t *testing.T) {
	const workers = 32
	pager, err := Build(context.Background(), testSource("abcdefgh"), 17, Options{
		TargetPageBytes: 4, MaximumPageBytes: 4, MaximumTasks: workers,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()

	start := make(chan struct{})
	type result struct {
		id    string
		stats Stats
		err   error
	}
	results := make(chan result, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for index := 0; index < workers; index++ {
		id := fmt.Sprintf("winner-%02d", index)
		go func() {
			ready.Done()
			<-start
			stats, publishErr := pager.Publish(context.Background(), Publication{
				Revision: 17, BaseGeneration: 0, IndexedThrough: 8, Complete: true,
				Fragments: []Fragment{{ID: id, Start: 0, End: 8, Measure: Measure(len(id))}},
			})
			results <- result{id: id, stats: stats, err: publishErr}
		}()
	}
	ready.Wait()
	close(start)

	winner := ""
	for range workers {
		result := <-results
		if result.err == nil {
			if winner != "" {
				t.Fatalf("multiple successful publications: %q and %q", winner, result.id)
			}
			winner = result.id
			if result.stats.Generation != 1 || result.stats.Fragments != 1 {
				t.Fatalf("winner Stats = %+v", result.stats)
			}
			continue
		}
		if !errors.Is(result.err, ErrStaleGeneration) {
			t.Fatalf("losing Publish(%q) = %v", result.id, result.err)
		}
	}
	if winner == "" {
		t.Fatal("no publication won")
	}

	stats := pager.Stats()
	if stats.Generation != 1 || stats.Fragments != 1 || stats.TotalMeasure != Measure(len(winner)) {
		t.Fatalf("final Stats = %+v, winner = %q", stats, winner)
	}
	window, err := pager.WindowByFragment(context.Background(), FragmentWindowRequest{
		Revision: 17, Generation: 1, ID: winner,
	})
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt []byte
	for _, page := range window.Pages {
		if page.FragmentID != winner || page.MeasureStart != 0 ||
			page.MeasureEnd != Measure(len(winner)) {
			t.Fatalf("mixed publication Page = %+v", page)
		}
		rebuilt = append(rebuilt, page.Content...)
	}
	if string(rebuilt) != "abcdefgh" {
		t.Fatalf("winner content = %q", rebuilt)
	}
}
