package virtual

import (
	"container/list"
	"context"
)

func (p *Pager) clearCacheLocked() {
	p.cache = make(map[cacheKey]*list.Element)
	p.cacheList.Init()
	p.cacheBytes = 0
}

func (p *Pager) cached(key cacheKey) ([]byte, bool) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, false
	}
	element := p.cache[key]
	if element == nil {
		p.mu.Unlock()
		return nil, false
	}
	p.cacheList.MoveToFront(element)
	data := element.Value.(*cacheEntry).data
	p.mu.Unlock()
	return append([]byte(nil), data...), true
}

func (p *Pager) storeCache(state *pagerState, key cacheKey, data []byte) {
	if p.options.cacheBytes == 0 || int64(len(data)) > p.options.cacheBytes {
		return
	}
	copyOfData := append([]byte(nil), data...)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.state != state {
		return
	}
	if existing := p.cache[key]; existing != nil {
		p.cacheList.MoveToFront(existing)
		return
	}
	element := p.cacheList.PushFront(&cacheEntry{key: key, data: copyOfData})
	p.cache[key] = element
	p.cacheBytes += int64(len(copyOfData))
	for p.cacheBytes > p.options.cacheBytes {
		last := p.cacheList.Back()
		entry := last.Value.(*cacheEntry)
		delete(p.cache, entry.key)
		p.cacheBytes -= int64(len(entry.data))
		p.cacheList.Remove(last)
	}
}

func (p *Pager) readMeta(ctx context.Context, state *pagerState, index int) (Page, error) {
	meta := state.pages[index]
	key := cacheKey{generation: state.generation, index: index, start: meta.start, end: meta.end}
	data, ok := p.cached(key)
	if !ok {
		var err error
		data, err = readPageBytes(ctx, p.source, meta)
		if err != nil {
			return Page{}, err
		}
		p.storeCache(state, key, data)
	}
	page := Page{
		Key: PageKey{
			Revision: p.revision, Generation: state.generation, Index: index,
			Start: meta.start, End: meta.end, identity: p.identity,
		},
		StartLine: meta.startLine, EndLine: meta.endLine,
		ContinuesFromPrevious: meta.continuesFrom, ContinuesToNext: meta.continuesTo,
		FragmentIndex: -1, ContinuationIndex: meta.continuation, ContinuationCount: meta.continuations,
		MeasureStart: meta.measureStart, MeasureEnd: meta.measureEnd, Indexed: meta.indexed,
		Content: data,
	}
	if meta.fragment >= 0 {
		fragment := state.fragments[meta.fragment]
		page.FragmentID = fragment.ID
		page.DataKey = fragment.DataKey
		page.FragmentIndex = meta.fragment
	}
	return page, nil
}
