package proxy

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

// proxyClientCache is a small LRU keyed by "scheme://[user:pass@]host:port"
// storing pre-built http.Client instances for per-account outbound proxies.
//
// The previous implementation used a sync.Map without any eviction. That was
// fine when a deployment had a handful of static proxy URLs, but anything
// that rotates proxy credentials per request (corporate squid, residential
// proxy pools, …) would grow the map unboundedly. This cache caps total
// entries and evicts the least-recently-used client when full; in addition,
// idle clients past idleTTL are pruned by a background sweeper so unused
// transports release their idle connection pools.
const (
	proxyClientCacheMax    = 64
	proxyClientCacheIdleTTL = 30 * time.Minute
	proxyClientSweepEvery  = 5 * time.Minute
)

type proxyClientEntry struct {
	key       string
	client    *http.Client
	lastUsed  time.Time
}

type proxyClientLRU struct {
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List // front = most recently used
}

func newProxyClientLRU() *proxyClientLRU {
	c := &proxyClientLRU{
		items: make(map[string]*list.Element, proxyClientCacheMax),
		order: list.New(),
	}
	go c.sweepLoop()
	return c
}

// Get returns the cached client for key, refreshing its LRU position.
func (c *proxyClientLRU) Get(key string) (*http.Client, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*proxyClientEntry)
		entry.lastUsed = time.Now()
		c.order.MoveToFront(elem)
		return entry.client, true
	}
	return nil, false
}

// Put stores client under key, evicting the LRU entry if over capacity.
func (c *proxyClientLRU) Put(key string, client *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*proxyClientEntry)
		entry.client = client
		entry.lastUsed = time.Now()
		c.order.MoveToFront(elem)
		return
	}
	entry := &proxyClientEntry{key: key, client: client, lastUsed: time.Now()}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
	for c.order.Len() > proxyClientCacheMax {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.evictLocked(oldest)
	}
}

func (c *proxyClientLRU) evictLocked(elem *list.Element) {
	entry := elem.Value.(*proxyClientEntry)
	c.order.Remove(elem)
	delete(c.items, entry.key)
	if entry.client != nil {
		if t, ok := entry.client.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
}

func (c *proxyClientLRU) sweepLoop() {
	ticker := time.NewTicker(proxyClientSweepEvery)
	defer ticker.Stop()
	for range ticker.C {
		c.sweepIdle()
	}
}

func (c *proxyClientLRU) sweepIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := time.Now().Add(-proxyClientCacheIdleTTL)
	for elem := c.order.Back(); elem != nil; {
		entry := elem.Value.(*proxyClientEntry)
		prev := elem.Prev()
		if entry.lastUsed.Before(cutoff) {
			c.evictLocked(elem)
		}
		elem = prev
	}
}
