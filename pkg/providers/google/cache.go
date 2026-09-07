package google

import (
	"container/list"
	"sync"
)

type signatureCache struct {
	mu        sync.Mutex
	capacity  int
	items     map[string]*list.Element
	evictList *list.List
}

type cacheEntry struct {
	key   string
	value string
}

func newSignatureCache(capacity int) *signatureCache {
	return &signatureCache{
		capacity:  capacity,
		items:     make(map[string]*list.Element),
		evictList: list.New(),
	}
}

var globalSignatureCache = newSignatureCache(10000)

// StoreThoughtSignature caches the thought signature for a given toolCallID.
func StoreThoughtSignature(callID, signature string) {
	if callID == "" || signature == "" {
		return
	}
	globalSignatureCache.Set(callID, signature)
}

// GetThoughtSignature retrieves the thought signature by toolCallID.
func GetThoughtSignature(callID string) string {
	if callID == "" {
		return ""
	}
	return globalSignatureCache.Get(callID)
}

func (c *signatureCache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		elem.Value.(*cacheEntry).value = value
		return
	}

	elem := c.evictList.PushFront(&cacheEntry{key: key, value: value})
	c.items[key] = elem

	if c.capacity > 0 && c.evictList.Len() > c.capacity {
		oldest := c.evictList.Back()
		if oldest != nil {
			c.evictList.Remove(oldest)
			kv := oldest.Value.(*cacheEntry)
			delete(c.items, kv.key)
		}
	}
}

func (c *signatureCache) Get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		return elem.Value.(*cacheEntry).value
	}

	return ""
}
