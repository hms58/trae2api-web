// credits_cache.go 额度查询共享缓存：账号列表与额度监控共用，
// 避免面板频繁刷新打上游触发风控。默认 TTL 10s，并发同 uid 只发一次上游请求。
package server

import (
	"errors"
	"time"
)

// creditsSnapshot 一次额度查询结果（EntUsage 全字段）。
type creditsSnapshot struct {
	Remain int64
	Limit  int64
	Used   int64
	Packs  int
	Err    error
}

// defaultCreditsCacheTTL 额度缓存默认时长。
const defaultCreditsCacheTTL = 10 * time.Second

// lookupCredits 查询账号额度（走共享缓存），并把结果同步回池供选号策略用。
func (h *Handler) lookupCredits(uid string) creditsSnapshot {
	snap := h.credits.get(uid, func() creditsSnapshot {
		if h.cfg.Upstream == nil {
			return creditsSnapshot{Err: errNoUpstream}
		}
		a := h.cfg.Pool.AuthByUID(uid)
		if a == nil {
			return creditsSnapshot{Err: errNoAuth}
		}
		remain, limit, used, packs, err := h.cfg.Upstream.EntUsage(a)
		if err != nil {
			return creditsSnapshot{Err: err}
		}
		return creditsSnapshot{Remain: remain, Limit: limit, Used: used, Packs: packs}
	})
	if snap.Err == nil {
		h.cfg.Pool.SetCredits(uid, snap.Remain) // 同步回池，供选号策略用
	}
	return snap
}

// invalidateCredits 使某账号额度缓存立即失效（凭证变化后调用）。
func (h *Handler) invalidateCredits(uid string) {
	if h.credits != nil {
		h.credits.invalidate(uid)
	}
}

var (
	errNoUpstream = errors.New("upstream not configured")
	errNoAuth     = errors.New("no auth for uid")
)
