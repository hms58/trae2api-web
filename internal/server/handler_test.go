package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

// 模拟 SOLO SSE 响应（glm-5.2 回答"你好"）。
const soloSSE = "event:metadata\ndata:{\"model\":\"\",\"session_id\":\"s1\"}\n\n" +
	"event:output\ndata:{\"response\":\"你好\",\"reasoning_content\":\"想一下\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// newFakeUpstream 返回 ChatStream 走 fake 的 upstream.Client。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

// newPathUpstream 按请求路径分流的 fake 上游（用于区分不同上游端点的用例）。
func newPathUpstream(t *testing.T, behavior func(path string) (status int, body string)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body := behavior(r.URL.Path)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		AgentHost: "https://fake.example",
		UgHost:    "https://fake.example",
		OAuthHost: "https://fake.example",
		ClientID:  upstream.ClientID,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Cloud-IDE-JWT at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["reasoning_content"] != "想一下" {
		t.Errorf("reasoning=%q", msg["reasoning_content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

func TestChatRotatesOnPlanLimit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Cloud-IDE-JWT at-bad" {
			return 200, "event:error\ndata:{\"code\":1005,\"message\":\"plan limit\",\"extra\":{\"plan\":2}}\n\n", true
		}
		return 200, soloSSE, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Cloud-IDE-JWT at-bad"] != 1 || calls["Cloud-IDE-JWT at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
}

// TestChatStreamCooldownOnStreamError 流式请求中上游 event:error（如 5xx/参数错误）
// 应触发 NoteError 冷却（而非仅透传不冷却）。
func TestChatStreamCooldownOnStreamError(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "event:error\ndata:{\"code\":5001,\"message\":\"upstream broke\"}\n\n", true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 流内错误不应中断整个请求的 SSE 输出（仍有 event:error + [DONE]）。
	body := rec.Body.String()
	if !strings.Contains(body, "solo error") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q want error event + [DONE]", body)
	}
	st, _ := p.Status("u1")
	if st.ErrCount == 0 {
		t.Errorf("stream error should bump errCount: %+v", st)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `rate limited`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":1001,"msg":"login required"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatUnknownModel400(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"does-not-exist-xyz","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) != 32 {
		t.Errorf("models count=%d want 32", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("glm-5.2 missing")
	}
}

// provider 前缀（"前缀:模型名"）应剥离，仅用冒号后的 config_name 转发。
func TestMapModelStripsProviderPrefix(t *testing.T) {
	h := NewHandler(Config{DefaultModel: "glm-5.2", Pool: pool.New("")})
	cases := []struct{ in, want string }{
		{"trae-cn-local:glm-5.2", "glm-5.2"},
		{"glm-5.2", "glm-5.2"},
		{"any-vendor:DeepSeek-V4-Pro", "DeepSeek-V4-Pro"},
		{"provider:glm-5.2__dev", "glm-5.2"},
		{"glm-5.2__dev", "glm-5.2"},
		{"auto", "glm-5.2"},
		{"", "glm-5.2"},
	}
	for _, c := range cases {
		got, err := h.mapModel(c.in)
		if err != nil {
			t.Errorf("mapModel(%q) err=%v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("mapModel(%q)=%q want %q", c.in, got, c.want)
		}
	}
	// 前缀后为无效模型仍拒绝（不静默回退默认）
	if _, err := h.mapModel("provider:no-such-model"); err == nil {
		t.Error("unknown tail should still be rejected")
	}
	// 仅前缀、冒号后为空 → 视为畸形输入报错
	if _, err := h.mapModel("provider:"); err == nil {
		t.Error("trailing colon should be rejected")
	}
}

// 动态模型表必须带非 0 的 context_length（曾因 int64/int 接口比较陷阱全返回 0）。
func TestModelListContextLengthNonZero(t *testing.T) {
	infos := []upstream.ModelInfo{{ID: "a", ContextWindow: 0}, {ID: "b", ContextWindow: 200000}}
	// 直接验证包装逻辑：动态路径 + 静态路径都不应出现 0
	for _, m := range wrapModelEntries(infos) {
		if m["context_length"] == 0 || m["context_length"] == int64(0) {
			t.Errorf("model %v has zero context_length", m["id"])
		}
	}
	for _, m := range staticModels {
		if m["context_length"] == 0 {
			t.Errorf("static model %v has zero context_length", m["id"])
		}
	}
}

// resetModelsCache 清空包级动态模型缓存（成功缓存 1h / 负缓存 5min 会串测试）。
func resetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// 管理面板模型列表端点：静态回退时返回静态表 + source=static。
func TestAdminModelsEndpoint(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false // 动态拉取失败 → 回退静态表
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     up,
		DefaultModel: "glm-5.2",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var d struct {
		Source string           `json:"source"`
		Count  int              `json:"count"`
		Models []map[string]any `json:"models"`
		Default string          `json:"default"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Source != "static" {
		t.Errorf("source=%s want static", d.Source)
	}
	if d.Count != len(staticModels) || len(d.Models) != d.Count {
		t.Errorf("count=%d models=%d want %d", d.Count, len(d.Models), len(staticModels))
	}
	if d.Default != "glm-5.2" {
		t.Errorf("default=%s", d.Default)
	}
	for _, m := range d.Models {
		if m["context_length"] == nil || m["context_length"] == 0 || m["context_length"] == int64(0) {
			t.Errorf("model %v missing context_length", m["id"])
		}
	}
}

// 动态拉取成功 → source=dynamic 且带缓存时间。
func TestAdminModelsEndpointDynamic(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}},{"config_name":"kimi-k3"}]}`, false
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream:     up,
		DefaultModel: "glm-5.2",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/models", nil))
	var d struct {
		Source    string `json:"source"`
		Count     int    `json:"count"`
		FetchedAt string `json:"fetched_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Source != "dynamic" {
		t.Errorf("source=%s want dynamic", d.Source)
	}
	if d.Count != 2 {
		t.Errorf("count=%d want 2", d.Count)
	}
	if d.FetchedAt == "" {
		t.Error("fetched_at should be set for dynamic source")
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 无 key → 401
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key → 401
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key → 200（models）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	big := strings.Repeat("a", maxBodyBytes+1)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestAPIKeyAuthCaseInsensitivePrefix(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 大小写不同的 Bearer 前缀也应接受（按规范，前缀大小写不敏感）。
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "bearer test-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("lowercase bearer: code=%d", rec.Code)
	}
	// 错误 key 仍拒绝。
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
}

// TestAdminEndpointsRequireKey 管理面板所有数据接口共用 TW2A_API_KEY 鉴权。
func TestAdminEndpointsRequireKey(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "test-key",
	})
	// 面板页面本身可匿名获取（不含数据），数据接口全部 401。
	for _, path := range []string{
		"/admin/api/accounts",
		"/admin/api/credits",
		"/admin/api/accounts/u1/json",
		"/admin/api/login/result?pending_id=none",
	} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("no key GET %s: code=%d want 401", path, rec.Code)
		}
	}
	// 正确 key → 200。
	for _, path := range []string{
		"/admin/api/accounts",
		"/admin/api/accounts/u1/json",
	} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("right key GET %s: code=%d want 200", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin", nil))
	if rec.Code != 200 {
		t.Errorf("admin page: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("code=%d", rec.Code)
	}
}
