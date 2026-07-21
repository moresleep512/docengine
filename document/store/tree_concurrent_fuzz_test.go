package store

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"
)

// FuzzTreeConcurrentReadDuringEdits stresses snapshot immutability under
// concurrency: many reader goroutines take snapshots and read them while a
// single writer applies ReplacePiece edits driven by the fuzzer program. The
// oracle is that every snapshot a reader captures reads back exactly the bytes
// it observed at capture time, regardless of subsequent writes, and that the
// writer's final tree matches the reference model. Run with -race to catch data
// races on the treap's shared structure.
//
// This complements the deterministic TestConcurrentSnapshotsDuringEdits by
// driving the edit sequence with random data and verifying byte-equality of
// retired snapshots (not just ReadAt error-freedom).
func FuzzTreeConcurrentReadDuringEdits(f *testing.F) {
	f.Add([]byte("base"), []byte{0, 1, 2, 3, 4, 5})
	f.Add(bytes.Repeat([]byte{'a'}, 32), bytes.Repeat([]byte{1}, 64))
	f.Add([]byte("x"), []byte{})
	f.Fuzz(func(t *testing.T, base, program []byte) {
		const maxBaseBytes, maxProgramBytes, maxDocument = 512, 2048, 4096
		if len(base) > maxBaseBytes {
			base = base[:maxBaseBytes]
		}
		if len(program) > maxProgramBytes {
			program = program[:maxProgramBytes]
		}
		tree := mustNewTree(t, base)
		var journal appendSource
		tree.SetSource(SourceJournal, journal.reader())
		reference := append([]byte(nil), base...)

		// Reader goroutines continuously snapshot + verify byte immutability.
		// They stop when the writer closes the done channel.
		done := make(chan struct{})
		failures := make(chan string, runtime.NumCPU())
		var readers sync.WaitGroup
		readerCount := runtime.NumCPU()
		if readerCount > 8 {
			readerCount = 8
		}
		if readerCount < 2 {
			readerCount = 2
		}
		for r := 0; r < readerCount; r++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				rng := rand.New(rand.NewPCG(uint64(r)+1, 999))
				for {
					select {
					case <-done:
						return
					default:
					}
					snapshot := tree.Snapshot()
					if snapshot.Len() == 0 {
						continue
					}
					captured, err := io.ReadAll(io.NewSectionReader(snapshot, 0, snapshot.Len()))
					if err != nil {
						select {
						case failures <- "snapshot read error: " + err.Error():
						default:
						}
						return
					}
					// Read the same snapshot again; the bytes must be identical.
					again, err := io.ReadAll(io.NewSectionReader(snapshot, 0, snapshot.Len()))
					if err != nil || !bytes.Equal(captured, again) {
						select {
						case failures <- "snapshot mutated between reads":
						default:
						}
						return
					}
					// Random sub-range reads must not error beyond EOF and must
					// stay consistent with the captured snapshot length.
					off := rng.Int64N(snapshot.Len() + 1)
					buf := make([]byte, rng.IntN(16))
					if _, err := snapshot.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
						select {
						case failures <- "snapshot ReadAt error: " + err.Error():
						default:
						}
						return
					}
				}
			}()
		}

		// Single writer: apply edits decoded from the 6-byte program records,
		// mirroring FuzzTreeMatchesReference's encoding. The insert length is
		// clamped so the document never exceeds maxDocument; this keeps the
		// reference model and the tree byte-for-byte aligned (truncating only
		// the reference would desync the two and produce false mismatches).
		for position := 0; position+6 <= len(program); position += 6 {
			record := program[position : position+6]
			start := int64(uint16(record[0]) | uint16(record[1])<<8)
			deleteLen := int64(record[2])
			insertLen := int64(record[5])
			if start > int64(len(reference)) {
				start = int64(len(reference))
			}
			if deleteLen < 0 {
				deleteLen = 0
			}
			if deleteLen > int64(len(reference))-start {
				deleteLen = int64(len(reference)) - start
			}
			if insertLen > 16 {
				insertLen = 16
			}
			if remain := int64(maxDocument) - (int64(len(reference)) - deleteLen); insertLen > remain {
				insertLen = remain
			}
			if insertLen < 0 {
				insertLen = 0
			}
			insert := make([]byte, insertLen)
			for i := range insert {
				insert[i] = byte('a' + ((int(record[3]) + i) % 26))
			}
			offset := journal.add(insert)
			tree.SetSource(SourceJournal, journal.reader())
			piece := Piece{}
			if len(insert) > 0 {
				piece = Piece{Source: SourceJournal, Offset: offset, Length: int64(len(insert))}
			}
			if _, _, err := tree.ReplacePiece(start, deleteLen, piece); err != nil {
				t.Fatalf("edit at %d: %v", position, err)
			}
			reference = splice(reference, int(start), int(deleteLen), insert)
		}

		close(done)
		readers.Wait()
		close(failures)
		for msg := range failures {
			t.Error(msg)
		}

		// Final tree must match the reference model exactly and satisfy invariants.
		assertSnapshotBytes(t, tree.Snapshot(), reference)
		assertTreeInvariants(t, tree)
	})
}
