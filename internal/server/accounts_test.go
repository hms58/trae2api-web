package server

import (
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
