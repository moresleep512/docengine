package document

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEventHubHistoryReplayOverflowAndResume(t *testing.T) {
	hub := newEventHub(3, 8)
	for revision := uint64(1); revision <= 5; revision++ {
		hub.publish(SessionEvent{Kind: EventChanged, Metadata: Metadata{Revision: revision}})
	}
	subscription, err := hub.subscribe(SubscribeOptions{Buffer: 2})
	if err != nil {
		t.Fatal(err)
	}
	event := receiveEvent(t, subscription.Events())
	if event.Sequence != 5 || event.Metadata.Revision != 5 || event.Dropped != 4 {
		t.Fatalf("overflow replay = %+v", event)
	}

	hub.publish(SessionEvent{Kind: EventChanged, Metadata: Metadata{Revision: 6}})
	hub.publish(SessionEvent{Kind: EventChanged, Metadata: Metadata{Revision: 7}})
	for sequence := uint64(6); sequence <= 7; sequence++ {
		event = receiveEvent(t, subscription.Events())
		if event.Sequence != sequence || event.Dropped != 0 {
			t.Fatalf("live event = %+v, want sequence %d", event, sequence)
		}
	}

	resumed, err := hub.subscribe(SubscribeOptions{Buffer: 3, AfterSequence: 4})
	if err != nil {
		t.Fatal(err)
	}
	for sequence := uint64(5); sequence <= 7; sequence++ {
		event = receiveEvent(t, resumed.Events())
		if event.Sequence != sequence || event.Dropped != 0 {
			t.Fatalf("resumed event = %+v, want sequence %d", event, sequence)
		}
	}

	future, err := hub.subscribe(SubscribeOptions{Buffer: 1, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-future.Events():
		t.Fatalf("future-only replayed %+v", event)
	default:
	}
	hub.publish(SessionEvent{Kind: EventChanged, Metadata: Metadata{Revision: 8}})
	if event := receiveEvent(t, future.Events()); event.Sequence != 8 || event.Dropped != 0 {
		t.Fatalf("future event = %+v", event)
	}
	if err := errors.Join(subscription.Close(), resumed.Close(), future.Close()); err != nil {
		t.Fatal(err)
	}
	if err := subscription.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEventHubValidationAndClose(t *testing.T) {
	hub := newEventHub(1, 8)
	for _, options := range []SubscribeOptions{
		{Buffer: -1},
		{Buffer: MaximumSubscriptionBuffer + 1},
		{FutureOnly: true, AfterSequence: 1},
	} {
		if _, err := hub.subscribe(options); !errors.Is(err, ErrInvalidSubscription) {
			t.Fatalf("invalid subscription %+v = %v", options, err)
		}
	}
	if _, err := hub.subscribe(SubscribeOptions{AfterSequence: 1}); !errors.Is(err, ErrEventSequence) {
		t.Fatalf("future sequence = %v", err)
	}
	subscription, err := hub.subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hub.unsubscribe(999)
	hub.close()
	hub.close()
	hub.publish(SessionEvent{Kind: EventChanged})
	if _, ok := <-subscription.Events(); ok {
		t.Fatal("subscription channel remains open")
	}
	if _, err := hub.subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("subscribe after close = %v", err)
	}
	if err := subscription.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEventKindsRemainAppendOnly(t *testing.T) {
	kinds := []EventKind{
		EventOpened,
		EventRecovered,
		EventChanged,
		EventClosed,
		EventSaveStarted,
		EventSaveProgress,
		EventSaved,
		EventSaveFailed,
		EventJournalSyncFailed,
		EventJournalSyncRestored,
		EventCompactionStarted,
		EventCompactionProgress,
		EventCompacted,
		EventCompactionFailed,
	}
	for index, kind := range kinds {
		if want := EventKind(index + 1); kind != want {
			t.Fatalf("event kind %d = %d, want %d", index, kind, want)
		}
	}
}

func TestEventHubBudgetsStatisticsAndCounterBoundaries(t *testing.T) {
	hub := newEventHub(2, 2)
	for revision := uint64(1); revision <= 3; revision++ {
		hub.publish(SessionEvent{Kind: EventChanged, Metadata: Metadata{Revision: revision}})
	}
	first, err := hub.subscribe(SubscribeOptions{Buffer: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := hub.subscribe(SubscribeOptions{Buffer: 1, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.subscribe(SubscribeOptions{}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("subscription budget = %v", err)
	}
	replayed := receiveEvent(t, first.Events())
	if replayed.Sequence != 3 || replayed.Dropped != 2 {
		t.Fatalf("bounded replay = %+v", replayed)
	}
	stats := hub.stats()
	if stats.Sequence != 3 || stats.HistoryEntries != 2 || stats.MaximumHistory != 2 ||
		stats.Subscriptions != 2 || stats.MaximumSubscriptions != 2 ||
		stats.DiscardedDeliveries != 1 || stats.HistoryGapEvents != 1 ||
		stats.SequenceExhausted || stats.Closed {
		t.Fatalf("event stats = %+v", stats)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	hub.nextID = math.MaxUint64
	replacement, err := hub.subscribe(SubscribeOptions{Buffer: 1, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.id == 0 || replacement.id == second.id {
		t.Fatalf("wrapped subscription id = %d, existing = %d", replacement.id, second.id)
	}
	if saturatingAdd(math.MaxUint64-1, 2) != math.MaxUint64 ||
		saturatingAdd(7, 5) != 12 {
		t.Fatal("saturatingAdd did not preserve counter bounds")
	}
	if err := errors.Join(second.Close(), replacement.Close()); err != nil {
		t.Fatal(err)
	}

	exhausted := newEventHub(1, 1)
	watcher, err := exhausted.subscribe(SubscribeOptions{Buffer: 1, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	exhausted.sequence = math.MaxUint64
	exhausted.publish(SessionEvent{Kind: EventChanged})
	if _, ok := <-watcher.Events(); ok {
		t.Fatal("sequence exhaustion left subscription open")
	}
	stats = exhausted.stats()
	if stats.Sequence != math.MaxUint64 || !stats.SequenceExhausted || !stats.Closed ||
		stats.Subscriptions != 0 {
		t.Fatalf("exhausted stats = %+v", stats)
	}
	if _, err := exhausted.subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("subscribe after sequence exhaustion = %v", err)
	}
}

func TestSessionEventStatsSubscriptionLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{
		SessionDir: filepath.Join(dir, "session"),
		Limits: SessionLimits{
			EventHistory:     3,
			MaxSubscriptions: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := session.Subscribe(SubscribeOptions{FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := session.Subscribe(SubscribeOptions{FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Subscribe(SubscribeOptions{}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("Session subscription budget = %v", err)
	}
	stats := session.EventStats()
	if stats.Sequence != 1 || stats.HistoryEntries != 1 || stats.MaximumHistory != 3 ||
		stats.Subscriptions != 2 || stats.MaximumSubscriptions != 2 || stats.Closed {
		t.Fatalf("live Session event stats = %+v", stats)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if stats = session.EventStats(); stats.Subscriptions != 1 {
		t.Fatalf("stats after unsubscribe = %+v", stats)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if stats = session.EventStats(); !stats.Closed || stats.Subscriptions != 0 ||
		stats.Sequence != 2 {
		t.Fatalf("closed Session event stats = %+v", stats)
	}
	if _, ok := <-second.Events(); !ok {
		// EventClosed is retained in the subscriber buffer before closure.
		t.Fatal("subscriber closed before EventClosed delivery")
	}
	if _, ok := <-second.Events(); ok {
		t.Fatal("subscriber remains open after EventClosed")
	}
}

func TestSessionChangeEventsAndCloseBarrier(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 16})
	if err != nil {
		t.Fatal(err)
	}
	opened := receiveEvent(t, subscription.Events())
	if opened.Sequence != 1 || opened.Kind != EventOpened || opened.Origin != ChangeOriginNone || opened.Metadata.ByteLength != 3 {
		t.Fatalf("opened event = %+v", opened)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-subscription.Events():
		t.Fatalf("no-op published %+v", event)
	default:
	}
	apply, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 3, Insert: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	assertChangeEvent(t, receiveEvent(t, subscription.Events()), 2, ChangeOriginApply, apply)
	undo, err := session.Undo()
	if err != nil {
		t.Fatal(err)
	}
	assertChangeEvent(t, receiveEvent(t, subscription.Events()), 3, ChangeOriginUndo, undo)
	redo, err := session.Redo()
	if err != nil {
		t.Fatal(err)
	}
	assertChangeEvent(t, receiveEvent(t, subscription.Events()), 4, ChangeOriginRedo, redo)

	future, err := session.Subscribe(SubscribeOptions{Buffer: 1, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.ApplyBatch(context.Background(), 3, []ReplaceOperation{{Start: 4, Insert: "y"}})
	if err != nil {
		t.Fatal(err)
	}
	assertChangeEvent(t, receiveEvent(t, future.Events()), 5, ChangeOriginApply, result)
	assertChangeEvent(t, receiveEvent(t, subscription.Events()), 5, ChangeOriginApply, result)
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-future.Events(); ok {
		t.Fatal("explicitly closed subscription remains open")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	closed := receiveEvent(t, subscription.Events())
	if closed.Sequence != 6 || closed.Kind != EventClosed || closed.Cause != nil || closed.Metadata.Revision != 4 {
		t.Fatalf("closed event = %+v", closed)
	}
	if _, ok := <-subscription.Events(); ok {
		t.Fatal("Session close did not close subscription")
	}
	if _, err := session.Subscribe(SubscribeOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close = %v", err)
	}
}

func TestSessionRecoveryEventAndHistoryGap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc")
	options := OpenOptions{
		RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session-1"),
		Limits: SessionLimits{EventHistory: 2},
	}
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	for revision := uint64(0); revision < 3; revision++ {
		if _, err := session.ApplyBatch(context.Background(), revision, []ReplaceOperation{{Start: int64(revision + 1), Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 4})
	if err != nil {
		t.Fatal(err)
	}
	first := receiveEvent(t, subscription.Events())
	second := receiveEvent(t, subscription.Events())
	if first.Sequence != 3 || first.Dropped != 2 || second.Sequence != 4 || second.Dropped != 0 {
		t.Fatalf("history gap = (%+v, %+v)", first, second)
	}
	if err := subscription.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	options.SessionDir = filepath.Join(dir, "session-2")
	recovered, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	recoveryEvents, err := recovered.Subscribe(SubscribeOptions{Buffer: 2})
	if err != nil {
		recovered.Close()
		t.Fatal(err)
	}
	opened := receiveEvent(t, recoveryEvents.Events())
	recovery := receiveEvent(t, recoveryEvents.Events())
	if opened.Kind != EventOpened || recovery.Kind != EventRecovered || !recovery.Metadata.Recovered || recovery.Sequence != 2 {
		t.Fatalf("recovery events = (%+v, %+v)", opened, recovery)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCloseCallsShareBarrier(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "abc")
	_, lease, err := session.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 2, FutureOnly: true})
	if err != nil {
		lease.Close()
		t.Fatal(err)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- session.Close() }()
	waitForSessionClosedFlag(t, session)
	go func() { second <- session.Close() }()
	for name, result := range map[string]<-chan error{"first": first, "second": second} {
		select {
		case err := <-result:
			t.Fatalf("%s Close crossed barrier early: %v", name, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]<-chan error{"first": first, "second": second} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("%s Close = %v", name, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s Close did not cross barrier", name)
		}
	}
	closed := receiveEvent(t, subscription.Events())
	if closed.Kind != EventClosed {
		t.Fatalf("close barrier event = %+v", closed)
	}
	if _, ok := <-subscription.Events(); ok {
		t.Fatal("close barrier left channel open")
	}
}

func TestSessionCloseEventSurvivesSubscriberOverflow(t *testing.T) {
	session, _, _ := openAtomicTestSession(t, "a")
	subscription, err := session.Subscribe(SubscribeOptions{Buffer: 1, FutureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for revision := uint64(0); revision < 3; revision++ {
		if _, err := session.ApplyBatch(context.Background(), revision, []ReplaceOperation{{Start: int64(revision + 1), Insert: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	event := receiveEvent(t, subscription.Events())
	if event.Kind != EventClosed || event.Sequence != 5 || event.Dropped != 3 || event.Metadata.Revision != 3 {
		t.Fatalf("overflowed close event = %+v", event)
	}
	if _, ok := <-subscription.Events(); ok {
		t.Fatal("overflowed subscription remains open after close event")
	}
}

func TestEventHubConcurrentPublishAndClose(t *testing.T) {
	const (
		publisherCount  = 4
		eventsPerWorker = 250
		subscriberCount = 12
	)
	hub := newEventHub(8, subscriberCount)
	subscriptions := make([]*Subscription, subscriberCount)
	for index := range subscriptions {
		var err error
		subscriptions[index], err = hub.subscribe(SubscribeOptions{Buffer: 1})
		if err != nil {
			t.Fatal(err)
		}
	}

	errorsSeen := make(chan error, subscriberCount)
	var readers sync.WaitGroup
	for index, subscription := range subscriptions {
		readers.Add(1)
		go func(index int, subscription *Subscription) {
			defer readers.Done()
			var last uint64
			for event := range subscription.Events() {
				if event.Sequence <= last {
					errorsSeen <- fmt.Errorf("subscriber %d sequence %d after %d", index, event.Sequence, last)
					return
				}
				if want := event.Sequence - last - 1; event.Dropped != want {
					errorsSeen <- fmt.Errorf("subscriber %d event %d dropped %d, want %d", index, event.Sequence, event.Dropped, want)
					return
				}
				last = event.Sequence
			}
		}(index, subscription)
	}

	start := make(chan struct{})
	var publishers sync.WaitGroup
	for worker := 0; worker < publisherCount; worker++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			<-start
			for count := 0; count < eventsPerWorker; count++ {
				hub.publish(SessionEvent{Kind: EventChanged})
			}
		}()
	}
	close(start)
	for index := 1; index < len(subscriptions); index += 2 {
		if err := subscriptions[index].Close(); err != nil {
			t.Fatal(err)
		}
	}
	publishers.Wait()
	hub.close()
	readers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
}

func receiveEvent(t testing.TB, events <-chan SessionEvent) SessionEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("event channel closed early")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return SessionEvent{}
	}
}

func assertChangeEvent(t testing.TB, event SessionEvent, sequence uint64, origin ChangeOrigin, result ApplyResult) {
	t.Helper()
	if event.Sequence != sequence || event.Kind != EventChanged || event.Origin != origin ||
		event.Metadata.Revision != result.Revision || event.Metadata.ByteLength != result.ByteLength ||
		event.Changes.BeforeRevision() != result.Changes.BeforeRevision() || event.Changes.AfterRevision() != result.Changes.AfterRevision() {
		t.Fatalf("change event = %+v, result = %+v", event, result)
	}
}

func waitForSessionClosedFlag(t testing.TB, session *Session) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session.mu.RLock()
		closed := session.closed
		session.mu.RUnlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Session did not enter closing state")
}
