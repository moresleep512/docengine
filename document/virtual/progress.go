package virtual

import (
	"context"
	"errors"
	"math"
	"sync"
)

type progressOperation struct {
	mu           sync.Mutex
	observer     ProgressObserver
	ctx          context.Context
	value        Progress
	maxFragments int
	reporting    bool
	reportError  error
	finished     bool
}

func (p *Pager) startProgress(ctx context.Context, kind ProgressKind) (*progressOperation, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	if p.observer == nil && kind == ProgressPublish {
		p.mu.Unlock()
		return nil, nil
	}
	if p.nextOperationID == math.MaxUint64 {
		p.mu.Unlock()
		return nil, ErrOperationOverflow
	}
	p.nextOperationID++
	operation := &progressOperation{
		observer: p.observer,
		ctx:      ctx,
		value: Progress{
			OperationID: p.nextOperationID,
			Kind:        kind, Stage: ProgressStarted,
			Revision: p.revision, BaseGeneration: p.state.generation,
			ByteLength: p.length,
		},
		maxFragments: p.options.maximumFragments,
		reporting:    kind == ProgressRefresh,
	}
	p.mu.Unlock()
	operation.emitLocked()
	return operation, nil
}

func (o *progressOperation) report(progress FragmentProgress) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.reporting || o.finished {
		return ErrStaleGeneration
	}
	if o.reportError != nil {
		return o.reportError
	}
	if err := contextError(o.ctx); err != nil {
		o.reportError = err
		return err
	}
	if progress.IndexedThrough < o.value.IndexedThrough ||
		progress.IndexedThrough > o.value.ByteLength ||
		progress.Fragments < o.value.Fragments ||
		progress.Fragments > o.maxFragments {
		o.reportError = ErrInvalidPublication
		return o.reportError
	}
	if progress.IndexedThrough == o.value.IndexedThrough &&
		progress.Fragments == o.value.Fragments {
		return nil
	}
	o.value.Stage = ProgressAdvanced
	o.value.IndexedThrough = progress.IndexedThrough
	o.value.Fragments = progress.Fragments
	o.emitLocked()
	return nil
}

func (o *progressOperation) finishProvider(result FragmentResult, providerErr error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reporting = false
	if o.reportError != nil {
		return errors.Join(o.reportError, providerErr)
	}
	if providerErr != nil {
		return providerErr
	}
	if result.IndexedThrough < o.value.IndexedThrough ||
		len(result.Fragments) < o.value.Fragments {
		return ErrInvalidPublication
	}
	o.value.IndexedThrough = result.IndexedThrough
	o.value.Fragments = len(result.Fragments)
	o.value.Complete = result.Complete
	return nil
}

func (o *progressOperation) finish(stats Stats, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.finished {
		return
	}
	o.finished = true
	o.reporting = false
	if err != nil {
		o.value.Stage = ProgressFailed
		o.value.Cause = err
		o.emitLocked()
		return
	}
	o.value.Stage = ProgressCompleted
	o.value.PublishedGeneration = stats.Generation
	o.value.IndexedThrough = stats.IndexedThrough
	o.value.Fragments = stats.Fragments
	o.value.Complete = stats.Complete
	o.value.Published = true
	o.emitLocked()
}

func (o *progressOperation) emitLocked() {
	if o.observer != nil {
		o.observer.ObserveVirtualProgress(o.value)
	}
}
