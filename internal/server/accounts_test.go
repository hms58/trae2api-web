package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api-web/internal/auth"
)

// PATCH enabled=true 对硬禁用账号：refresh 成功 → 复活；失败 → 保持禁用并报错。
func TestPatchEnabledRevivesDisabledAccount(t *testing.T) {
	exchangeBody := `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":9999999999,"TokenExpireDuration":1209600}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, exchangeBody, false
	})
	dir := t.TempDir()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt", ExpiresAt: 1})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: dir})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PATCH", "/admin/api/accounts/u1", strings.NewReader(`{"enabled":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Errorf("account should be revived: %+v", st)
	}
	if !st.Enabled {
		t.Errorf("account should be enabled: %+v", st)
	}
	// 凭证已换新并落盘
	a := p.AuthByUID("u1")
	if a.AccessToken != "newat" {
		t.Errorf("token=%s want newat", a.AccessToken)
	}
}

func TestPatchEnabledReviveFailsKeepsDisabled(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":20101,"msg":"login required"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt", ExpiresAt: 1})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PATCH", "/admin/api/accounts/u1", strings.NewReader(`{"enabled":true}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code=%d want 502 body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("failed revive should keep disabled: %+v", st)
	}
}

// 账号列表的积分应实时查上游（池缓存可能长期为 0），并同步回池。
func TestAdminAccountsFetchesLiveCredits(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":4250}},"usage":{"credits_amount":1022}}]}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 0) // 池内缓存为 0（未签到过）
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/accounts", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var d struct {
		Accounts []accountSummary `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Accounts) != 1 {
		t.Fatalf("accounts=%d want 1", len(d.Accounts))
	}
	if d.Accounts[0].Credits != 3228 { // 4250 - 1022
		t.Errorf("credits=%d want 3228 (live)", d.Accounts[0].Credits)
	}
	if d.Accounts[0].CreditsError != "" {
		t.Errorf("credits_error=%q want empty", d.Accounts[0].CreditsError)
	}
	// 同步回池（选号策略用）
	st, _ := p.Status("u1")
	if st.Credits != 3228 {
		t.Errorf("pool credits=%d want 3228", st.Credits)
	}
}

// 实时查询失败时回退池内缓存值，并标记 credits_error（页面可见）。
func TestAdminAccountsFallsBackOnQueryFailure(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 777)
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/accounts", nil))
	var d struct {
		Accounts []accountSummary `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Accounts[0].Credits != 777 {
		t.Errorf("credits=%d want cached 777", d.Accounts[0].Credits)
	}
	if d.Accounts[0].CreditsError == "" {
		t.Error("credits_error should be set on query failure")
	}
}

// 手动刷新成功也复活禁用账号。
func TestManualRefreshRevivesDisabledAccount(t *testing.T) {
	exchangeBody := `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":9999999999,"TokenExpireDuration":1209600}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, exchangeBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", RefreshToken: "rt", ExpiresAt: 1})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: t.TempDir()})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/api/accounts/u1/refresh", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Errorf("manual refresh success should revive: %+v", st)
	}
}
