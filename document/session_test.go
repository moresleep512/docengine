package document

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCommitSnapshotKeepsNewerEditsPending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.md")
	if err := os.WriteFile(path, []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 5, Insert: "A"}}); err != nil {
		t.Fatal(err)
	}
	started, proceed := make(chan struct{}), make(chan struct{})
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			close(started)
			<-proceed
		}
	}
	saved := make(chan error, 1)
	go func() {
		_, saveErr := session.Save()
		saved <- saveErr
	}()
	<-started
	if _, err := session.ApplyBatch(context.Background(), 1, []ReplaceOperation{{Start: 1, Insert: " two"}}); err != nil {
		t.Fatal(err)
	}
	close(proceed)
	if err := <-saved; err != nil {
		t.Fatal(err)
	}
	session.commitHook = nil
	if content, _ := os.ReadFile(path); string(content) != "A" {
		t.Fatalf("first generation wrote %q", content)
	}
	meta := session.Metadata()
	if !meta.Dirty || meta.CommittedRevision != 1 || meta.Revision != 2 {
		t.Fatalf("unexpected generation state: %+v", meta)
	}
	text, _ := readSession(session, meta.ByteLength)
	if string(text) != "A two" {
		t.Fatalf("newer edit was lost: %q", text)
	}
	if _, err := session.Save(); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(path); string(content) != "A two" {
		t.Fatalf("second generation wrote %q", content)
	}
}

func TestSavePreservesUndoAndDeletesCommittedWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "undo.md")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveryDir := filepath.Join(dir, "recovery")
	session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir, SessionDir: filepath.Join(dir, "session")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, DeleteLength: 6, Insert: "after"}}); err != nil {
		t.Fatal(err)
	}
	walPath := session.journal.Path()
	if _, err := session.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(walPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed WAL still exists: %v", err)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatalf("undo after save: %v", err)
	}
	text, _ := readSession(session, session.Metadata().ByteLength)
	if string(text) != "before" {
		t.Fatalf("undo restored %q", text)
	}
}

func TestCommitRejectsExternalChangeAtReplaceBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conflict.md")
	if err := os.WriteFile(path, []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{RecoveryDir: filepath.Join(dir, "recovery"), SessionDir: filepath.Join(dir, "session")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 4, Insert: " local"}}); err != nil {
		t.Fatal(err)
	}
	session.commitHook = func(stage string) {
		if stage == "snapshot" {
			if err := os.WriteFile(path, []byte("external content"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := session.Save(); !errors.Is(err, ErrExternalChange) {
		t.Fatalf("save error = %v", err)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "external content" {
		t.Fatalf("external content was overwritten: %q", content)
	}
}

func TestSessionEditUndoRecoveryAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, []byte("hello\r\nworld")...), 0o600); err != nil {
		t.Fatal(err)
	}
	options := OpenOptions{RecoveryDir: filepath.Join(dir, "recovery")}
	session, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	meta := session.Metadata()
	if !meta.HasBOM || meta.EOL != EOLCRLF || meta.ByteLength != 12 {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	result, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 7, DeleteLength: 5, Insert: "engine"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 1 {
		t.Fatalf("unexpected revision: %+v", result)
	}
	if _, err := session.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Redo(); err != nil {
		t.Fatal(err)
	}
	if err := session.journal.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Metadata().Recovered {
		t.Fatal("expected recovered session")
	}
	content, err := io.ReadAll(io.NewSectionReader(recovered, 0, recovered.Metadata().ByteLength))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello\r\nengine" {
		t.Fatalf("got %q", content)
	}
	if _, err := recovered.Save(); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != "\xef\xbb\xbfhello\r\nengine" {
		t.Fatalf("saved %q", saved)
	}
}

func TestSessionRejectsRevisionConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	_ = os.WriteFile(path, []byte("abc"), 0o600)
	session, err := Open(path, OpenOptions{RecoveryDir: filepath.Join(dir, "recovery")})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_, err = session.ApplyBatch(context.Background(), 99, []ReplaceOperation{{Start: 0, Insert: "x"}})
	if err != ErrRevisionConflict {
		t.Fatalf("got %v", err)
	}
}

func TestApplyBatchIsOneRecoverableUndoUnit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "batch.md")
	if err := os.WriteFile(path, []byte("alpha beta gamma"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := OpenOptions{RecoveryDir: filepath.Join(dir, "recovery")}
	session, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{
		{Start: 11, DeleteLength: 5, Insert: "G"},
		{Start: 0, DeleteLength: 5, Insert: "A"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := recovered.Undo(); err != nil {
		t.Fatal(err)
	}
	text, err := readSession(recovered, recovered.Metadata().ByteLength)
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != "alpha beta gamma" {
		t.Fatalf("undo restored %q", text)
	}
}

func TestOpenQuarantinesRecoveryLogAfterExternalFileChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "external.md")
	recoveryDir := filepath.Join(dir, "recovery")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Open(path, OpenOptions{RecoveryDir: recoveryDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ApplyBatch(context.Background(), 0, []ReplaceOperation{{Start: 0, Insert: "local "}}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external version"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, OpenOptions{RecoveryDir: recoveryDir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	text, _ := readSession(reopened, reopened.Metadata().ByteLength)
	if string(text) != "external version" {
		t.Fatalf("opened %q", text)
	}
	stale, _ := filepath.Glob(filepath.Join(recoveryDir, "*.stale-*"))
	if len(stale) != 1 {
		t.Fatalf("stale logs = %v", stale)
	}
}

func readSession(session *Session, length int64) ([]byte, error) {
	buffer := make([]byte, length)
	n, err := session.ReadAt(buffer, 0)
	if errors.Is(err, io.EOF) && n == len(buffer) {
		err = nil
	}
	return buffer[:n], err
}
