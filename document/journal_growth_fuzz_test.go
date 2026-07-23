package document

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// FuzzSessionJournalQuotaIsAtomic compares journal quota failures with a byte
// slice model. A rejected batch must publish no revision, content, or physical
// journal growth, and every accepted prefix must survive a reopen.
func FuzzSessionJournalQuotaIsAtomic(f *testing.F) {
	f.Add([]byte("abc"), []byte{0, 0, 'x', 1, 1, 'y', 2, 0, 'z'}, uint16(256))
	f.Add([]byte{}, []byte{0, 0, 'a'}, uint16(0))
	f.Fuzz(func(t *testing.T, base, program []byte, extraLimit uint16) {
		base = sanitizeBase(base, 128)
		if len(program) > 768 {
			program = program[:768]
		}
		limit := MinimumJournalBytes + int64(extraLimit%4_096)
		dir := t.TempDir()
		path := filepath.Join(dir, "doc")
		recoveryDir := filepath.Join(dir, "recovery")
		if err := os.WriteFile(path, base, 0o600); err != nil {
			t.Fatal(err)
		}
		options := OpenOptions{
			RecoveryDir: recoveryDir,
			SessionDir:  filepath.Join(dir, "session-1"),
			Limits:      SessionLimits{MaxJournalBytes: limit},
		}
		session, err := Open(path, options)
		if err != nil {
			t.Fatal(err)
		}
		reference := append([]byte(nil), base...)
		revision := uint64(0)
		for offset := 0; offset+3 <= len(program); offset += 3 {
			start := int64(program[offset]) % (int64(len(reference)) + 1)
			maxDelete := int64(len(reference)) - start
			deleteLength := int64(program[offset+1]) % (maxDelete + 1)
			insert := []byte{'a' + program[offset+2]%26}
			beforeMetadata := session.Metadata()
			beforeStats := session.RecoveryStats()
			beforeContent := compactSessionContent(t, session)
			result, applyErr := session.ApplyBatch(context.Background(), revision, []ReplaceOperation{{
				Start: start, DeleteLength: deleteLength, Insert: string(insert),
			}})
			if errors.Is(applyErr, ErrLimitExceeded) {
				if session.Metadata() != beforeMetadata || session.RecoveryStats() != beforeStats ||
					compactSessionContent(t, session) != beforeContent {
					t.Fatalf("quota rejection changed state: metadata=%+v stats=%+v content=%q",
						session.Metadata(), session.RecoveryStats(), compactSessionContent(t, session))
				}
				break
			}
			if applyErr != nil {
				t.Fatalf("ApplyBatch = %v", applyErr)
			}
			reference = spliceBytes(reference, int(start), int(deleteLength), insert)
			revision++
			if result.Revision != revision || session.RecoveryStats().JournalBytes > limit {
				t.Fatalf("accepted state = result=%+v stats=%+v limit=%d", result, session.RecoveryStats(), limit)
			}
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		options.SessionDir = filepath.Join(dir, "session-2")
		reopened, err := Open(path, options)
		if err != nil {
			t.Fatal(err)
		}
		if got := []byte(compactSessionContent(t, reopened)); !bytesEqual(got, reference) {
			_ = reopened.Close()
			t.Fatalf("recovered content = %q, want %q", got, reference)
		}
		if _, err := reopened.Save(); err != nil {
			_ = reopened.Close()
			t.Fatal(err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
