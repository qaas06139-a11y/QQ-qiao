// Package auth 提供认证相关功能的 HTTP 客户端
package auth

import (
	"container/list"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// 全局 HTTP 客户端存储，支持运行时代理重配置
var httpClientStore atomic.Pointer[http.Client]

// authProxyClientCache caches per-proxy auth HTTP clients with LRU eviction
// + idle pruning so deployments that rotate per-account proxies don't grow
// the cache unboundedly.
var authProxyClientCache = newAuthClientLRU()

const (
	authClientCacheMax     = 64
	authClientCacheIdleTTL = 30 * time.Minute
	authClientSweepEvery   = 5 * time.Minute
)

type authClientEntry struct {
	key      string
	client   *http.Client
	lastUsed time.Time
}

type authClientLRU struct {
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List
}

func newAuthClientLRU() *authClientLRU {
	c := &authClientLRU{
		items: make(map[string]*list.Element, authClientCacheMax),
		order: list.New(),
	}
	go c.sweepLoop()
	return c
}

func (c *authClientLRU) Get(key string) (*http.Client, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*authClientEntry)
		entry.lastUsed = time.Now()
		c.order.MoveToFront(elem)
		return entry.client, true
	}
	return nil, false
}

func (c *authClientLRU) Put(key string, client *http.Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*authClientEntry)
		entry.client = client
		entry.lastUsed = time.Now()
		c.order.MoveToFront(elem)
		return
	}
	entry := &authClientEntry{key: key, client: client, lastUsed: time.Now()}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
	for c.order.Len() > authClientCacheMax {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.evictLocked(oldest)
	}
}

func (c *authClientLRU) evictLocked(elem *list.Element) {
	entry := elem.Value.(*authClientEntry)
	c.order.Remove(elem)
	delete(c.items, entry.key)
	if entry.client != nil {
		if t, ok := entry.client.Transport.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
}

func (c *authClientLRU) sweepLoop() {
	ticker := time.NewTicker(authClientSweepEvery)
	defer ticker.Stop()
	for range ticker.C {
		c.sweepIdle()
	}
}

func (c *authClientLRU) sweepIdle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := time.Now().Add(-authClientCacheIdleTTL)
	for elem := c.order.Back(); elem != nil; {
		entry := elem.Value.(*authClientEntry)
		prev := elem.Prev()
		if entry.lastUsed.Before(cutoff) {
			c.evictLocked(elem)
		}
		elem = prev
	}
}

// httpClient 返回当前全局 auth HTTP 客户端
func httpClient() *http.Client {
	return httpClientStore.Load()
}

func init() {
	InitHttpClient("")
}

// GetAuthClientForProxy returns an auth HTTP client for the given proxy URL.
// If proxyURL is empty, returns the global auth HTTP client.
func GetAuthClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return httpClient()
	}
	if cached, ok := authProxyClientCache.Get(proxyURL); ok {
		return cached
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildAuthTransport(proxyURL),
	}
	authProxyClientCache.Put(proxyURL, client)
	return client
}

// buildAuthTransport 构建带可选代理的 Transport
func buildAuthTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        50,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitHttpClient 初始化（或重新初始化）auth 模块的全局 HTTP 客户端
func InitHttpClient(proxyURL string) {
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildAuthTransport(proxyURL),
	}
	httpClientStore.Store(client)
}
