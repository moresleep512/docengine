package document

import "testing"

func BenchmarkSessionSnapshotLease(b *testing.B) {
	session, _, _ := openLifecycleTestSession(b, "snapshot content", 1, 0)
	defer session.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, lease, err := session.Snapshot()
		if err != nil {
			b.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
