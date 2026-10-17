// cache.go 通用 TTL 缓存 + single-flight：面板数据（额度/签到状态）共用，
// 避免频繁刷新打上游触发风控。同一 key 并发只发一次上游请求。
package server

import (
	"sync"
	"time"
)

// ttlCache 带 TTL 的键值缓存，同 key 并发请求合并为一次 fetch（single-flight）。
// 错误结果同样缓存 TTL 时长，避免上游故障时被反复击穿。
type ttlCache[T any] struct {
	ttl time.Duration

	mu       sync.Mutex
	items    map[string]cacheItem[T]
	inflight map[string]*cacheCall[T]
}

type cacheItem[T any] struct {
	val T
	at  time.Time
}

type cacheCall[T any] struct {
	done chan struct{}
	val  T
}

// newTTLCache 构建缓存；ttl <= 0 时使用 fallback。
func newTTLCache[T any](ttl, fallback time.Duration) *ttlCache[T] {
	if ttl <= 0 {
		ttl = fallback
	}
	return &ttlCache[T]{
		ttl:      ttl,
		items:    map[string]cacheItem[T]{},
		inflight: map[string]*cacheCall[T]{},
	}
}

// get 返回 key 的缓存值：TTL 内命中；否则调用 fetch 并写入缓存。
func (c *ttlCache[T]) get(key string, fetch func() T) T {
	c.mu.Lock()
	if it, ok := c.items[key]; ok && time.Since(it.at) < c.ttl {
		c.mu.Unlock()
		return it.val
	}
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.val
	}
	call := &cacheCall[T]{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	val := fetch()

	c.mu.Lock()
	c.items[key] = cacheItem[T]{val: val, at: time.Now()}
	call.val = val
	delete(c.inflight, key)
	c.mu.Unlock()
	close(call.done)
	return val
}

// invalidate 使 key 缓存立即失效（下次重查）。
func (c *ttlCache[T]) invalidate(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}
