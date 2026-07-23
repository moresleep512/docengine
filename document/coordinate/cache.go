package coordinate

func (i *Index) cachedWindow(start int64) ([]byte, bool) {
	i.cacheMu.Lock()
	defer i.cacheMu.Unlock()
	if element := i.cache[start]; element != nil {
		i.cacheList.MoveToFront(element)
		i.cacheHits++
		return element.Value.(*cacheEntry).data, true
	}
	i.cacheMisses++
	return nil, false
}

func (i *Index) storeWindow(start int64, data []byte) {
	i.cacheMu.Lock()
	defer i.cacheMu.Unlock()
	if i.maxCacheBytes == 0 || int64(len(data)) > i.maxCacheBytes || i.cache == nil {
		return
	}
	if element := i.cache[start]; element != nil {
		i.cacheList.MoveToFront(element)
		return
	}
	element := i.cacheList.PushFront(&cacheEntry{start: start, data: data})
	i.cache[start] = element
	i.cacheBytes += int64(len(data))
	for i.cacheBytes > i.maxCacheBytes {
		last := i.cacheList.Back()
		entry := last.Value.(*cacheEntry)
		delete(i.cache, entry.start)
		i.cacheBytes -= int64(len(entry.data))
		i.cacheList.Remove(last)
	}
}
