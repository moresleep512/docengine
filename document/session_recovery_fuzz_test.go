package document

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// FuzzSessionCrashRecovery randomizes the crash-recovery surface: it applies a
// sequence of edits to a session, closes it WITHOUT saving (simulating a crash
// with a live WAL), reopens, and verifies the recovered content and Recovered
// flag. It then corrupts the journal tail at a fuzz-derived offset and reopens
// again to verify RepairTail lands on a valid batch boundary and the surviving
// content equals the last fully-committed-batch content. This complements the
// deterministic recovery tests, which exercise one crash point at a time.
func FuzzSessionCrashRecovery(f *testing.F) {
	f.Add([]byte("base"), []byte{0, 3, 0, 1, 0, 2, 0, 0, 1, 50})
	f.Add([]byte("abc"), []byte{0, 0, 1, 0, 1, 0, 2, 200})
	f.Add([]byte("hello world"), []byte{0, 0, 0, 0, 1, 0, 0, 2, 100, 0, 0, 0, 0, 0, 1, 0, 0, 0, 250})
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

		// Apply edits from the program (3 bytes each) WITHOUT saving, so the
		// WAL accumulates uncommitted batches. Track the reference content and
		// the content after each batch, so a later tail corruption can be checked
		// against a known set of intermediate states.
		session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "s1")})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		reference := append([]byte(nil), base...)
		states := [][]byte{append([]byte(nil), base...)} // states[k] = content after k batches
		pos := 0
		for pos+3 <= len(program) {
			sb := program[pos]
			db := program[pos+1]
			ib := program[pos+2]
			pos += 3
			start := int64(sb) % (int64(len(reference)) + 1)
			maxDelete := int64(len(reference)) - start
			deleteLength := int64(db) % (maxDelete + 1)
			insertLength := int64(ib) % 6
			if remain := int64(maxDocument) - (int64(len(reference)) - deleteLength); insertLength > remain {
				insertLength = remain
			}
			if insertLength < 0 {
				insertLength = 0
			}
			insert := make([]byte, insertLength)
			for i := range insert {
				insert[i] = byte('a' + (int(ib)+i)%26)
			}
			meta := session.Metadata()
			if _, err := session.ApplyBatch(context.Background(), meta.Revision, []ReplaceOperation{{
				Start: start, DeleteLength: deleteLength, Insert: string(insert),
			}}); err != nil {
				t.Fatalf("ApplyBatch at %d: %v", pos, err)
			}
			reference = spliceBytes(reference, int(start), int(deleteLength), insert)
			states = append(states, append([]byte(nil), reference...))
		}
		appliedBatches := len(states) - 1
		// Close without Save: the journal holds appliedBatches uncommitted
		// batches. Close drains the syncLoop but does NOT commit.
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}

		// Reopen 1: recovery must replay every batch and yield the reference.
		reopened, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "s2")})
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		if appliedBatches > 0 {
			if !reopened.Metadata().Recovered {
				t.Fatalf("reopen not recovered despite %d batches: %+v", appliedBatches, reopened.Metadata())
			}
			if !bytesEqual(sessionContent(t, reopened), reference) {
				t.Fatalf("recovered content mismatch\n got: %q\nwant: %q", sessionContent(t, reopened), reference)
			}
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}

		// Corrupt the journal tail at a fuzz-derived offset and reopen. The
		// recovery must truncate at the last valid batch boundary and replay a
		// strict prefix of the batches; the surviving content must equal the
		// content after some number of complete batches (a member of states).
		journals, err := filepath.Glob(filepath.Join(recoveryDir, "*.docengine-journal-v2"))
		if err != nil || len(journals) != 1 || appliedBatches == 0 {
			return // no journal to corrupt
		}
		journalPath := journals[0]
		content, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		// Flip a byte at cutOffset >= fileHeaderSize so the file header (and
		// thus the fingerprint/quarantine path) is untouched; the containing
		// batch fails its CRC and is dropped.
		cutOffset := len(content) / 2
		if pos < len(program) && len(content) > 96 {
			cutOffset = 96 + int(program[pos])%(len(content)-96)
		}
		if cutOffset < 96 {
			cutOffset = 96
		}
		if cutOffset >= len(content) {
			cutOffset = len(content) - 1
		}
		corrupted := append([]byte(nil), content...)
		corrupted[cutOffset] ^= 0xff
		if err := os.WriteFile(journalPath, corrupted, 0o600); err != nil {
			t.Fatal(err)
		}
		corruptedReopen, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "s3")})
		if err != nil {
			// A corruption that breaks the file header (not reachable here
			// since cutOffset >= 96) would quarantine; be defensive anyway.
			return
		}
		corruptedContent := sessionContent(t, corruptedReopen)
		// The truncated replay exposes a strict prefix of the batches, so the
		// content must equal one of states[0..appliedBatches-1] (never the full
		// reference, since at least one batch is dropped).
		matched := false
		for k := 0; k < appliedBatches; k++ {
			if bytesEqual(corruptedContent, states[k]) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("corrupted content %q is not any intermediate state (cutOffset=%d)", corruptedContent, cutOffset)
		}
		if err := corruptedReopen.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
