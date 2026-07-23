package virtual

import (
	"context"
	"math"
	"sort"
)

// ReadPage reads one exact current Page. Keys from an older Fragment
// generation are rejected.
func (p *Pager) ReadPage(ctx context.Context, key PageKey) (Page, error) {
	if err := p.acquireTask(ctx); err != nil {
		return Page{}, err
	}
	defer p.releaseTask()
	state, err := p.capture(key.Revision, key.Generation)
	if err != nil {
		return Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if key.identity != p.identity {
		return Page{}, ErrInvalidRequest
	}
	if key.Index < 0 || key.Index >= len(state.pages) {
		return Page{}, ErrInvalidRequest
	}
	meta := state.pages[key.Index]
	if key.Start != meta.start || key.End != meta.end {
		return Page{}, ErrInvalidRequest
	}
	page, err := p.readMeta(ctx, state, key.Index)
	if err != nil {
		return Page{}, err
	}
	if err := p.stateStillCurrent(state); err != nil {
		return Page{}, err
	}
	return page, nil
}

func (p *Pager) WindowByByte(ctx context.Context, request ByteWindowRequest) (Window, error) {
	if err := p.acquireTask(ctx); err != nil {
		return Window{}, err
	}
	defer p.releaseTask()
	if request.Offset < 0 || request.Before < 0 || request.After < 0 {
		return Window{}, ErrInvalidRequest
	}
	state, err := p.capture(request.Revision, request.Generation)
	if err != nil {
		return Window{}, err
	}
	if request.Offset > p.length {
		return Window{}, ErrInvalidRequest
	}
	budget, err := p.resolveBudget(request.Budget)
	if err != nil {
		return Window{}, err
	}
	anchor := pageAtByte(state.pages, request.Offset, p.length)
	first, last := anchor, anchor
	if request.Before > 0 {
		start := request.Offset - min(request.Offset, request.Before)
		first = pageAtByte(state.pages, start, p.length)
	}
	if request.After > 0 {
		end := p.length
		if request.Offset <= math.MaxInt64-request.After && request.Offset+request.After < end {
			end = request.Offset + request.After
		}
		if end > request.Offset {
			last = pageBeforeByte(state.pages, end, p.length)
		}
	}
	return p.readWindow(ctx, state, anchor, anchor, first, last, budget)
}

func (p *Pager) WindowByFragment(ctx context.Context, request FragmentWindowRequest) (Window, error) {
	if err := p.acquireTask(ctx); err != nil {
		return Window{}, err
	}
	defer p.releaseTask()
	if request.ID == "" || request.Continuation < 0 || request.Before < 0 || request.After < 0 {
		return Window{}, ErrInvalidRequest
	}
	state, err := p.capture(request.Revision, request.Generation)
	if err != nil {
		return Window{}, err
	}
	fragmentIndex, exists := state.fragmentByID[request.ID]
	if !exists {
		return Window{}, ErrNotFound
	}
	budget, err := p.resolveBudget(request.Budget)
	if err != nil {
		return Window{}, err
	}
	firstFragment := fragmentIndex - min(fragmentIndex, request.Before)
	lastFragment := len(state.fragments) - 1
	if request.After < lastFragment-fragmentIndex {
		lastFragment = fragmentIndex + request.After
	}
	target := state.fragments[fragmentIndex]
	if request.Continuation >= target.pageLast-target.pageFirst+1 {
		return Window{}, ErrInvalidRequest
	}
	anchor := target.pageFirst + request.Continuation
	return p.readWindow(
		ctx, state, anchor, anchor,
		state.fragments[firstFragment].pageFirst, state.fragments[lastFragment].pageLast,
		budget,
	)
}

func (p *Pager) WindowByMeasure(ctx context.Context, request MeasureWindowRequest) (Window, error) {
	if err := p.acquireTask(ctx); err != nil {
		return Window{}, err
	}
	defer p.releaseTask()
	if request.Offset < 0 || request.Before < 0 || request.After < 0 ||
		(request.Affinity != AffinityBefore && request.Affinity != AffinityAfter) {
		return Window{}, ErrInvalidRequest
	}
	state, err := p.capture(request.Revision, request.Generation)
	if err != nil {
		return Window{}, err
	}
	if len(state.fragments) == 0 {
		return Window{}, ErrMeasureUnavailable
	}
	if request.Offset > state.totalMeasure {
		return Window{}, ErrInvalidRequest
	}
	budget, err := p.resolveBudget(request.Budget)
	if err != nil {
		return Window{}, err
	}
	fragmentIndex := fragmentAtMeasure(state.fragments, request.Offset, request.Affinity)
	anchorPage := state.fragments[fragmentIndex].pageFirst
	if state.totalMeasure > 0 && request.Offset == 0 {
		anchorPage = state.fragments[fragmentIndex].pageFirst
	} else if state.totalMeasure > 0 && request.Offset == state.totalMeasure {
		anchorPage = state.fragments[fragmentIndex].pageLast
	} else if request.Affinity == AffinityBefore {
		anchorPage = state.fragments[fragmentIndex].pageLast
	}
	firstFragment, lastFragment := fragmentIndex, fragmentIndex
	if request.Before > 0 {
		start := request.Offset - min(request.Offset, request.Before)
		if start < request.Offset {
			firstFragment = fragmentAtMeasure(state.fragments, start, AffinityAfter)
		}
	}
	if request.After > 0 {
		end := state.totalMeasure
		if request.Offset <= Measure(math.MaxInt64)-request.After && request.Offset+request.After < end {
			end = request.Offset + request.After
		}
		if end > request.Offset {
			lastFragment = fragmentAtMeasure(state.fragments, end, AffinityBefore)
		}
	}
	return p.readWindow(
		ctx, state, anchorPage, anchorPage,
		state.fragments[firstFragment].pageFirst, state.fragments[lastFragment].pageLast,
		budget,
	)
}

type usage struct {
	bytes     int64
	pages     int
	fragments map[int]struct{}
	measure   Measure
}

func (p *Pager) readWindow(ctx context.Context, state *pagerState, anchorFirst, anchorLast, desiredFirst, desiredLast int, budget Budget) (Window, error) {
	if anchorFirst < 0 || anchorLast < anchorFirst || anchorLast >= len(state.pages) ||
		desiredFirst < 0 || desiredFirst > anchorFirst || desiredLast < anchorLast || desiredLast >= len(state.pages) {
		return Window{}, ErrInvalidRequest
	}
	current := usage{fragments: make(map[int]struct{})}
	for index := anchorFirst; index <= anchorLast; index++ {
		if !fitsPage(state, index, budget, &current) {
			return Window{}, ErrBudgetExceeded
		}
		addPageUsage(state, index, &current)
	}
	first, last := anchorFirst, anchorLast
	truncatedBefore, truncatedAfter := false, false
	for index := anchorFirst - 1; index >= desiredFirst; index-- {
		if !fitsPage(state, index, budget, &current) {
			truncatedBefore = true
			break
		}
		addPageUsage(state, index, &current)
		first = index
	}
	for index := anchorLast + 1; index <= desiredLast; index++ {
		if !fitsPage(state, index, budget, &current) {
			truncatedAfter = true
			break
		}
		addPageUsage(state, index, &current)
		last = index
	}
	pages := make([]Page, 0, last-first+1)
	for index := first; index <= last; index++ {
		if err := ctx.Err(); err != nil {
			return Window{}, err
		}
		page, err := p.readMeta(ctx, state, index)
		if err != nil {
			return Window{}, err
		}
		pages = append(pages, page)
	}
	if err := p.stateStillCurrent(state); err != nil {
		return Window{}, err
	}
	return Window{
		Revision: p.revision, Generation: state.generation, Pages: pages,
		Bytes: current.bytes, Fragments: len(current.fragments), Measure: current.measure,
		IndexedThrough: state.indexedThrough, Complete: state.complete,
		TruncatedBefore: truncatedBefore, TruncatedAfter: truncatedAfter,
	}, nil
}

func fitsPage(state *pagerState, index int, budget Budget, current *usage) bool {
	page := state.pages[index]
	length := page.end - page.start
	if current.pages >= budget.Pages || length > budget.Bytes-current.bytes {
		return false
	}
	if page.fragment < 0 {
		return true
	}
	if _, exists := current.fragments[page.fragment]; exists {
		return true
	}
	if len(current.fragments) >= budget.Fragments {
		return false
	}
	fragment := state.fragments[page.fragment]
	return fragment.Measure <= budget.Measure-current.measure
}

func addPageUsage(state *pagerState, index int, current *usage) {
	page := state.pages[index]
	current.bytes += page.end - page.start
	current.pages++
	if page.fragment < 0 {
		return
	}
	if _, exists := current.fragments[page.fragment]; exists {
		return
	}
	current.fragments[page.fragment] = struct{}{}
	current.measure += state.fragments[page.fragment].Measure
}

func pageAtByte(pages []pageMeta, offset, length int64) int {
	if offset == length {
		return len(pages) - 1
	}
	index := sort.Search(len(pages), func(index int) bool { return pages[index].end > offset })
	if index == len(pages) {
		return len(pages) - 1
	}
	return index
}

func pageBeforeByte(pages []pageMeta, offset, length int64) int {
	if offset <= 0 {
		return 0
	}
	if offset >= length {
		return len(pages) - 1
	}
	return pageAtByte(pages, offset-1, length)
}

func fragmentAtMeasure(fragments []fragmentMeta, offset Measure, affinity Affinity) int {
	if len(fragments) == 1 {
		return 0
	}
	total := fragments[len(fragments)-1].measureStart + fragments[len(fragments)-1].Measure
	if total == 0 {
		if affinity == AffinityBefore {
			return len(fragments) - 1
		}
		return 0
	}
	if affinity == AffinityBefore {
		if offset == 0 {
			return 0
		}
		index := sort.Search(len(fragments), func(index int) bool {
			return fragments[index].measureStart >= offset
		}) - 1
		if index < 0 {
			return 0
		}
		return index
	}
	index := sort.Search(len(fragments), func(index int) bool {
		return fragments[index].measureStart+fragments[index].Measure > offset
	})
	if index == len(fragments) {
		return len(fragments) - 1
	}
	return index
}
