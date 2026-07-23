package document

import (
	"context"
	"errors"
	"testing"

	"github.com/moresleep512/docengine/document/virtual"
)

func FuzzVirtualPagerSessionLifecycle(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 4, 2, 5, 2, 6, 2, 3})
	f.Add([]byte{1, 0, 4, 0, 1, 5, 6, 2, 3})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 256 {
			return
		}
		session, _, _ := openAtomicTestSession(t, "seed")
		type pinned struct {
			pager *virtual.Pager
			text  string
		}
		var pagers []pinned
		closePagers := func() {
			for _, item := range pagers {
				if err := item.pager.Close(); err != nil {
					t.Errorf("Pager.Close: %v", err)
				}
			}
			pagers = nil
		}
		defer func() {
			closePagers()
			if err := session.Close(); err != nil {
				t.Errorf("Session.Close: %v", err)
			}
		}()

		for index, operation := range operations {
			switch operation % 8 {
			case 0:
				metadata := session.Metadata()
				position := int64(0)
				if metadata.ByteLength > 0 {
					position = int64(operation>>3) % (metadata.ByteLength + 1)
				}
				deleteLength := int64(0)
				if operation&0x80 != 0 && position < metadata.ByteLength {
					deleteLength = 1
				}
				if _, err := session.ApplyBatch(context.Background(), metadata.Revision, []ReplaceOperation{{
					Start: position, DeleteLength: deleteLength,
					Insert: string([]byte{'a' + byte(index%26)}),
				}}); err != nil {
					t.Fatalf("ApplyBatch[%d]: %v", index, err)
				}
			case 1:
				if len(pagers) == 16 {
					if err := pagers[0].pager.Close(); err != nil {
						t.Fatal(err)
					}
					pagers = pagers[1:]
				}
				content, err := readSession(session, session.Metadata().ByteLength)
				if err != nil {
					t.Fatal(err)
				}
				pager, err := session.VirtualPager(context.Background(), virtual.Options{
					TargetPageBytes: 4, MaximumPageBytes: 8,
					Window: virtual.Budget{
						Bytes: 1024, Pages: 512, Fragments: 512, Measure: 1,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				pagers = append(pagers, pinned{pager: pager, text: string(content)})
			case 2:
				for _, item := range pagers {
					assertPagerText(t, item.pager, item.text)
				}
			case 3:
				if len(pagers) != 0 {
					selected := int(operation>>3) % len(pagers)
					if err := pagers[selected].pager.Close(); err != nil {
						t.Fatal(err)
					}
					pagers = append(pagers[:selected], pagers[selected+1:]...)
				}
			case 4:
				if _, err := session.Save(); err != nil {
					t.Fatalf("Save[%d]: %v", index, err)
				}
			case 5:
				if _, err := session.Undo(); err != nil && !errors.Is(err, ErrNothingToUndo) {
					t.Fatalf("Undo[%d]: %v", index, err)
				}
			case 6:
				if _, err := session.Redo(); err != nil && !errors.Is(err, ErrNothingToRedo) {
					t.Fatalf("Redo[%d]: %v", index, err)
				}
			case 7:
				if _, err := session.Compact(context.Background(), CompactOptions{
					CheckpointJournal: operation&0x80 != 0,
				}); err != nil {
					t.Fatalf("Compact[%d]: %v", index, err)
				}
			}
		}
		for _, item := range pagers {
			assertPagerText(t, item.pager, item.text)
		}
	})
}
