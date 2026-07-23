package document

import (
	"errors"
	"math"
	"sync"

	"github.com/moresleep512/docengine/document/coordinate"
	"github.com/moresleep512/docengine/document/virtual"
)

const (
	// DefaultEventHistory is the number of recent Session events retained for
	// resumable subscriptions when OpenOptions does not specify a limit.
	DefaultEventHistory = 256
	// MaximumEventHistory bounds the memory retained by one Session event hub.
	MaximumEventHistory = 65_536
	// DefaultSubscriptionBuffer is used when SubscribeOptions.Buffer is zero.
	DefaultSubscriptionBuffer = 64
	// MaximumSubscriptionBuffer bounds one subscriber's pending event queue.
	MaximumSubscriptionBuffer = 4_096
)

var (
	// ErrInvalidSubscription reports an invalid buffer or cursor combination.
	ErrInvalidSubscription = errors.New("document: invalid subscription options")
	// ErrEventSequence reports an AfterSequence newer than the Session stream.
	ErrEventSequence = errors.New("document: event sequence is in the future")
)

// EventKind identifies a Session lifecycle or content transition.
type EventKind uint8

const (
	// EventOpened is the first transition in every successfully opened Session.
	EventOpened EventKind = iota + 1
	// EventRecovered follows EventOpened when journal operations were replayed.
	EventRecovered
	// EventChanged reports one committed ApplyBatch, Undo, or Redo transaction.
	EventChanged
	// EventClosed is the last event and precedes subscription channel closure.
	EventClosed
	// EventSaveStarted begins a persistence attempt that will perform I/O.
	EventSaveStarted
	// EventSaveProgress reports monotonically increasing bytes written for one
	// persistence attempt.
	EventSaveProgress
	// EventSaved reports a committed persistence attempt. Cause may contain a
	// DurabilityError when replacement succeeded but directory sync did not.
	EventSaved
	// EventSaveFailed reports an attempt that did not complete normally.
	// Persistence.Committed distinguishes pre-commit failure from a permanent
	// post-commit Session fault.
	EventSaveFailed
	// EventJournalSyncFailed reports the transition from a healthy recovery WAL
	// to a failed background or close-time Sync.
	EventJournalSyncFailed
	// EventJournalSyncRestored reports the first successful Sync or clean save
	// checkpoint after EventJournalSyncFailed.
	EventJournalSyncRestored
	// EventCompactionStarted begins structural and undo-store reclamation.
	EventCompactionStarted
	// EventCompactionProgress reports monotonically increasing live undo bytes
	// copied into the candidate replacement store.
	EventCompactionProgress
	// EventCompacted reports a successfully committed compaction attempt.
	EventCompacted
	// EventCompactionFailed reports a compaction error. Compaction.Committed
	// distinguishes a discarded candidate from committed cleanup failure.
	EventCompactionFailed
	// EventVirtualizationStarted begins one Fragment publication or refresh.
	EventVirtualizationStarted
	// EventVirtualizationProgress reports a provider watermark advance.
	EventVirtualizationProgress
	// EventVirtualizationCompleted reports an atomically published generation.
	EventVirtualizationCompleted
	// EventVirtualizationFailed reports a rejected or canceled publication.
	EventVirtualizationFailed
)

// ChangeOrigin identifies the operation that produced an EventChanged map.
type ChangeOrigin uint8

const (
	// ChangeOriginNone is used by events that do not contain a ChangeMap.
	ChangeOriginNone ChangeOrigin = iota
	// ChangeOriginApply identifies a successful non-empty ApplyBatch.
	ChangeOriginApply
	// ChangeOriginUndo identifies a successful Undo transaction.
	ChangeOriginUndo
	// ChangeOriginRedo identifies a successful Redo transaction.
	ChangeOriginRedo
)

// SessionEvent is an immutable state transition. Dropped is specific to one
// subscription and reports how many preceding events were omitted before this
// delivery. Consumers that observe a drop must rebuild derived state from the
// event Metadata and a matching Snapshot instead of applying Changes blindly.
type SessionEvent struct {
	Sequence       uint64
	Dropped        uint64
	Kind           EventKind
	Origin         ChangeOrigin
	Metadata       Metadata
	Changes        coordinate.ChangeMap
	Persistence    PersistenceProgress
	Compaction     CompactionProgress
	Virtualization virtual.Progress
	Cause          error
}

// PersistenceProgress correlates save events. CompletedBytes is monotonic for
// an Operation and never exceeds TotalBytes. TargetRevision is the immutable
// Snapshot selected when the attempt began.
type PersistenceProgress struct {
	OperationID    uint64
	TargetRevision uint64
	CompletedBytes int64
	TotalBytes     int64
	Committed      bool
}

// CompactionProgress correlates compaction events. TotalBytes is the exact
// unique live undo payload selected by the attempt. CompletedBytes is
// monotonic and never exceeds it.
type CompactionProgress struct {
	OperationID         uint64
	CompletedBytes      int64
	TotalBytes          int64
	PiecesBefore        int64
	PiecesAfter         int64
	JournalCheckpointed bool
	Committed           bool
}

// EventStats is an atomic view of retained history, live subscriptions, and
// subscriber-specific delivery loss. It remains available after Session close.
type EventStats struct {
	Sequence             uint64
	HistoryEntries       int
	MaximumHistory       int
	Subscriptions        int
	MaximumSubscriptions int
	DiscardedDeliveries  uint64
	HistoryGapEvents     uint64
	SequenceExhausted    bool
	Closed               bool
}

