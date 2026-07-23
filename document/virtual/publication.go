package virtual

import (
	"context"
	"math"
	"sort"
	"strings"
)

// Publish validates and atomically installs all Fragments known below one
// indexed watermark.
func (p *Pager) Publish(ctx context.Context, publication Publication) (Stats, error) {
	taskContext, finish, err := p.acquireTask(ctx)
	if err != nil {
		return Stats{}, err
	}
	defer finish()
	operation, err := p.startProgress(taskContext, ProgressPublish)
	if err != nil {
		return Stats{}, err
	}
	var stats Stats
	if operation != nil {
		defer func() { operation.finish(stats, err) }()
	}
	stats, err = p.publish(taskContext, publication)
	return stats, err
}

// Refresh asks provider for a replacement without holding a Pager lock, then
// installs it only if no newer generation won the race.
func (p *Pager) Refresh(ctx context.Context, provider FragmentProvider) (Stats, error) {
	taskContext, finish, err := p.acquireTask(ctx)
	if err != nil {
		return Stats{}, err
	}
	defer finish()
	if provider == nil {
		return Stats{}, ErrInvalidPublication
	}
	operation, err := p.startProgress(taskContext, ProgressRefresh)
	if err != nil {
		return Stats{}, err
	}
	var stats Stats
	defer func() { operation.finish(stats, err) }()
	p.mu.RLock()
	request := FragmentRequest{
		Revision: p.revision, BaseGeneration: p.state.generation,
		ByteLength: p.length, MaxFragments: p.options.maximumFragments,
		MaxKeyBytes: p.options.maximumKeyBytes, MaxFragmentMeasure: p.options.window.Measure,
		Report: operation.report,
	}
	p.mu.RUnlock()
	result, providerErr := provider.Fragments(taskContext, request)
	if err = operation.finishProvider(result, providerErr); err != nil {
		return Stats{}, err
	}
	stats, err = p.publish(taskContext, Publication{
		Revision: request.Revision, BaseGeneration: request.BaseGeneration,
		IndexedThrough: result.IndexedThrough, Complete: result.Complete,
		Fragments: result.Fragments,
	})
	return stats, err
}

func (p *Pager) publish(ctx context.Context, publication Publication) (Stats, error) {
	if ctx == nil {
		return Stats{}, ErrInvalidContext
	}
	if err := contextError(ctx); err != nil {
		return Stats{}, err
	}
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return Stats{}, ErrClosed
	}
	revision, length := p.revision, p.length
	baseGeneration := p.state.generation
	p.mu.RUnlock()
	if publication.Revision != revision {
		return Stats{}, ErrRevisionMismatch
	}
	if publication.BaseGeneration != baseGeneration {
		return Stats{}, ErrStaleGeneration
	}
	if baseGeneration == math.MaxUint64 {
		return Stats{}, ErrGenerationOverflow
	}
	fragments, total, keyBytes, err := p.validateFragments(ctx, publication, length)
	if err != nil {
		return Stats{}, err
	}
	pages, err := p.buildPublishedPages(ctx, fragments, publication.IndexedThrough, publication.Complete)
	if err != nil {
		return Stats{}, err
	}
	byID := make(map[string]int, len(fragments))
	for index := range fragments {
		byID[fragments[index].ID] = index
	}
	state := &pagerState{
		generation: baseGeneration + 1, pages: pages, fragments: fragments,
		fragmentByID: byID, indexedThrough: publication.IndexedThrough,
		complete: publication.Complete, totalMeasure: total, keyBytes: keyBytes,
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return Stats{}, ErrClosed
	}
	if p.state.generation != baseGeneration {
		p.mu.Unlock()
		return Stats{}, ErrStaleGeneration
	}
	p.state = state
	p.clearCacheLocked()
	stats := p.statsLocked()
	p.mu.Unlock()
	return stats, nil
}

