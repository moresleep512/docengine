package virtual

import (
	"container/list"
	"context"
	"errors"
	"math"
	"sync"
)

type resolvedOptions struct {
	targetPageBytes      int64
	maximumPageBytes     int64
	maximumFragments     int
	maximumTasks         int
	maximumKeyBytes      int64
	cacheBytes           int64
	maximumInflightBytes int64
	window               Budget
	lineage              *Lineage
	observer             ProgressObserver
}

type pageMeta struct {
	start, end                  int64
	startLine, endLine          int64
	continuesFrom, continuesTo  bool
	endsWithLF                  bool
	fragment                    int
	continuation, continuations int
	measureStart, measureEnd    Measure
	indexed                     bool
}

type fragmentMeta struct {
	Fragment
	measureStart Measure
	pageFirst    int
	pageLast     int
}

type pagerState struct {
	generation     uint64
	pages          []pageMeta
	fragments      []fragmentMeta
	fragmentByID   map[string]int
	indexedThrough int64
	complete       bool
	totalMeasure   Measure
	keyBytes       int64
}

type cacheKey struct {
	generation uint64
	index      int
	start, end int64
}

type cacheEntry struct {
	key  cacheKey
	data []byte
}

// Pager is permanently bound to one immutable Source revision. Fragment
// publication changes only its independent generation.
type Pager struct {
	mu               sync.RWMutex
	source           Source
	release          func() error
	identity         *pagerIdentity
	lineage          *Lineage
	observer         ProgressObserver
	revision         uint64
	length           int64
	options          resolvedOptions
	logical          []pageMeta
	state            *pagerState
	closed           bool
	closing          bool
	closeCompleted   bool
	closeDone        chan struct{}
	closeErr         error
	lifecycleContext context.Context
	cancelLifecycle  context.CancelCauseFunc
	nextOperationID  uint64

	tasks               chan struct{}
	activeInflightBytes int64

	cacheList  *list.List
	cache      map[cacheKey]*list.Element
	cacheBytes int64
	taskCond   *sync.Cond
}

func Build(ctx context.Context, source Source, revision uint64, options Options) (*Pager, error) {
	return build(ctx, source, revision, options, nil)
}

func BuildOwned(ctx context.Context, source OwnedSource, revision uint64, options Options) (*Pager, error) {
	if source == nil {
		return nil, ErrInvalidSource
	}
	pager, err := build(ctx, source, revision, options, source.Close)
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return pager, nil
}

func build(ctx context.Context, source Source, revision uint64, options Options, release func() error) (*Pager, error) {
	if ctx == nil {
		return nil, ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrInvalidSource
	}
	length := source.Len()
	if length < 0 {
		return nil, ErrInvalidSource
	}
	resolved, err := resolveOptions(options)
	if err != nil {
		return nil, err
	}
	logical, err := buildLogicalPages(ctx, source, length, resolved)
	if err != nil {
		return nil, err
	}
	if source.Len() != length {
		return nil, ErrSourceInconsistent
	}
	initial := append([]pageMeta(nil), logical...)
	pager := &Pager{
		source: source, release: release, revision: revision, length: length,
		identity: &pagerIdentity{},
		lineage:  resolved.lineage, observer: resolved.observer,
		options: resolved, logical: logical,
		tasks:     make(chan struct{}, resolved.maximumTasks),
		cacheList: list.New(), cache: make(map[cacheKey]*list.Element),
		closeDone: make(chan struct{}),
	}
	pager.lifecycleContext, pager.cancelLifecycle = context.WithCancelCause(context.Background())
	pager.taskCond = sync.NewCond(&pager.mu)
	pager.state = &pagerState{pages: initial, fragmentByID: make(map[string]int)}
	return pager, nil
}

