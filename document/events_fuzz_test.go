package document

import "testing"

func FuzzEventHubStateMachine(f *testing.F) {
	for _, seed := range [][]byte{
		{0},
		{2, 0, 0, 0, 2, 4, 0, 4},
		{7, 1, 2, 3, 0, 0, 0, 0, 0, 4, 5},
		{1, 0, 0, 0, 0, 2, 0, 0, 0, 3, 4},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 4_096 {
			input = input[:4_096]
		}
		historyLimit := 1
		if len(input) > 0 {
			historyLimit = int(input[0]%8) + 1
			input = input[1:]
		}
		hub := newEventHub(historyLimit)
		sequence := uint64(0)
		type trackedSubscription struct {
			subscription *Subscription
			cursor       uint64
			closed       bool
		}
		subscriptions := make([]trackedSubscription, 0, 16)

		publish := func() {
			sequence++
			hub.publish(SessionEvent{Kind: EventChanged, Metadata: Metadata{Revision: sequence}})
		}
		consume := func(tracked *trackedSubscription, blocking bool) {
			if tracked.closed {
				return
			}
			var (
				event SessionEvent
				ok    bool
			)
			if blocking {
				event, ok = <-tracked.subscription.Events()
			} else {
				select {
				case event, ok = <-tracked.subscription.Events():
				default:
					return
				}
			}
			if !ok {
				tracked.closed = true
				return
			}
			if event.Sequence <= tracked.cursor {
				t.Fatalf("sequence %d after cursor %d", event.Sequence, tracked.cursor)
			}
			if want := event.Sequence - tracked.cursor - 1; event.Dropped != want {
				t.Fatalf("event %d dropped %d, want %d after cursor %d", event.Sequence, event.Dropped, want, tracked.cursor)
			}
			if event.Metadata.Revision != event.Sequence {
				t.Fatalf("event %d carries revision %d", event.Sequence, event.Metadata.Revision)
			}
			tracked.cursor = event.Sequence
		}

		for _, action := range input {
			switch action % 6 {
			case 0, 1:
				publish()
			case 2:
				if len(subscriptions) == cap(subscriptions) {
					continue
				}
				distance := uint64(action>>3) + 1
				after := uint64(0)
				if distance < sequence {
					after = sequence - distance
				}
				subscription, err := hub.subscribe(SubscribeOptions{Buffer: int(action%4) + 1, AfterSequence: after})
				if err != nil {
					t.Fatal(err)
				}
				subscriptions = append(subscriptions, trackedSubscription{subscription: subscription, cursor: after})
			case 3:
				if len(subscriptions) == cap(subscriptions) {
					continue
				}
				subscription, err := hub.subscribe(SubscribeOptions{Buffer: int(action%4) + 1, FutureOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				subscriptions = append(subscriptions, trackedSubscription{subscription: subscription, cursor: sequence})
			case 4:
				if len(subscriptions) > 0 {
					consume(&subscriptions[int(action)%len(subscriptions)], false)
				}
			case 5:
				if len(subscriptions) > 0 {
					tracked := &subscriptions[int(action)%len(subscriptions)]
					if err := tracked.subscription.Close(); err != nil {
						t.Fatal(err)
					}
					for !tracked.closed {
						consume(tracked, true)
					}
				}
			}
		}

		publish()
		hub.close()
		for index := range subscriptions {
			for !subscriptions[index].closed {
				consume(&subscriptions[index], true)
			}
			if err := subscriptions[index].subscription.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
}
