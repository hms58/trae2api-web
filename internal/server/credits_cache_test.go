package server

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"trae2api-web/internal/auth"
)

// TTL 内重复查询命中缓存，不重复打上游。
func TestCreditsCacheHitsWithinTTL(t *testing.T) {
	calls := atomic.Int32{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":10}}]}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	// 两次查询（模拟账号列表 + 额度监控）
	for i := 0; i < 2; i++ {
		snap := h.lookupCredits("u1")
		if snap.Err != nil {
			t.Fatalf("query %d err=%v", i, snap.Err)
		}
		if snap.Remain != 90 {
			t.Errorf("remain=%d want 90", snap.Remain)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream calls=%d want 1 (cache hit)", got)
	}
}

// 超过 TTL 后重新查询上游。
func TestCreditsCacheExpiresAfterTTL(t *testing.T) {
	calls := atomic.Int32{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":100}}}]}`, false
	})
	h := NewHandler(Config{
		Pool:            testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:        up,
		CreditsCacheTTL: 20 * time.Millisecond,
	})
	h.lookupCredits("u1")
	h.lookupCredits("u1")
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls=%d want 1 before expiry", got)
	}
	time.Sleep(30 * time.Millisecond)
	h.lookupCredits("u1")
	if got := calls.Load(); got != 2 {
		t.Errorf("calls=%d want 2 after expiry", got)
	}
}

// 并发同一 uid 只发一次上游请求（single-flight）。
func TestCreditsCacheSingleFlight(t *testing.T) {
	calls := atomic.Int32{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond) // 拉长窗口让并发都撞上
		return 200, `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":50}}}]}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.lookupCredits("u1")
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Errorf("concurrent calls=%d want 1 (single-flight)", got)
	}
}

// 错误也缓存，避免上游故障时被反复击穿。
func TestCreditsCacheCachesError(t *testing.T) {
	calls := atomic.Int32{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	for i := 0; i < 3; i++ {
		if snap := h.lookupCredits("u1"); snap.Err == nil {
			t.Fatal("want error")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("error should be cached, calls=%d want 1", got)
	}
}

// invalidate 后立即重查。
func TestCreditsCacheInvalidate(t *testing.T) {
	calls := atomic.Int32{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":10}}}]}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	h.lookupCredits("u1")
	h.invalidateCredits("u1")
	h.lookupCredits("u1")
	if got := calls.Load(); got != 2 {
		t.Errorf("calls=%d want 2 after invalidate", got)
	}
}

func TestCreditsCacheNilUpstream(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	if snap := h.lookupCredits("u1"); snap.Err == nil {
		t.Error("nil upstream should return error")
	}
}