func resolveOptions(options Options) (resolvedOptions, error) {
	resolved := resolvedOptions{
		targetPageBytes: options.TargetPageBytes, maximumPageBytes: options.MaximumPageBytes,
		maximumFragments: options.MaximumFragments, maximumTasks: options.MaximumTasks,
		maximumKeyBytes: options.MaximumKeyBytes, cacheBytes: options.CacheBytes,
		maximumInflightBytes: options.MaximumInflightBytes, window: options.Window,
		lineage: options.Lineage, observer: options.Observer,
	}
	if resolved.targetPageBytes == 0 {
		resolved.targetPageBytes = DefaultTargetPageBytes
	}
	if resolved.maximumPageBytes == 0 {
		resolved.maximumPageBytes = DefaultMaximumPageBytes
	}
	if resolved.maximumFragments == 0 {
		resolved.maximumFragments = DefaultMaximumFragments
	}
	if resolved.maximumTasks == 0 {
		resolved.maximumTasks = DefaultMaximumConcurrentTasks
	}
	if resolved.maximumKeyBytes == 0 {
		resolved.maximumKeyBytes = DefaultMaximumKeyBytes
	}
	if options.DisableCache {
		if options.CacheBytes != 0 {
			return resolvedOptions{}, ErrInvalidOptions
		}
		resolved.cacheBytes = 0
	} else if resolved.cacheBytes == 0 {
		resolved.cacheBytes = DefaultCacheBytes
	}
	if resolved.window.Bytes == 0 {
		resolved.window.Bytes = DefaultWindowBytes
	}
	if resolved.window.Pages == 0 {
		resolved.window.Pages = DefaultWindowPages
	}
	if resolved.window.Fragments == 0 {
		resolved.window.Fragments = DefaultWindowFragments
	}
	if resolved.window.Measure == 0 {
		resolved.window.Measure = DefaultWindowMeasure
	}
	if resolved.maximumInflightBytes == 0 {
		resolved.maximumInflightBytes = max(DefaultMaximumInflightBytes, resolved.window.Bytes)
	}
	if resolved.targetPageBytes < 4 || resolved.maximumPageBytes < resolved.targetPageBytes ||
		resolved.maximumPageBytes > MaximumPageBytes ||
		resolved.maximumFragments < 1 || resolved.maximumFragments > MaximumFragments ||
		resolved.maximumTasks < 1 || resolved.maximumTasks > MaximumConcurrentTasks ||
		resolved.maximumKeyBytes < 1 || resolved.maximumKeyBytes > MaximumKeyBytes ||
		resolved.cacheBytes < 0 || resolved.cacheBytes > MaximumCacheBytes ||
		resolved.window.Bytes < 1 || resolved.window.Pages < 1 ||
		resolved.window.Fragments < 1 || resolved.window.Measure < 1 ||
		resolved.window.Bytes > MaximumWindowBytes ||
		resolved.window.Pages > MaximumWindowPages ||
		resolved.window.Fragments > MaximumWindowFragments ||
		resolved.window.Measure > MaximumWindowMeasure ||
		resolved.window.Bytes < resolved.maximumPageBytes ||
		resolved.maximumInflightBytes < resolved.window.Bytes ||
		resolved.maximumInflightBytes > MaximumInflightBytes {
		return resolvedOptions{}, ErrInvalidOptions
	}
	return resolved, nil
}

func (p *Pager) Stats() Stats {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.statsLocked()
}

func (p *Pager) statsLocked() Stats {
	state := p.state
	return Stats{
		Revision: p.revision, Generation: state.generation, ByteLength: p.length,
		LogicalPages: len(p.logical), Pages: len(state.pages), Fragments: len(state.fragments),
		IndexedThrough: state.indexedThrough, Complete: state.complete, TotalMeasure: state.totalMeasure,
		TargetPageBytes: p.options.targetPageBytes, MaximumPageBytes: p.options.maximumPageBytes,
		CacheBytes: p.cacheBytes, CacheEntries: len(p.cache), MaximumCacheBytes: p.options.cacheBytes,
		ActiveTasks: len(p.tasks), MaximumTasks: cap(p.tasks),
		MaximumKeyBytes: p.options.maximumKeyBytes,
		KeyBytes:        state.keyBytes,
		WindowBytes:     p.options.window.Bytes, WindowPages: p.options.window.Pages,
		WindowFragments: p.options.window.Fragments, WindowMeasure: p.options.window.Measure,
		ActiveInflightBytes:  p.activeInflightBytes,
		MaximumInflightBytes: p.options.maximumInflightBytes,
		Closing:              p.closing && !p.closeCompleted, Closed: p.closeCompleted,
	}
}

func (p *Pager) Close() error {
	return p.CloseContext(context.Background())
}

// CloseContext starts one shared shutdown and waits for all admitted tasks.
// Closing cancels their derived Contexts. If ctx expires, cleanup continues
// and a later Close observes the same result.
func (p *Pager) CloseContext(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidContext
	}
	select {
	case <-p.closeDone:
		p.mu.RLock()
		err := p.closeErr
		p.mu.RUnlock()
		return err
	default:
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	if !p.closing {
		p.closing = true
		p.closed = true
		p.cancelLifecycle(ErrClosed)
		go p.finishClose()
	}
	done := p.closeDone
	p.mu.Unlock()
	select {
	case <-done:
		p.mu.RLock()
		err := p.closeErr
		p.mu.RUnlock()
		return err
	case <-ctx.Done():
		return contextError(ctx)
	}
}

func (p *Pager) finishClose() {
	p.mu.Lock()
	for len(p.tasks) != 0 {
		p.taskCond.Wait()
	}
	p.cache = nil
	p.cacheList.Init()
	p.cacheBytes = 0
	release := p.release
	p.release = nil
	p.mu.Unlock()
	var err error
	if release != nil {
		err = release()
	}
	p.mu.Lock()
	p.closeErr = err
	p.closeCompleted = true
	close(p.closeDone)
	p.mu.Unlock()
}

func (p *Pager) acquireTask(parent context.Context) (context.Context, func(), error) {
	if parent == nil {
		return nil, nil, ErrInvalidContext
	}
	if err := contextError(parent); err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, ErrClosed
	}
	if err := contextError(parent); err != nil {
		p.mu.Unlock()
		return nil, nil, err
	}
	select {
	case p.tasks <- struct{}{}:
	default:
		p.mu.Unlock()
		return nil, nil, ErrBusy
	}
	merged, cancel := context.WithCancelCause(parent)
	taskContext := &pagerTaskContext{Context: merged, parent: parent, lifecycle: p.lifecycleContext}
	stop := context.AfterFunc(p.lifecycleContext, func() { cancel(ErrClosed) })
	p.mu.Unlock()
	return taskContext, func() {
		stop()
		cancel(nil)
		p.releaseTask()
	}, nil
}

