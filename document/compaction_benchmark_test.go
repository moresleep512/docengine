package document

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkEventHubPublish(b *testing.B) {
	for _, subscriberCount := range []int{0, DefaultMaxSubscriptions} {
		b.Run(fmt.Sprintf("subscribers-%d", subscriberCount), func(b *testing.B) {
			maximum := max(subscriberCount, 1)
			hub := newEventHub(DefaultEventHistory, maximum)
			for index := 0; index < subscriberCount; index++ {
				if _, err := hub.subscribe(SubscribeOptions{Buffer: 1, FutureOnly: true}); err != nil {
					b.Fatal(err)
				}
			}
			event := SessionEvent{Kind: EventChanged}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				hub.publish(event)
			}
			b.StopTimer()
			hub.close()
		})
	}
}

func BenchmarkUndoStoreRewrite4MiB(b *testing.B) {
	store, err := openUndoStore(b.TempDir(), 8<<20)
	if err != nil {
		b.Fatal(err)
	}
	defer store.close()
	refs := make([]textRef, 4)
	payload := make([]byte, 1<<20)
	for index := range refs {
		refs[index], err = store.append(payload)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(4 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		mapping, err := store.rewriteContext(context.Background(), refs, nil)
		if err != nil {
			b.Fatal(err)
		}
		for index, ref := range refs {
			refs[index] = mapping[ref]
		}
	}
}
