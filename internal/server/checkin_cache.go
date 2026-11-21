// checkin_cache.go 签到状态缓存：额度监控页每次刷新都会查签到状态，
// 而该状态一天只变一次（签到后），故用较长 TTL（默认 5min）避免频繁打上游。
package server

import "time"

// checkinSnapshot 一次签到状态查询结果。
type checkinSnapshot struct {
	CheckedIn bool
	Credits   int64
	Enable    bool
	Err       error
}

// defaultCheckinCacheTTL 签到状态缓存默认时长。
const defaultCheckinCacheTTL = 5 * time.Minute

// lookupCheckin 查询签到状态（走共享缓存）。
func (h *Handler) lookupCheckin(uid string) checkinSnapshot {
	return h.checkin.get(uid, func() checkinSnapshot {
		if h.cfg.Upstream == nil {
			return checkinSnapshot{Err: errNoUpstream}
		}
		a := h.cfg.Pool.AuthByUID(uid)
		if a == nil {
			return checkinSnapshot{Err: errNoAuth}
		}
		checkedIn, credits, enable, err := h.cfg.Upstream.CheckinStatus(a)
		if err != nil {
			return checkinSnapshot{Err: err}
		}
		return checkinSnapshot{CheckedIn: checkedIn, Credits: credits, Enable: enable}
	})
}

// invalidateCheckin 使某账号签到缓存立即失效（签到成功后调用）。
func (h *Handler) invalidateCheckin(uid string) {
	if h.checkin != nil {
		h.checkin.invalidate(uid)
	}
}
