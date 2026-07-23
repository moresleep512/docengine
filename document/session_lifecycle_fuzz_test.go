package document

import (
	"context"
	"errors"
	"testing"

	"github.com/moresleep512/docengine/document/coordinate"
	"github.com/moresleep512/docengine/document/virtual"
)

func FuzzSessionLifecycleBudgets(f *testing.F) {
	f.Add([]byte{2, 0, 2, 0, 5, 1, 7, 3})
	f.Add([]byte{1, 0, 0, 2, 5, 1, 0, 6, 7})
	f.Add([]byte{4, 2, 0, 2, 0, 2, 0, 5, 1, 1, 1, 1})
	f.Fuzz(func(t *testing.T, program []byte) {
		if len(program) == 0 {
			return
		}
		maximum := 1 + int(program[0]%4)
		session, _, _ := openLifecycleTestSession(t, "a", maximum, 0)
		type heldLease struct {
			lease   SnapshotLease
			content string
		}
		var leases []heldLease
		content := "a"
		revision := uint64(0)
		for step, opcode := range program[1:] {
			if step >= 64 {
				break
			}
			switch opcode % 8 {
			case 0:
				gotRevision, lease, err := session.Snapshot()
				if len(leases) == maximum {
					if !errors.Is(err, ErrLimitExceeded) || lease != nil {
						t.Fatalf("Snapshot at limit = (%d, %T, %v)", gotRevision, lease, err)
					}
					break
				}
				if err != nil || gotRevision != revision {
					t.Fatalf("Snapshot = (%d, %v), want revision %d", gotRevision, err, revision)
				}
				leases = append(leases, heldLease{lease: lease, content: content})
			case 1:
				if len(leases) == 0 {
					break
				}
				index := int(opcode) % len(leases)
				assertLifecycleLeaseContent(t, leases[index])
				if err := leases[index].lease.Close(); err != nil {
					t.Fatal(err)
				}
				leases = append(leases[:index], leases[index+1:]...)
			case 2:
				insert := string(rune('b' + opcode%24))
				result, err := session.ApplyBatch(context.Background(), revision, []ReplaceOperation{{
					Start: int64(len(content)), Insert: insert,
				}})
				if err != nil {
					t.Fatal(err)
				}
				revision++
				content += insert
				if result.Revision != revision || result.ByteLength != int64(len(content)) {
					t.Fatalf("ApplyBatch = %+v, want revision=%d length=%d", result, revision, len(content))
				}
			case 3:
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				before := session.Metadata()
				if _, err := session.SaveContext(canceled); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled SaveContext = %v", err)
				}
				if after := session.Metadata(); after != before {
					t.Fatalf("canceled SaveContext changed metadata: before=%+v after=%+v", before, after)
				}
			case 4:
				stats := session.LifecycleStats()
				if stats.ActiveSnapshotLeases != len(leases) || stats.ActiveSnapshotLeases > stats.MaxSnapshotLeases ||
					stats.PeakSnapshotLeases < stats.ActiveSnapshotLeases || stats.MaxSnapshotLeases != maximum {
					t.Fatalf("LifecycleStats = %+v, held=%d", stats, len(leases))
				}
			case 5:
				metadata, err := session.Save()
				if err != nil {
					t.Fatal(err)
				}
				if metadata.CommittedRevision != revision || metadata.Dirty {
					t.Fatalf("Save metadata = %+v, want revision %d", metadata, revision)
				}
			case 6:
				index, err := session.CoordinateIndex(context.Background(), coordinate.Options{})
				if len(leases) == maximum {
					if !errors.Is(err, ErrLimitExceeded) || index != nil {
						t.Fatalf("CoordinateIndex at limit = (%T, %v)", index, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if closeErr := index.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				}
			case 7:
				pager, err := session.VirtualPager(context.Background(), virtual.Options{})
				if len(leases) == maximum {
					if !errors.Is(err, ErrLimitExceeded) || pager != nil {
						t.Fatalf("VirtualPager at limit = (%T, %v)", pager, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if closeErr := pager.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}
				}
			}
			assertLifecycleSessionContent(t, session, content)
		}
		for _, item := range leases {
			assertLifecycleLeaseContent(t, item)
			if err := item.lease.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if stats := session.LifecycleStats(); stats.ActiveSnapshotLeases != 0 {
			t.Fatalf("final LifecycleStats = %+v", stats)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func assertLifecycleLeaseContent(t testing.TB, item struct {
	lease   SnapshotLease
	content string
}) {
	t.Helper()
	buffer := make([]byte, len(item.content))
	n, err := item.lease.ReadAt(buffer, 0)
	if n != len(buffer) || err != nil || string(buffer) != item.content {
		t.Fatalf("leased content = (%d, %q, %v), want %q", n, buffer, err, item.content)
	}
}

func assertLifecycleSessionContent(t testing.TB, session *Session, want string) {
	t.Helper()
	buffer := make([]byte, len(want))
	n, err := session.ReadAt(buffer, 0)
	if n != len(buffer) || err != nil || string(buffer) != want {
		t.Fatalf("Session content = (%d, %q, %v), want %q", n, buffer, err, want)
	}
}
