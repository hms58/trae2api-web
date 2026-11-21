package server

import (
	"strings"
	"sync/atomic"
	"testing"

	"trae2api-web/internal/auth"
)

// 签到状态缓存命中：TTL 内重复查询不重复打上游。
func TestCheckinCacheHitsWithinTTL(t *testing.T) {
	calls := atomic.Int32{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"checked_in":true,"credits":100,"enable":true}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	for i := 0; i < 3; i++ {
		snap := h.lookupCheckin("u1")
		if snap.Err != nil {
			t.Fatalf("query %d err=%v", i, snap.Err)
		}
		if !snap.CheckedIn || snap.Credits != 100 || !snap.Enable {
			t.Errorf("snap=%+v", snap)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("checkin upstream calls=%d want 1 (cache hit)", got)
	}
}

// invalidate 后立即重查。
func TestCheckinCacheInvalidate(t *testing.T) {
	calls := atomic.Int32{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 200, `{"checked_in":false,"credits":100,"enable":true}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	h.lookupCheckin("u1")
	h.invalidateCheckin("u1")
	h.lookupCheckin("u1")
	if got := calls.Load(); got != 2 {
		t.Errorf("calls=%d want 2 after invalidate", got)
	}
}

// 额度与签到状态是两个独立端点，缓存也独立：
// 用按路径分流的 fake 上游分别计数，验证各自只查一次。
func TestCreditsAndCheckinCachesAreIndependent(t *testing.T) {
	entCalls := atomic.Int32{}
	ckCalls := atomic.Int32{}
	up := newPathUpstream(t, func(path string) (int, string) {
		switch {
		case strings.HasSuffix(path, "/ide_user_ent_usage"):
			entCalls.Add(1)
			return 200, `{"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":100}}}]}`
		case strings.HasSuffix(path, "/checkin_credits/status"):
			ckCalls.Add(1)
			return 200, `{"checked_in":true,"credits":100,"enable":true}`
		}
		return 404, `not found`
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	// 两次额度查询 + 两次签到查询，均命中缓存 → 各 1 次上游
	h.lookupCredits("u1")
	h.lookupCredits("u1")
	h.lookupCheckin("u1")
	h.lookupCheckin("u1")
	if got := entCalls.Load(); got != 1 {
		t.Errorf("ent calls=%d want 1", got)
	}
	if got := ckCalls.Load(); got != 1 {
		t.Errorf("checkin calls=%d want 1", got)
	}
}
