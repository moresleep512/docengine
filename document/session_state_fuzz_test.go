package document

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// FuzzSessionStateMachine is the Session-level state fuzzer, the largest testing
// blind spot before this file: the existing fuzzers cover the Piece Tree and
// the recovery journal in isolation, but never the coordination layer that
// owns revision history, undo/redo, the WAL, and atomic save together.
//
// A byte program drives ApplyBatch / Undo / Redo / Save / Compact / Snapshot against a
// single open session. A parallel reference model maintains the document
// content and the undo/redo edit stacks, and after every operation the live
// session content is asserted byte-for-byte equal to the model. After every
// Save the on-disk file is asserted equal to the model as well.
//
// The program is interpreted one opcode at a time; ApplyBatch consumes three
// extra bytes for (start, deleteLength, insertLength). Inserts are drawn from a
// fixed ASCII alphabet so they are always valid UTF-8 and the document length
// is clamped so the reference model and the session never diverge.
func FuzzSessionStateMachine(f *testing.F) {
	f.Add([]byte("hello"), []byte{0, 3, 0, 1, 1, 2, 0, 0, 5, 0, 1, 2, 0, 3, 0, 0, 4})
	f.Add([]byte("abc"), []byte{0, 0, 0, 1, 0, 1, 0, 2, 0, 2, 0, 0, 3})
	f.Add([]byte("x"), []byte{0, 1, 0, 0, 0, 1, 3, 4})
	f.Fuzz(func(t *testing.T, base, program []byte) {
		const maxBaseBytes, maxProgramBytes, maxDocument = 256, 2048, 4096
		base = sanitizeBase(base, maxBaseBytes)
		if len(program) > maxProgramBytes {
			program = program[:maxProgramBytes]
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "doc.md")
		recoveryDir := filepath.Join(dir, "recovery")
		if err := os.WriteFile(path, base, 0o600); err != nil {
			t.Fatal(err)
		}
		session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session")})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = session.Close() }()

		reference := append([]byte(nil), base...)
		committedReference := append([]byte(nil), base...)
		var undoStack, redoStack []sessionEdit
		previousRevision := session.Metadata().Revision

		assertContent := func() {
			t.Helper()
			if !bytes.Equal(sessionContent(t, session), reference) {
				t.Fatalf("content mismatch\n got: %q\nwant: %q", sessionContent(t, session), reference)
			}
			meta := session.Metadata()
			if meta.ByteLength != int64(len(reference)) {
				t.Fatalf("length %d, want %d", meta.ByteLength, len(reference))
			}
			if meta.Revision < previousRevision {
				t.Fatalf("revision retreated %d -> %d", previousRevision, meta.Revision)
			}
			previousRevision = meta.Revision
		}

		pos := 0
		readByte := func() (byte, bool) {
			if pos >= len(program) {
				return 0, false
			}
			b := program[pos]
			pos++
			return b, true
		}

		for {
			op, ok := readByte()
			if !ok {
				break
			}
			switch op % 6 {
			case 0: // ApplyBatch (one operation)
				startByte, ok1 := readByte()
				deleteByte, ok2 := readByte()
				insertByte, ok3 := readByte()
				if !ok1 || !ok2 || !ok3 {
					return
				}
				start := int64(startByte) % (int64(len(reference)) + 1)
				maxDelete := int64(len(reference)) - start
				deleteLength := int64(deleteByte) % (maxDelete + 1)
				insertLength := int64(insertByte) % 8
				// Clamp the insert so the document never exceeds maxDocument;
				// this keeps the reference model and the session in lockstep.
				if remain := int64(maxDocument) - (int64(len(reference)) - deleteLength); insertLength > remain {
					insertLength = remain
				}
				if insertLength < 0 {
					insertLength = 0
				}
				insert := make([]byte, insertLength)
				for i := range insert {
					insert[i] = byte('a' + (int(insertByte)+i)%26)
				}
				deleted := append([]byte(nil), reference[start:int(start)+int(deleteLength)]...)
				reference = spliceBytes(reference, int(start), int(deleteLength), insert)
				undoStack = append(undoStack, sessionEdit{start: start, deleted: deleted, inserted: insert})
				redoStack = redoStack[:0]
				meta := session.Metadata()
				if _, err := session.ApplyBatch(context.Background(), meta.Revision, []ReplaceOperation{{
					Start: start, DeleteLength: deleteLength, Insert: string(insert),
				}}); err != nil {
					t.Fatalf("ApplyBatch: %v", err)
				}
				assertContent()
			case 1: // Undo
				if len(undoStack) == 0 {
					if _, err := session.Undo(); !errors.Is(err, ErrNothingToUndo) {
						t.Fatalf("Undo on empty stack = %v, want %v", err, ErrNothingToUndo)
					}
					continue
				}
				e := undoStack[len(undoStack)-1]
				undoStack = undoStack[:len(undoStack)-1]
				reference = spliceBytes(reference, int(e.start), len(e.inserted), e.deleted)
				redoStack = append(redoStack, e)
				if _, err := session.Undo(); err != nil {
					t.Fatalf("Undo: %v", err)
				}
				assertContent()
			case 2: // Redo
				if len(redoStack) == 0 {
					if _, err := session.Redo(); !errors.Is(err, ErrNothingToRedo) {
						t.Fatalf("Redo on empty stack = %v, want %v", err, ErrNothingToRedo)
					}
					continue
				}
				e := redoStack[len(redoStack)-1]
				redoStack = redoStack[:len(redoStack)-1]
				reference = spliceBytes(reference, int(e.start), len(e.deleted), e.inserted)
				undoStack = append(undoStack, e)
				if _, err := session.Redo(); err != nil {
					t.Fatalf("Redo: %v", err)
				}
				assertContent()
			case 3: // Save
				if _, err := session.Save(); err != nil {
					t.Fatalf("Save: %v", err)
				}
				committedReference = append(committedReference[:0], reference...)
				assertContent()
				if disk, err := os.ReadFile(path); err != nil || !bytes.Equal(disk, reference) {
					t.Fatalf("disk after save = %q (err %v), want %q", disk, err, reference)
				}
			case 4: // Piece/undo compaction + verify
				if _, err := session.Compact(context.Background(), CompactOptions{}); err != nil {
					t.Fatalf("Compact: %v", err)
				}
				assertContent()
			case 5: // Snapshot + verify
				_, lease, err := session.Snapshot()
				if err != nil {
					t.Fatalf("Snapshot: %v", err)
				}
				snap, err := io.ReadAll(io.NewSectionReader(lease, 0, lease.Len()))
				if err != nil {
					t.Fatalf("snapshot read: %v", err)
				}
				if err := lease.Close(); err != nil {
					t.Fatalf("lease close: %v", err)
				}
				if !bytes.Equal(snap, reference) {
					t.Fatalf("snapshot mismatch\n got: %q\nwant: %q", snap, reference)
				}
				assertContent()
			}
		}
		// The committed reference is always recoverable on disk.
		if disk, err := os.ReadFile(path); err != nil || !bytes.Equal(disk, committedReference) {
			t.Fatalf("final disk = %q (err %v), want committed %q", disk, err, committedReference)
		}
	})
}

