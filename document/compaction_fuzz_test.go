package document

import (
	"context"
	"errors"
	"os"
	"testing"
)

func FuzzUndoStoreRewriteAtomicity(f *testing.F) {
	for _, seed := range [][]byte{
		{0},
		{1, 'a'},
		{2, 'a', 'b', 'a'},
		{0, 0, 1, 2, 3, 4, 5},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64 {
			input = input[:64]
		}
		dir := t.TempDir()
		store, err := openUndoStore(dir, DefaultUndoBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer store.close()
		if _, err := store.append([]byte("dead")); err != nil {
			t.Fatal(err)
		}
		refs := make([]textRef, 0, len(input))
		values := make(map[textRef]string, len(input))
		for _, value := range input {
			ref, err := store.append([]byte{value})
			if err != nil {
				t.Fatal(err)
			}
			refs = append(refs, ref)
			values[ref] = string([]byte{value})
		}
		selected := make([]textRef, 0, len(input)+1)
		for index, value := range input {
			if len(refs) > 0 && value&1 != 0 {
				selected = append(selected, refs[(index+int(value))%len(refs)])
			}
			if value&2 != 0 {
				selected = append(selected, textRef{})
			}
		}
		if len(selected) > 0 {
			selected = append(selected, selected[0])
		}

		beforePath, beforeBytes := store.path, store.bytes()
		ctx, cancel := context.WithCancel(context.Background())
		cancelMode := byte(0)
		if len(input) > 0 {
			cancelMode = input[0] % 3
		}
		canceled := false
		mapping, rewriteErr := store.rewriteContext(ctx, selected, func(completed, total int64) {
			if (cancelMode == 1 && completed == 0) || (cancelMode == 2 && completed > 0) {
				canceled = true
				cancel()
			}
		})
		if canceled {
			if mapping != nil || !errors.Is(rewriteErr, context.Canceled) {
				t.Fatalf("canceled rewrite = (%+v, %v)", mapping, rewriteErr)
			}
			if store.path != beforePath || store.bytes() != beforeBytes {
				t.Fatalf("canceled rewrite changed active store: path=%q bytes=%d", store.path, store.bytes())
			}
			for ref, want := range values {
				if got, err := store.read(ref); err != nil || got != want {
					t.Fatalf("canceled rewrite corrupted %v = (%q, %v), want %q", ref, got, err, want)
				}
			}
			return
		}
		if rewriteErr != nil || mapping == nil {
			t.Fatalf("committed rewrite = (%+v, %v)", mapping, rewriteErr)
		}
		unique := make(map[textRef]struct{})
		var liveBytes int64
		for _, ref := range selected {
			if ref.length == 0 {
				continue
			}
			if _, ok := unique[ref]; ok {
				continue
			}
			unique[ref] = struct{}{}
			liveBytes += ref.length
			mapped, ok := mapping[ref]
			if !ok {
				t.Fatalf("missing mapping for %v", ref)
			}
			if got, err := store.read(mapped); err != nil || got != values[ref] {
				t.Fatalf("mapped content %v = (%q, %v), want %q", mapped, got, err, values[ref])
			}
		}
		if len(mapping) != len(unique) || store.bytes() != liveBytes {
			t.Fatalf("mapping/store size = (%d, %d), want (%d, %d)", len(mapping), store.bytes(), len(unique), liveBytes)
		}
		if beforePath != store.path {
			if _, err := os.Stat(beforePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retired active store remains: %v", err)
			}
		}
	})
}
