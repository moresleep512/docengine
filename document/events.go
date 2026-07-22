package document

import (
	"errors"
	"sync"

	"github.com/moresleep512/docengine/document/coordinate"
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
	Sequence uint64
	Dropped  uint64
	Kind     EventKind
	Origin   ChangeOrigin
	Metadata Metadata
	Changes  coordinate.ChangeMap
	Cause    error
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
	mu           sync.Mutex
	closed       bool
	sequence     uint64
	nextID       uint64
	history      []SessionEvent
	historyStart int
	historyCount int
	subscribers  map[uint64]*subscriptionState
}

func newEventHub(historyLimit int) *eventHub {
	return &eventHub{history: make([]SessionEvent, historyLimit), subscribers: make(map[uint64]*subscriptionState)}
}

func (h *eventHub) publish(event SessionEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.sequence++
	event.Sequence = h.sequence
	event.Dropped = 0
	h.appendHistory(event)
	for _, subscriber := range h.subscribers {
		offerEvent(subscriber, event)
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
	after := options.AfterSequence
	if options.FutureOnly {
		after = h.sequence
	}
	h.nextID++
	state := &subscriptionState{events: make(chan SessionEvent, buffer)}
	if h.historyCount > 0 && after < h.sequence {
		earliest := h.sequence - uint64(h.historyCount) + 1
		if after < earliest-1 {
			state.pendingDropped = earliest - after - 1
		}
		for index := 0; index < h.historyCount; index++ {
			event := h.history[(h.historyStart+index)%len(h.history)]
			if event.Sequence > after {
				offerEvent(state, event)
			}
		}
	}
	h.subscribers[h.nextID] = state
	return &Subscription{hub: h, id: h.nextID, events: state.events}, nil
}

func offerEvent(subscriber *subscriptionState, event SessionEvent) {
	event.Dropped = subscriber.pendingDropped
	select {
	case subscriber.events <- event:
		subscriber.pendingDropped = 0
		return
	default:
	}
	dropped := subscriber.pendingDropped
	for {
		select {
		case previous := <-subscriber.events:
			dropped += previous.Dropped + 1
		default:
			event.Dropped = dropped
			subscriber.events <- event
			subscriber.pendingDropped = 0
			return
		}
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
