// chatlog_test.go 调用记录：裁剪、埋点、面板查询/统计。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/upstream"
)

func newTestChatLog(t *testing.T) *ChatLog {
	t.Helper()
	return NewChatLog(filepath.Join(t.TempDir(), "chatlog.jsonl"))
}

func readChatLogFile(t *testing.T, cl *ChatLog) []ChatLogEntry {
	t.Helper()
	raw, err := os.ReadFile(cl.path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var out []ChatLogEntry
	for _, ln := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if ln == "" {
			continue
		}
		var e ChatLogEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("parse entry: %v (%s)", err, ln)
		}
		out = append(out, e)
	}
	return out
}

func TestChatLogRecordAndTrim(t *testing.T) {
	cl := newTestChatLog(t)
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour).Format(time.RFC3339)
	fresh := now.Format(time.RFC3339)
	for _, e := range []ChatLogEntry{
		{Ts: old, Model: "glm-5.2", Status: "ok"},
		{Ts: old, Model: "glm-5", Status: "ok"},
		{Ts: fresh, Model: "glm-5.2", Status: "ok"},
	} {
		cl.Record(e)
	}
	if got := readChatLogFile(t, cl); len(got) != 3 {
		t.Fatalf("pre-trim lines=%d", len(got))
	}
	cl.mu.Lock()
	cl.trimLocked()
	cl.mu.Unlock()
	got := readChatLogFile(t, cl)
	if len(got) != 1 || got[0].Model != "glm-5.2" || got[0].Ts != fresh {
		t.Fatalf("time trim: got %+v", got)
	}
}

func TestChatLogTrimBySize(t *testing.T) {
	oldMax := chatLogMaxBytes
	t.Cleanup(func() { chatLogMaxBytes = oldMax })
	chatLogMaxBytes = 512 // 测试注入小上限

	cl := newTestChatLog(t)
	now := time.Now().Format(time.RFC3339)
	// 3 条各 ~300 字节（长 preview），总超 512 → 只应保留最新 1~2 条。
	for i := 0; i < 3; i++ {
		cl.Record(ChatLogEntry{Ts: now, Model: "m", Status: "ok", Preview: strings.Repeat("x", 100)})
	}
	cl.mu.Lock()
	cl.trimLocked()
	cl.mu.Unlock()
	if st, err := os.Stat(cl.path); err != nil || st.Size() > int64(chatLogMaxBytes) {
		t.Fatalf("size trim: size=%v err=%v", st, err)
	}
	got := readChatLogFile(t, cl)
	if len(got) == 0 || len(got) >= 3 {
		t.Fatalf("size trim kept=%d", len(got))
	}
}

func TestChatCompletionsLogsNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, soloSSE, true
	})
	logFile := filepath.Join(t.TempDir(), "chatlog.jsonl")
	h := NewHandler(Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    up,
		ChatLogFile: logFile,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"帮我写个排序"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	entries := readChatLogFile(t, h.chatlog)
	if len(entries) != 1 {
		t.Fatalf("entries=%d", len(entries))
	}
	e := entries[0]
	if e.Status != "ok" || e.UID != "u1" || e.Model != "glm-5.2" || e.ReqModel != "glm-5.2" {
		t.Fatalf("entry=%+v", e)
	}
	if e.Stream {
		t.Errorf("stream should be false")
	}
	// soloSSE 带 token_usage 事件：prompt_tokens=5, completion_tokens=2。
	if e.ReqTokens != 5 || e.RespTokens != 2 {
		t.Errorf("tokens=%d/%d want 5/2", e.ReqTokens, e.RespTokens)
	}
	if !strings.Contains(e.Preview, "帮我写个排序") {
		t.Errorf("preview=%q", e.Preview)
	}
	if e.LatencyMS < 0 {
		t.Errorf("latency=%d", e.LatencyMS)
	}
	if _, err := time.Parse(time.RFC3339, e.Ts); err != nil {
		t.Errorf("ts=%q", e.Ts)
	}
}

func TestChatCompletionsLogsStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, soloSSE, true
	})
	h := NewHandler(Config{
		Pool:        testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:    up,
		ChatLogFile: filepath.Join(t.TempDir(), "chatlog.jsonl"),
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	entries := readChatLogFile(t, h.chatlog)
	if len(entries) != 1 {
		t.Fatalf("entries=%d", len(entries))
	}
	e := entries[0]
	if !e.Stream || e.Status != "ok" || e.UID != "u1" {
		t.Fatalf("entry=%+v", e)
	}
	if e.RespTokens <= 0 {
		t.Errorf("resp tokens should be estimated from stream bytes, got %d", e.RespTokens)
	}
}

func TestChatCompletionsLogsFailures(t *testing.T) {
	// 无可用账号 → no_account；未知模型 → client_error。
	h := NewHandler(Config{
		Pool:        testPoolWith(),
		Upstream:    upstream.New(),
		ChatLogFile: filepath.Join(t.TempDir(), "chatlog.jsonl"),
	})
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"no-such-model","messages":[]}`)))
	entries := readChatLogFile(t, h.chatlog)
	if len(entries) != 2 {
		t.Fatalf("entries=%d", len(entries))
	}
	if entries[0].Status != "no_account" {
		t.Errorf("entry0 status=%q", entries[0].Status)
	}
	if entries[1].Status != "client_error" {
		t.Errorf("entry1 status=%q err=%q", entries[1].Status, entries[1].Err)
	}
}

func TestAdminChatLogsFiltersAndPagination(t *testing.T) {
	cl := newTestChatLog(t)
	now := time.Now().Format(time.RFC3339)
	for _, e := range []ChatLogEntry{
		{Ts: now, Model: "glm-5.2", UID: "u1", Status: "ok", Preview: "first hello"},
		{Ts: now, Model: "glm-5", UID: "u2", Status: "upstream_error", Err: "boom", Preview: "second world"},
		{Ts: now, Model: "glm-5.2", UID: "u1", Status: "ok", Preview: "third hello"},
	} {
		cl.Record(e)
	}
	h := NewHandler(Config{Pool: testPoolWith(), ChatLogFile: cl.path})

	// 过滤 status。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/chatlogs?status=ok", nil))
	var page ChatLogPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("resp: %v", rec.Body)
	}
	if page.Total != 2 || len(page.Records) != 2 {
		t.Fatalf("status filter: total=%d recs=%d", page.Total, len(page.Records))
	}
	// 倒序：最新（third）在前。
	if page.Records[0].Preview != "third hello" {
		t.Errorf("order: first=%q", page.Records[0].Preview)
	}
	// 关键词匹配 preview 与 err。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/chatlogs?q=world", nil))
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 1 || page.Records[0].Err != "boom" {
		t.Fatalf("q filter: %+v", page)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/chatlogs?q=BOOM", nil))
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 1 {
		t.Fatalf("q filter case-insensitive: total=%d", page.Total)
	}
	// model + uid 组合过滤。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/chatlogs?model=glm-5.2&uid=u1", nil))
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 2 {
		t.Fatalf("model+uid filter: total=%d", page.Total)
	}
	// facets。
	if len(page.Models) != 2 || len(page.UIDs) != 2 {
		t.Fatalf("facets: models=%v uids=%v", page.Models, page.UIDs)
	}
	// 分页：page_size=1 page=2 → 1 条且是中间那条。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/chatlogs?model=glm-5.2&uid=u1&page=2&page_size=1", nil))
	json.Unmarshal(rec.Body.Bytes(), &page)
	if page.Total != 2 || len(page.Records) != 1 || page.Records[0].Preview != "first hello" {
		t.Fatalf("pagination: %+v", page)
	}
}

func TestAdminChatLogStats(t *testing.T) {
	cl := newTestChatLog(t)
	now := time.Now()
	cl.Record(ChatLogEntry{Ts: now.Format(time.RFC3339), Model: "glm-5.2", Status: "ok", ReqTokens: 10, RespTokens: 20})
	cl.Record(ChatLogEntry{Ts: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339), Model: "glm-5.2", Status: "ok", ReqTokens: 100, RespTokens: 200})
	cl.Record(ChatLogEntry{Ts: now.Format(time.RFC3339), Model: "glm-5", Status: "upstream_error", ReqTokens: 5})
	h := NewHandler(Config{Pool: testPoolWith(), ChatLogFile: cl.path})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/chatlogs/stats", nil))
	var st map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("resp: %v", rec.Body)
	}
	if st["today_calls"].(float64) != 2 {
		t.Errorf("today_calls=%v", st["today_calls"])
	}
	if st["today_ok"].(float64) != 1 {
		t.Errorf("today_ok=%v", st["today_ok"])
	}
	if st["today_req_tokens"].(float64) != 15 {
		t.Errorf("today_req_tokens=%v", st["today_req_tokens"])
	}
	if st["today_resp_tokens"].(float64) != 20 {
		t.Errorf("today_resp_tokens=%v", st["today_resp_tokens"])
	}
	if st["file_bytes"].(float64) <= 0 {
		t.Errorf("file_bytes=%v", st["file_bytes"])
	}
}

func TestFirstUserPreview(t *testing.T) {
	got := firstUserPreview([]byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"你好，世界"}]}`))
	if got != "你好，世界" {
		t.Fatalf("got %q", got)
	}
	// 多模态数组 content。
	got = firstUserPreview([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]}]}`))
	if !strings.Contains(got, "part1") || !strings.Contains(got, "part2") {
		t.Fatalf("multimodal got %q", got)
	}
	// 截断 200 rune。
	got = firstUserPreview([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("长", 300) + `"}]}`))
	runes := len([]rune(got))
	if runes != 201 { // 200 + 省略号
		t.Fatalf("truncate runes=%d", runes)
	}
}