// SubscribeOptions controls replay and buffering for a Session subscription.
// AfterSequence resumes after a previously observed event. FutureOnly skips
// retained history and cannot be combined with AfterSequence.
type SubscribeOptions struct {
	Buffer        int
	AfterSequence uint64
	FutureOnly    bool
}

// Subscription is a nonblocking Session event stream. Events returns a channel
// that is closed by Subscription.Close or after the Session close barrier.
type Subscription struct {
	hub    *eventHub
	id     uint64
	events <-chan SessionEvent
	once   sync.Once
}

// Events returns the ordered delivery channel for this subscription.
func (s *Subscription) Events() <-chan SessionEvent { return s.events }

// Close detaches the subscription and closes its event channel. It is
// idempotent and does not close the Session.
func (s *Subscription) Close() error {
	s.once.Do(func() { s.hub.unsubscribe(s.id) })
	return nil
}

type subscriptionState struct {
	events         chan SessionEvent
	pendingDropped uint64
}

type eventHub struct {
	mu             sync.Mutex
	closed         bool
	sequence       uint64
	nextID         uint64
	history        []SessionEvent
	historyStart   int
	historyCount   int
	subscribers    map[uint64]*subscriptionState
	maxSubscribers int
	discarded      uint64
	historyGaps    uint64
	exhausted      bool
}

func newEventHub(historyLimit, maxSubscribers int) *eventHub {
	return &eventHub{
		history: make([]SessionEvent, historyLimit), subscribers: make(map[uint64]*subscriptionState),
		maxSubscribers: maxSubscribers,
	}
}

func (h *eventHub) publish(event SessionEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	if h.sequence == math.MaxUint64 {
		h.exhausted = true
		h.closed = true
		for id, subscriber := range h.subscribers {
			delete(h.subscribers, id)
			close(subscriber.events)
		}
		return
	}
	h.sequence++
	event.Sequence = h.sequence
	event.Dropped = 0
	h.appendHistory(event)
	for _, subscriber := range h.subscribers {
		h.discarded = saturatingAdd(h.discarded, offerEvent(subscriber, event))
	}
}

func (h *eventHub) appendHistory(event SessionEvent) {
	if h.historyCount < len(h.history) {
		index := (h.historyStart + h.historyCount) % len(h.history)
		h.history[index] = event
		h.historyCount++
		return
	}
	h.history[h.historyStart] = event
	h.historyStart = (h.historyStart + 1) % len(h.history)
}

func (h *eventHub) subscribe(options SubscribeOptions) (*Subscription, error) {
	buffer := options.Buffer
	if buffer == 0 {
		buffer = DefaultSubscriptionBuffer
	}
	if buffer < 0 || buffer > MaximumSubscriptionBuffer || options.FutureOnly && options.AfterSequence != 0 {
		return nil, ErrInvalidSubscription
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	if options.AfterSequence > h.sequence {
		return nil, ErrEventSequence
	}
	if len(h.subscribers) >= h.maxSubscribers {
		return nil, ErrLimitExceeded
	}
	after := options.AfterSequence
	if options.FutureOnly {
		after = h.sequence
	}
	state := &subscriptionState{events: make(chan SessionEvent, buffer)}
	if h.historyCount > 0 && after < h.sequence {
		earliest := h.sequence - uint64(h.historyCount) + 1
		if after < earliest-1 {
			state.pendingDropped = earliest - after - 1
			h.historyGaps = saturatingAdd(h.historyGaps, state.pendingDropped)
		}
		for index := 0; index < h.historyCount; index++ {
			event := h.history[(h.historyStart+index)%len(h.history)]
			if event.Sequence > after {
				h.discarded = saturatingAdd(h.discarded, offerEvent(state, event))
			}
		}
	}
	id := h.nextSubscriptionID()
	h.subscribers[id] = state
	return &Subscription{hub: h, id: id, events: state.events}, nil
}

func offerEvent(subscriber *subscriptionState, event SessionEvent) uint64 {
	event.Dropped = subscriber.pendingDropped
	select {
	case subscriber.events <- event:
		subscriber.pendingDropped = 0
		return 0
	default:
	}
	dropped := subscriber.pendingDropped
	var discarded uint64
	for {
		select {
		case previous := <-subscriber.events:
			dropped = saturatingAdd(dropped, saturatingAdd(previous.Dropped, 1))
			discarded++
		default:
			event.Dropped = dropped
			subscriber.events <- event
			subscriber.pendingDropped = 0
			return discarded
		}
	}
}

func (h *eventHub) nextSubscriptionID() uint64 {
	for {
		h.nextID++
		if h.nextID == 0 {
			h.nextID++
		}
		if _, exists := h.subscribers[h.nextID]; !exists {
			return h.nextID
		}
	}
}

func (h *eventHub) stats() EventStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return EventStats{
		Sequence: h.sequence, HistoryEntries: h.historyCount, MaximumHistory: len(h.history),
		Subscriptions: len(h.subscribers), MaximumSubscriptions: h.maxSubscribers,
		DiscardedDeliveries: h.discarded, HistoryGapEvents: h.historyGaps,
		SequenceExhausted: h.exhausted, Closed: h.closed,
	}
}

func (h *eventHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if subscriber, ok := h.subscribers[id]; ok {
		delete(h.subscribers, id)
		close(subscriber.events)
	}
}

func (h *eventHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, subscriber := range h.subscribers {
		delete(h.subscribers, id)
		close(subscriber.events)
	}
}

func saturatingAdd(value, increment uint64) uint64 {
	if increment > math.MaxUint64-value {
		return math.MaxUint64
	}
	return value + increment
}