// sessionEdit is one undo/redo unit in the Session state-fuzz reference model.
type sessionEdit struct {
	start             int64
	deleted, inserted []byte
}

// spliceBytes returns a new slice with base[start:start+delete] replaced by
// insert. It is the in-memory analogue of Session.ApplyBatch.
func spliceBytes(base []byte, start, delete int, insert []byte) []byte {
	result := make([]byte, 0, len(base)-delete+len(insert))
	result = append(result, base[:start]...)
	result = append(result, insert...)
	result = append(result, base[start+delete:]...)
	return result
}

// sessionContent reads the full current logical content of a session.
func sessionContent(t testing.TB, session *Session) []byte {
	t.Helper()
	length := session.Metadata().ByteLength
	buffer := make([]byte, length)
	n, err := session.ReadAt(buffer, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	return buffer[:n]
}

// sanitizeBase clamps the base content to maxBaseBytes, forces every byte into
// the ASCII range (so the file is always valid UTF-8), and removes a leading
// UTF-8 BOM so the reference model equals the raw bytes the session reports.
func sanitizeBase(base []byte, maxBaseBytes int) []byte {
	if len(base) > maxBaseBytes {
		base = base[:maxBaseBytes]
	}
	if len(base) >= 3 && bytes.Equal(base[:3], []byte{0xef, 0xbb, 0xbf}) {
		base[0] = 'x'
	}
	for i := range base {
		if base[i] >= 0x80 {
			base[i] = byte('a' + int(base[i])%26)
		}
	}
	return base
}
