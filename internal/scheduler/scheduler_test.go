package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// fakeUpstream 同时模拟 checkin/ent_usage/refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	claimCalls     atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
	checkedIn      bool   // status 返回"已签到"（人工签到过）
	claimBody      string // claim 响应体，空 = {"code":0,"message":"success"}
	statusBody     string // status 响应体，空 = 按 checkedIn 生成
	// checkedInAfterClaim 领取请求过后 status 改报已签到（模拟「报错但实际已入账」）
	checkedInAfterClaim bool
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/status"):
			f.checkinCalls.Add(1)
			if f.statusBody != "" {
				w.Write([]byte(f.statusBody))
				return
			}
			checked := "false"
			if f.checkedIn || (f.checkedInAfterClaim && f.claimCalls.Load() > 0) {
				checked = "true"
			}
			w.Write([]byte(`{"checked_in":` + checked + `,"credits":200,"enable":true}`))
		case strings.HasSuffix(r.URL.Path, "/checkin_credits/claim"):
			f.claimCalls.Add(1)
			if f.claimBody != "" {
				w.Write([]byte(f.claimBody))
				return
			}
			w.Write([]byte(`{"code":0,"message":"success"}`))
		case strings.HasSuffix(r.URL.Path, "/ide_user_ent_usage"):
			w.Write([]byte(`{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":` +
				jsonI64(f.resourceRemain) + `}}}]}`))
		case strings.HasSuffix(r.URL.Path, "/ExchangeToken"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`))
		default:
			http.Error(w, "not found: "+r.URL.Path, 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newTestScheduler(f *fakeUpstream, p *pool.Pool, srv *httptest.Server) *Scheduler {
	up := &upstream.Client{
		HTTP:      srv.Client(),
		AgentHost: srv.URL,
		UgHost:    srv.URL,
		OAuthHost: srv.URL,
		ClientID:  upstream.ClientID,
	}
	return New(Config{
		Pool:         p,
		Upstream:     up,
		CheckinHour:  9,
		RefreshHours: []int{3},
		RefreshSkew:  time.Hour,
	})
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolPlan, time.Hour, "plan limit")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin status calls=%d", f.checkinCalls.Load())
	}
	if f.claimCalls.Load() != 1 {
		t.Errorf("claim calls=%d", f.claimCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunCheckinSkipsDisabled(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 0 {
		t.Errorf("disabled account should be skipped, calls=%d", f.checkinCalls.Load())
	}
}

// 领取被上游以业务码拒绝（HTTP 200 + code=9074）时必须记失败：
// 旧实现丢弃响应体，把 9074 当成功写进执行记录，造成「显示成功其实没领到」。
func TestRunCheckinClaimBusinessErrorCountsFailed(t *testing.T) {
	f := &fakeUpstream{
		resourceRemain: 500,
		claimBody:      `{"code":9074,"message":"当前参与用户太多，请稍后再试"}`,
	}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	recs := s.Records()
	if len(recs) != 1 {
		t.Fatalf("records=%d want 1", len(recs))
	}
	if recs[0].Result != "failed" {
		t.Errorf("result=%q want failed", recs[0].Result)
	}
	if !strings.Contains(recs[0].Summary, "9074") {
		t.Errorf("summary should carry upstream code: %q", recs[0].Summary)
	}
}

// 领取报错但复查已签到：按已签到计，不误报失败（如超时后上游其实已入账）。
func TestRunCheckinClaimErrorVerifiedCheckedIn(t *testing.T) {
	f := &fakeUpstream{
		resourceRemain:      500,
		claimBody:           `{"code":1,"message":"internal busy"}`,
		checkedInAfterClaim: true,
	}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 2 {
		t.Errorf("status calls=%d want 2 (initial + verify after claim error)", f.checkinCalls.Load())
	}
	recs := s.Records()
	if len(recs) != 1 {
		t.Fatalf("records=%d want 1", len(recs))
	}
	if recs[0].Result != "success" {
		t.Errorf("result=%q summary=%q want success (verified checked in)", recs[0].Result, recs[0].Summary)
	}
}

// 上游关闭签到（enable=false）不能计入成功：没有领取动作就是没有领取。
func TestRunCheckinUpstreamDisabledCountsSkipped(t *testing.T) {
	f := &fakeUpstream{
		resourceRemain: 500,
		statusBody:     `{"checked_in":false,"credits":0,"enable":false}`,
	}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.claimCalls.Load() != 0 {
		t.Errorf("claim should not be sent when enable=false, calls=%d", f.claimCalls.Load())
	}
	recs := s.Records()
	if len(recs) != 1 {
		t.Fatalf("records=%d want 1", len(recs))
	}
	if recs[0].Result != "skipped" {
		t.Errorf("result=%q want skipped", recs[0].Result)
	}
}

func TestRunRefreshRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL}
	p.Add(a)

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "newat" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunRefreshSkipsFreshToken(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "fresh", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: srv.URL})

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()
	if f.refreshCalls.Load() != 0 {
		t.Errorf("fresh token should not refresh, calls=%d", f.refreshCalls.Load())
	}
}

func TestRunRefreshSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":20101,"msg":"login required"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL})

	up := &upstream.Client{HTTP: srv.Client(), AgentHost: srv.URL, UgHost: srv.URL, OAuthHost: srv.URL, ClientID: upstream.ClientID}
	s := New(Config{Pool: p, Upstream: up})
	s.RunRefreshNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("should disable session-dead account: %+v", st)
	}
}
