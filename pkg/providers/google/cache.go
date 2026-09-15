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

// noThoughtSignature records that Google deliberately omitted a signature for
// this call (for example, later calls in a parallel function-call response).
// This is distinct from a cache miss, where a cross-protocol caller needs the
// documented validator-bypass sentinel.
const noThoughtSignature = "\x00"

func signatureKey(scope, callID string) string {
	if scope == "" || callID == "" {
		return ""
	}
	return scope + "\x00" + callID
}

// StoreThoughtSignature caches the thought signature for a given toolCallID.
func StoreThoughtSignature(callID, signature string) {
	StoreThoughtSignatureForScope("legacy", callID, signature)
}

// GetThoughtSignature retrieves the thought signature by toolCallID.
func GetThoughtSignature(callID string) string {
	return GetThoughtSignatureForScope("legacy", callID)
}

// StoreThoughtSignatureForScope keeps signatures isolated between client
// sessions. Google signatures are opaque and must never be reused across
// unrelated conversations that happen to choose the same tool-call ID.
func StoreThoughtSignatureForScope(scope, callID, signature string) {
	key := signatureKey(scope, callID)
	if key == "" || signature == "" {
		return
	}
	globalSignatureCache.Set(key, signature)
}

func GetThoughtSignatureForScope(scope, callID string) string {
	signature, _ := lookupThoughtSignatureForScope(scope, callID)
	return signature
}

func storeThoughtSignaturePresenceForScope(scope, callID, signature string) {
	key := signatureKey(scope, callID)
	if key == "" {
		return
	}
	if signature == "" {
		signature = noThoughtSignature
	}
	globalSignatureCache.Set(key, signature)

}

func lookupThoughtSignatureForScope(scope, callID string) (string, bool) {
	key := signatureKey(scope, callID)
	if key == "" {
		return "", false
	}
	signature, ok := globalSignatureCache.Lookup(key)
	if signature == noThoughtSignature {
		return "", ok
	}
	return signature, ok
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
	value, _ := c.Lookup(key)
	return value
}

func (c *signatureCache) Lookup(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.evictList.MoveToFront(elem)
		return elem.Value.(*cacheEntry).value, true
	}

	return "", false
}