func (p *Pager) releaseTask() {
	<-p.tasks
	p.mu.Lock()
	p.taskCond.Broadcast()
	p.mu.Unlock()
}

type pagerTaskContext struct {
	context.Context
	parent    context.Context
	lifecycle context.Context
}

func (c *pagerTaskContext) Err() error {
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

func (p *Pager) reserveInflight(bytes int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if bytes < 0 || bytes > p.options.maximumInflightBytes-p.activeInflightBytes {
		return ErrBusy
	}
	p.activeInflightBytes += bytes
	return nil
}

func (p *Pager) releaseInflight(bytes int64) {
	p.mu.Lock()
	p.activeInflightBytes -= bytes
	p.mu.Unlock()
}

func (p *Pager) capture(revision, generation uint64) (*pagerState, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return nil, ErrClosed
	}
	if revision != p.revision {
		return nil, ErrRevisionMismatch
	}
	if generation != p.state.generation {
		return nil, ErrStaleGeneration
	}
	return p.state, nil
}

func (p *Pager) stateStillCurrent(state *pagerState) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	if p.state != state {
		return ErrStaleGeneration
	}
	return nil
}

func (p *Pager) resolveBudget(budget Budget) (Budget, error) {
	if budget.Bytes == 0 {
		budget.Bytes = p.options.window.Bytes
	}
	if budget.Pages == 0 {
		budget.Pages = p.options.window.Pages
	}
	if budget.Fragments == 0 {
		budget.Fragments = p.options.window.Fragments
	}
	if budget.Measure == 0 {
		budget.Measure = p.options.window.Measure
	}
	if budget.Bytes < 1 || budget.Pages < 1 || budget.Fragments < 1 || budget.Measure < 1 ||
		budget.Bytes > p.options.window.Bytes || budget.Pages > p.options.window.Pages ||
		budget.Fragments > p.options.window.Fragments || budget.Measure > p.options.window.Measure {
		return Budget{}, ErrInvalidRequest
	}
	return budget, nil
}

func checkedAddMeasure(left, right Measure) (Measure, bool) {
	if left < 0 || right < 0 || left > Measure(math.MaxInt64)-right {
		return 0, false
	}
	return left + right, true
}

// BelongsTo reports whether this Pager was built or rebuilt with lineage. It
// remains available after Close and does not expose the stored token.
func (p *Pager) BelongsTo(lineage *Lineage) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return lineage != nil && p.lineage == lineage
}

func (p *Pager) rebuildOptions() (Options, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return Options{}, ErrClosed
	}
	return Options{
		TargetPageBytes:      p.options.targetPageBytes,
		MaximumPageBytes:     p.options.maximumPageBytes,
		MaximumFragments:     p.options.maximumFragments,
		MaximumTasks:         p.options.maximumTasks,
		MaximumKeyBytes:      p.options.maximumKeyBytes,
		CacheBytes:           p.options.cacheBytes,
		MaximumInflightBytes: p.options.maximumInflightBytes,
		Window:               p.options.window,
		DisableCache:         p.options.cacheBytes == 0,
		Lineage:              p.lineage,
		Observer:             p.observer,
	}, nil
}

// Rebuild creates a new revision-bound Pager with the previous Pager's exact
// resource policy and lineage. provider may be nil to retain logical fallback.
func Rebuild(ctx context.Context, source Source, revision uint64, previous *Pager, provider FragmentProvider) (*Pager, error) {
	if previous == nil {
		return nil, ErrInvalidPager
	}
	options, err := previous.rebuildOptions()
	if err != nil {
		return nil, err
	}
	pager, err := Build(ctx, source, revision, options)
	if err != nil {
		return nil, err
	}
	return finishRebuild(ctx, pager, provider)
}

// RebuildOwned is Rebuild with Source ownership transferred to the result. The
// Source is closed on every failure.
func RebuildOwned(ctx context.Context, source OwnedSource, revision uint64, previous *Pager, provider FragmentProvider) (*Pager, error) {
	if source == nil {
		return nil, ErrInvalidSource
	}
	if previous == nil {
		return nil, errors.Join(ErrInvalidPager, source.Close())
	}
	options, err := previous.rebuildOptions()
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}
	pager, err := BuildOwned(ctx, source, revision, options)
	if err != nil {
		return nil, err
	}
	return finishRebuild(ctx, pager, provider)
}

func finishRebuild(ctx context.Context, pager *Pager, provider FragmentProvider) (*Pager, error) {
	if provider == nil {
		return pager, nil
	}
	if _, err := pager.Refresh(ctx, provider); err != nil {
		return nil, errors.Join(err, pager.Close())
	}
	return pager, nil
}