func (p *Pager) validateFragments(ctx context.Context, publication Publication, length int64) ([]fragmentMeta, Measure, int64, error) {
	if publication.IndexedThrough < 0 || publication.IndexedThrough > length ||
		(publication.Complete && publication.IndexedThrough != length) ||
		len(publication.Fragments) > p.options.maximumFragments {
		return nil, 0, 0, ErrInvalidPublication
	}
	fragments := make([]fragmentMeta, len(publication.Fragments))
	seen := make(map[string]struct{}, len(fragments))
	var cursor int64
	var total Measure
	var keyBytes int64
	for index, fragment := range publication.Fragments {
		if err := contextError(ctx); err != nil {
			return nil, 0, 0, err
		}
		if fragment.ID == "" || fragment.Start < cursor || fragment.End <= fragment.Start ||
			fragment.End > publication.IndexedThrough || fragment.Measure < 0 ||
			fragment.Measure > p.options.window.Measure {
			return nil, 0, 0, ErrInvalidFragment
		}
		keyLength := int64(len(fragment.ID)) + int64(len(fragment.DataKey))
		if keyLength > p.options.maximumKeyBytes-keyBytes {
			return nil, 0, 0, ErrInvalidFragment
		}
		if _, exists := seen[fragment.ID]; exists {
			return nil, 0, 0, ErrInvalidFragment
		}
		seen[fragment.ID] = struct{}{}
		keyBytes += keyLength
		next, ok := checkedAddMeasure(total, fragment.Measure)
		if !ok {
			return nil, 0, 0, ErrInvalidFragment
		}
		owned := fragment
		owned.ID = strings.Clone(fragment.ID)
		owned.DataKey = strings.Clone(fragment.DataKey)
		fragments[index] = fragmentMeta{Fragment: owned, measureStart: total, pageFirst: -1, pageLast: -1}
		total = next
		cursor = fragment.End
	}
	return fragments, total, keyBytes, nil
}

func (p *Pager) buildPublishedPages(ctx context.Context, fragments []fragmentMeta, indexedThrough int64, complete bool) ([]pageMeta, error) {
	extra := make([]int64, 0, len(fragments)*2+1)
	for _, fragment := range fragments {
		extra = append(extra, fragment.Start)
		extra = append(extra, fragment.End)
	}
	if indexedThrough > 0 && indexedThrough < p.length {
		extra = append(extra, indexedThrough)
	}
	sort.Slice(extra, func(left, right int) bool { return extra[left] < extra[right] })
	extra = compactOffsets(extra)
	lines, afterLF, err := lineAndBoundaryMap(ctx, p.source, p.logical, extra)
	if err != nil {
		return nil, err
	}
	boundaries := make([]int64, 0, len(p.logical)+len(extra)+1)
	boundaries = append(boundaries, 0)
	for _, page := range p.logical {
		boundaries = append(boundaries, page.end)
	}
	boundaries = append(boundaries, extra...)
	sort.Slice(boundaries, func(left, right int) bool { return boundaries[left] < boundaries[right] })
	boundaries = compactOffsets(boundaries)
	if p.length == 0 {
		return []pageMeta{{fragment: -1, indexed: complete}}, nil
	}
	pages := make([]pageMeta, 0, len(boundaries)-1)
	fragmentIndex := 0
	for index := 0; index+1 < len(boundaries); index++ {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		start, end := boundaries[index], boundaries[index+1]
		for fragmentIndex < len(fragments) && fragments[fragmentIndex].End <= start {
			fragmentIndex++
		}
		meta := pageMeta{
			start: start, end: end, startLine: lines[start], endLine: lines[end],
			continuesFrom: start > 0 && !afterLF[start],
			continuesTo:   end < p.length && !afterLF[end],
			endsWithLF:    afterLF[end],
			fragment:      -1,
			indexed:       end <= indexedThrough,
		}
		if fragmentIndex < len(fragments) && fragments[fragmentIndex].Start <= start && end <= fragments[fragmentIndex].End {
			meta.fragment = fragmentIndex
			meta.measureStart = fragments[fragmentIndex].measureStart
			meta.measureEnd = fragments[fragmentIndex].measureStart + fragments[fragmentIndex].Measure
		}
		pages = append(pages, meta)
	}
	for pageIndex := range pages {
		fragmentIndex := pages[pageIndex].fragment
		if fragmentIndex < 0 {
			continue
		}
		if fragments[fragmentIndex].pageFirst < 0 {
			fragments[fragmentIndex].pageFirst = pageIndex
		}
		fragments[fragmentIndex].pageLast = pageIndex
	}
	for fragmentIndex := range fragments {
		count := fragments[fragmentIndex].pageLast - fragments[fragmentIndex].pageFirst + 1
		for pageIndex := fragments[fragmentIndex].pageFirst; pageIndex <= fragments[fragmentIndex].pageLast; pageIndex++ {
			pages[pageIndex].continuation = pageIndex - fragments[fragmentIndex].pageFirst
			pages[pageIndex].continuations = count
		}
	}
	return pages, nil
}

func compactOffsets(offsets []int64) []int64 {
	if len(offsets) == 0 {
		return offsets
	}
	write := 1
	for read := 1; read < len(offsets); read++ {
		if offsets[read] != offsets[write-1] {
			offsets[write] = offsets[read]
			write++
		}
	}
	return offsets[:write]
}
