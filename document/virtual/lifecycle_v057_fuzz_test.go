package virtual

import (
	"context"
	"errors"
	"testing"
)

func FuzzPagerRefreshLifecycleStateMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 0, 3, 5})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 256 {
			return
		}
		var recorder progressRecorder
		pager, err := Build(context.Background(), testSource("abcd"), 5, Options{
			TargetPageBytes: 4, MaximumPageBytes: 4, Observer: &recorder,
		})
		if err != nil {
			t.Fatal(err)
		}
		generation := uint64(0)
		closed := false
		for _, operation := range operations {
			switch operation % 6 {
			case 0:
				stats, refreshErr := pager.Refresh(context.Background(), fragmentProviderFunc(
					func(_ context.Context, request FragmentRequest) (FragmentResult, error) {
						if err := request.Report(FragmentProgress{IndexedThrough: 2, Fragments: 1}); err != nil {
							return FragmentResult{}, err
						}
						return FragmentResult{
							IndexedThrough: 4, Complete: true,
							Fragments: []Fragment{{ID: "all", Start: 0, End: 4, Measure: 1}},
						}, nil
					},
				))
				if closed {
					if !errors.Is(refreshErr, ErrClosed) {
						t.Fatalf("closed Refresh = %v", refreshErr)
					}
					break
				}
				if refreshErr != nil {
					t.Fatal(refreshErr)
				}
				generation = stats.Generation
			case 1:
				_, refreshErr := pager.Refresh(context.Background(), fragmentProviderFunc(
					func(_ context.Context, request FragmentRequest) (FragmentResult, error) {
						_ = request.Report(FragmentProgress{IndexedThrough: 3, Fragments: 1})
						_ = request.Report(FragmentProgress{IndexedThrough: 2, Fragments: 1})
						return FragmentResult{
							IndexedThrough: 4, Complete: true,
							Fragments: []Fragment{{ID: "all", Start: 0, End: 4}},
						}, nil
					},
				))
				want := ErrInvalidPublication
				if closed {
					want = ErrClosed
				}
				if !errors.Is(refreshErr, want) {
					t.Fatalf("invalid Refresh = %v, want %v", refreshErr, want)
				}
			case 2:
				sentinel := errors.New("provider failure")
				_, refreshErr := pager.Refresh(context.Background(), fragmentProviderFunc(
					func(context.Context, FragmentRequest) (FragmentResult, error) {
						return FragmentResult{}, sentinel
					},
				))
				want := sentinel
				if closed {
					want = ErrClosed
				}
				if !errors.Is(refreshErr, want) {
					t.Fatalf("failed Refresh = %v, want %v", refreshErr, want)
				}
			case 3:
				stats, publishErr := pager.Publish(context.Background(), Publication{
					Revision: 5, BaseGeneration: generation, IndexedThrough: 4, Complete: true,
				})
				if closed {
					if !errors.Is(publishErr, ErrClosed) {
						t.Fatalf("closed Publish = %v", publishErr)
					}
					break
				}
				if publishErr != nil {
					t.Fatal(publishErr)
				}
				generation = stats.Generation
			case 4:
				stats := pager.Stats()
				if stats.Generation != generation || stats.Closing {
					t.Fatalf("Stats = %+v, generation=%d", stats, generation)
				}
				if stats.Closed != closed {
					t.Fatalf("closed Stats = %+v, want %v", stats, closed)
				}
			case 5:
				if err := pager.Close(); err != nil {
					t.Fatal(err)
				}
				closed = true
			}
		}
		if err := pager.Close(); err != nil {
			t.Fatal(err)
		}
		assertProgressStateMachine(t, recorder.snapshot())
	})
}

func assertProgressStateMachine(t testing.TB, progress []Progress) {
	t.Helper()
	var operationID uint64
	var stage ProgressStage
	for _, value := range progress {
		if value.OperationID != operationID {
			if value.OperationID <= operationID || value.Stage != ProgressStarted {
				t.Fatalf("operation transition = %+v after id=%d stage=%d", value, operationID, stage)
			}
			operationID = value.OperationID
			stage = ProgressStarted
			continue
		}
		if stage == ProgressCompleted || stage == ProgressFailed ||
			(value.Stage != ProgressAdvanced && value.Stage != ProgressCompleted && value.Stage != ProgressFailed) {
			t.Fatalf("stage transition = %d -> %+v", stage, value)
		}
		stage = value.Stage
	}
	if len(progress) != 0 && stage != ProgressCompleted && stage != ProgressFailed {
		t.Fatalf("unterminated progress operation %d at stage %d", operationID, stage)
	}
}
