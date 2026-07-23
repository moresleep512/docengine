package virtual

import (
	"container/list"
	"context"
	"errors"
	"math"
	"sync"
)

type resolvedOptions struct {
	targetPageBytes  int64
	maximumPageBytes int64
	maximumFragments int
	maximumTasks     int
	maximumKeyBytes  int64
	cacheBytes       int64
	window           Budget
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
	mu        sync.RWMutex
	source    Source
	release   func() error
	identity  *pagerIdentity
	revision  uint64
	length    int64
	options   resolvedOptions
	logical   []pageMeta
	state     *pagerState
	closed    bool
	closing   bool
	closeDone chan struct{}
	closeErr  error

	tasks chan struct{}

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
		options:  resolved, logical: logical,
		tasks:     make(chan struct{}, resolved.maximumTasks),
		cacheList: list.New(), cache: make(map[cacheKey]*list.Element),
		closeDone: make(chan struct{}),
	}
	pager.taskCond = sync.NewCond(&pager.mu)
	pager.state = &pagerState{pages: initial, fragmentByID: make(map[string]int)}
	return pager, nil
}

func resolveOptions(options Options) (resolvedOptions, error) {
	resolved := resolvedOptions{
		targetPageBytes: options.TargetPageBytes, maximumPageBytes: options.MaximumPageBytes,
		maximumFragments: options.MaximumFragments, maximumTasks: options.MaximumTasks,
		maximumKeyBytes: options.MaximumKeyBytes, cacheBytes: options.CacheBytes, window: options.Window,
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
	if resolved.targetPageBytes < 4 || resolved.maximumPageBytes < resolved.targetPageBytes ||
		resolved.maximumPageBytes > MaximumPageBytes ||
		resolved.maximumFragments < 1 || resolved.maximumFragments > MaximumFragments ||
		resolved.maximumTasks < 1 || resolved.maximumTasks > MaximumConcurrentTasks ||
		resolved.maximumKeyBytes < 1 || resolved.maximumKeyBytes > MaximumKeyBytes ||
		resolved.cacheBytes < 0 || resolved.cacheBytes > MaximumCacheBytes ||
		resolved.window.Bytes < 1 || resolved.window.Pages < 1 ||
		resolved.window.Fragments < 1 || resolved.window.Measure < 1 ||
		resolved.window.Bytes < resolved.maximumPageBytes {
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
	}
}

func (p *Pager) Close() error {
	p.mu.Lock()
	if p.closing {
		done := p.closeDone
		p.mu.Unlock()
		<-done
		p.mu.RLock()
		err := p.closeErr
		p.mu.RUnlock()
		return err
	}
	p.closing = true
	p.closed = true
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
	close(p.closeDone)
	p.mu.Unlock()
	return err
}

func (p *Pager) acquireTask(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	select {
	case p.tasks <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrBusy
	}
}

func (p *Pager) releaseTask() {
	<-p.tasks
	p.mu.Lock()
	p.taskCond.Broadcast()
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
