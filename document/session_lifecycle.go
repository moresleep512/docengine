package document

import (
	"context"
	"fmt"
	"io"
)

// LifecycleStats returns bounded host-resource and shutdown state without
// touching the filesystem. It remains available after Close.
func (s *Session) LifecycleStats() LifecycleStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	closing := context.Cause(s.lifecycleContext) != nil && !s.closeCompleted
	return LifecycleStats{
		ActiveSnapshotLeases:       s.activeSnapshotLeases,
		PeakSnapshotLeases:         s.peakSnapshotLeases,
		MaxSnapshotLeases:          s.config.Limits.MaxSnapshotLeases,
		WaitingSaves:               s.waitingSaves,
		SaveActive:                 s.saveActive,
		AutomaticCheckpointPending: s.automaticCheckpointPending,
		Closing:                    closing,
		Closed:                     s.closeCompleted,
	}
}

func (s *Session) operationContext(parent context.Context) (context.Context, func(), error) {
	if parent == nil {
		return nil, nil, ErrInvalidContext
	}
	if context.Cause(s.lifecycleContext) != nil {
		return nil, nil, ErrClosed
	}
	merged, cancel := context.WithCancelCause(parent)
	ctx := &sessionOperationContext{Context: merged, parent: parent, lifecycle: s.lifecycleContext}
	stop := context.AfterFunc(s.lifecycleContext, func() { cancel(ErrClosed) })
	if context.Cause(s.lifecycleContext) != nil {
		cancel(ErrClosed)
	}
	return ctx, func() {
		stop()
		cancel(nil)
	}, nil
}

// sessionOperationContext preserves direct Err polling for valid custom
// Context implementations whose Done channel is nil, while the embedded
// derived context still merges ordinary parent cancellation with Session
// shutdown.
type sessionOperationContext struct {
	context.Context
	parent    context.Context
	lifecycle context.Context
}

func (c *sessionOperationContext) Err() error {
	if err := c.parent.Err(); err != nil {
		return err
	}
	if context.Cause(c.lifecycle) != nil {
		return ErrClosed
	}
	return c.Context.Err()
}

func contextError(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

func (s *Session) acquireSave(ctx context.Context) error {
	s.mu.Lock()
	if s.closed || context.Cause(s.lifecycleContext) != nil {
		s.mu.Unlock()
		return ErrClosed
	}
	s.waitingSaves++
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		s.mu.Lock()
		s.waitingSaves--
		s.mu.Unlock()
		return contextError(ctx)
	case <-s.lifecycleContext.Done():
		s.mu.Lock()
		s.waitingSaves--
		s.mu.Unlock()
		return ErrClosed
	case <-s.saveGate:
	}

	s.mu.Lock()
	s.waitingSaves--
	if s.closed || context.Cause(s.lifecycleContext) != nil {
		s.mu.Unlock()
		s.saveGate <- struct{}{}
		return ErrClosed
	}
	if err := contextError(ctx); err != nil {
		s.mu.Unlock()
		s.saveGate <- struct{}{}
		return err
	}
	s.saveActive = true
	s.mu.Unlock()
	return nil
}

func (s *Session) releaseSave() {
	s.mu.Lock()
	s.saveActive = false
	s.mu.Unlock()
	s.saveGate <- struct{}{}
}

func (s *Session) snapshotLocked() (uint64, SnapshotLease, error) {
	if s.closed || context.Cause(s.lifecycleContext) != nil {
		return 0, nil, ErrClosed
	}
	maximum := s.config.Limits.MaxSnapshotLeases
	if s.activeSnapshotLeases >= maximum {
		return 0, nil, fmt.Errorf("%w: active Snapshot leases are %d, maximum is %d",
			ErrLimitExceeded, s.activeSnapshotLeases, maximum)
	}
	lease := s.generation.acquire(s.tree.Snapshot())
	s.activeSnapshotLeases++
	if s.activeSnapshotLeases > s.peakSnapshotLeases {
		s.peakSnapshotLeases = s.activeSnapshotLeases
	}
	return s.revision, &trackedSnapshotLease{lease: lease, session: s}, nil
}

func (s *Session) releaseSnapshotLease() {
	s.mu.Lock()
	if s.activeSnapshotLeases > 0 {
		s.activeSnapshotLeases--
	}
	s.mu.Unlock()
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(value []byte) (int, error) {
	if err := contextError(w.ctx); err != nil {
		return 0, err
	}
	return w.writer.Write(value)
}
