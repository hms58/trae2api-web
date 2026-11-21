// chatlog.go /v1/chat/completions 调用记录：摘要级 JSONL 落盘 + 30 天/100MB 裁剪 + 面板查询。
package server

import (
	"bufio"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 裁剪阈值用 var 便于测试注入；chatLogTrimEvery 固定。
var (
	chatLogMaxAge   = 30 * 24 * time.Hour // 保留时长下限
	chatLogMaxBytes = 100 << 20           // 文件大小上限 100MB
)

const chatLogTrimEvery = 500 // 每写 500 条触发一次裁剪

// ChatLogEntry 单条调用记录（JSONL 一行）。
type ChatLogEntry struct {
	Ts         string `json:"ts"`                    // RFC3339
	Model      string `json:"model"`                 // 映射后 config_name
	ReqModel   string `json:"req_model,omitempty"`   // 客户端原始 model
	UID        string `json:"uid,omitempty"`         // 最终服务的账号
	Attempts   int    `json:"attempts,omitempty"`    // 换号次数
	Stream     bool   `json:"stream,omitempty"`
	Status     string `json:"status"` // ok / client_error / upstream_error / no_account
	Err        string `json:"err,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
	ReqTokens  int64  `json:"req_tokens"`  // 粗估
	RespTokens int64  `json:"resp_tokens"` // 粗估（非流式优先用上游 usage）
	Preview    string `json:"preview,omitempty"` // 首条 user 消息截断预览
}

// ChatLog 调用记录存储：追加写单 JSONL 文件，定期裁剪。
// 写入失败置 broken 静默跳过（不刷错误日志、不影响请求主流程），
// 下次 trim 成功后自愈。nil 接收者所有方法 no-op（未启用记录时零侵入）。
type ChatLog struct {
	mu      sync.Mutex
	path    string
	written int  // 自上次裁剪以来写入条数
	broken  bool // 写入失败后暂停写入，trim 成功后恢复
}

// NewChatLog 构建记录器；目录不存在自动创建。
func NewChatLog(path string) *ChatLog {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	return &ChatLog{path: path}
}

// Record 追加一条记录；每写 chatLogTrimEvery 条触发一次裁剪。
func (cl *ChatLog) Record(e ChatLogEntry) {
	if cl == nil {
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.broken {
		return
	}
	f, err := os.OpenFile(cl.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		if _, err = f.Write(append(raw, '\n')); err == nil {
			cl.written++
		}
		_ = f.Close()
	}
	if err != nil {
		log.Printf("chatlog write failed: %v (写入暂停，裁剪成功后恢复)", err)
		cl.broken = true
		return
	}
	if cl.written >= chatLogTrimEvery {
		cl.trimLocked()
	}
}

// trimLocked 按 30 天下限 + 100MB 上限裁剪（从文件头丢弃旧行，重写临时文件后原子替换）。
// 调用方须持 cl.mu。
func (cl *ChatLog) trimLocked() {
	cl.written = 0
	cutoff := time.Now().Add(-chatLogMaxAge)
	lines, err := readLines(cl.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("chatlog trim read failed: %v", err)
		}
		return
	}
	// 时间裁剪：保留 30 天内的行（解析失败的行视为坏行丢弃，自愈）。
	type kept struct{ raw string; size int }
	var keep []kept
	var total int
	for _, ln := range lines {
		var e ChatLogEntry
		if json.Unmarshal([]byte(ln), &e) != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.Ts)
		if err != nil || ts.Before(cutoff) {
			continue
		}
		keep = append(keep, kept{ln, len(ln) + 1})
		total += len(ln) + 1
	}
	// 大小裁剪：从最旧的一端（keep 头部）丢弃直到不超限。
	drop := 0
	for total > chatLogMaxBytes && drop < len(keep) {
		total -= keep[drop].size
		drop++
	}
	if drop == 0 && len(keep) == len(lines) {
		return // 无需重写
	}
	out := make([]byte, 0, total)
	for i := drop; i < len(keep); i++ {
		out = append(out, keep[i].raw...)
		out = append(out, '\n')
	}
	tmp := cl.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		log.Printf("chatlog trim write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, cl.path); err != nil {
		log.Printf("chatlog trim rename failed: %v", err)
		_ = os.Remove(tmp)
		return
	}
	cl.broken = false // 裁剪成功，恢复写入
}

// readLines 按行读整个 JSONL 文件（不含行尾换行；空行跳过）。
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		if ln := strings.TrimSpace(sc.Text()); ln != "" {
			lines = append(lines, ln)
		}
	}
	return lines, sc.Err()
}

// ---------------------------------------------------------------------------
// 查询（面板 API）
// ---------------------------------------------------------------------------

// ChatLogFilter 查询条件；Page 从 1 起，PageSize 默认 50 上限 200。
type ChatLogFilter struct {
	Model    string
	Status   string
	UID      string
	Q        string // 对 preview / err 子串匹配
	Page     int
	PageSize int
}

// ChatLogPage 倒序分页结果；Models/UIDs 为去重列表（供筛选下拉）。
type ChatLogPage struct {
	Total   int            `json:"total"`
	Page    int            `json:"page"`
	PageSize int           `json:"page_size"`
	Records []ChatLogEntry `json:"records"`
	Models  []string       `json:"models"`
	UIDs    []string       `json:"uids"`
}

// Query 全文件倒序扫描后过滤分页（摘要级单文件 ≤100MB，无索引足够）。
func (cl *ChatLog) Query(f ChatLogFilter) ChatLogPage {
	page := ChatLogPage{Page: f.Page, PageSize: f.PageSize, Records: []ChatLogEntry{}, Models: []string{}, UIDs: []string{}}
	if cl == nil {
		return page
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	lines, err := readLines(cl.path)
	if err != nil {
		return page
	}
	q := strings.ToLower(f.Q)
	seenModel, seenUID := map[string]bool{}, map[string]bool{}
	// 倒序扫描（文件尾部最新）， facets 按最新出现顺序。
	for i := len(lines) - 1; i >= 0; i-- {
		var e ChatLogEntry
		if json.Unmarshal([]byte(lines[i]), &e) != nil {
			continue
		}
		if !seenModel[e.Model] {
			seenModel[e.Model] = true
			page.Models = append(page.Models, e.Model)
		}
		if e.UID != "" && !seenUID[e.UID] {
			seenUID[e.UID] = true
			page.UIDs = append(page.UIDs, e.UID)
		}
		if f.Model != "" && e.Model != f.Model {
			continue
		}
		if f.Status != "" && e.Status != f.Status {
			continue
		}
		if f.UID != "" && e.UID != f.UID {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Preview), q) && !strings.Contains(strings.ToLower(e.Err), q) {
			continue
		}
		page.Total++
		start := (f.Page - 1) * f.PageSize
		if page.Total > start && len(page.Records) < f.PageSize {
			page.Records = append(page.Records, e)
		}
	}
	return page
}

// Stats 面板统计：今日调用/成功/token 估算 + 文件当前大小。
func (cl *ChatLog) Stats() map[string]any {
	if cl == nil {
		return map[string]any{"today_calls": 0, "today_ok": 0, "today_req_tokens": 0, "today_resp_tokens": 0, "file_bytes": 0}
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	out := map[string]any{"today_calls": 0, "today_ok": 0, "today_req_tokens": int64(0), "today_resp_tokens": int64(0)}
	today := time.Now().Format("2006-01-02")
	if lines, err := readLines(cl.path); err == nil {
		for _, ln := range lines {
			var e ChatLogEntry
			if json.Unmarshal([]byte(ln), &e) != nil || !strings.HasPrefix(e.Ts, today) {
				continue
			}
			out["today_calls"] = out["today_calls"].(int) + 1
			if e.Status == "ok" {
				out["today_ok"] = out["today_ok"].(int) + 1
			}
			out["today_req_tokens"] = out["today_req_tokens"].(int64) + e.ReqTokens
			out["today_resp_tokens"] = out["today_resp_tokens"].(int64) + e.RespTokens
		}
	}
	if st, err := os.Stat(cl.path); err == nil {
		out["file_bytes"] = st.Size()
	} else {
		out["file_bytes"] = int64(0)
	}
	return out
}

// ---------------------------------------------------------------------------
// 记录辅助
// ---------------------------------------------------------------------------

// estimateTokens 粗估 token 数：ASCII 约 4 字符/token，非 ASCII（中文等）约 1 字符/token。
func estimateTokens(s string) int64 {
	units := 0
	for _, r := range s {
		if r < 128 {
			units++
		} else {
			units += 4
		}
	}
	return int64(units / 4)
}

// truncateRunes 按 rune 截断，超长补省略号。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// firstUserPreview 提取请求体中首条 user 消息文本，截断 200 字。
// content 兼容字符串与 OpenAI 多模态数组（取 text 片段拼接）。
func firstUserPreview(body []byte) string {
	var m struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	for _, msg := range m.Messages {
		if msg.Role != "user" {
			continue
		}
		return truncateRunes(contentText(msg.Content), 200)
	}
	return ""
}

func contentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
				b.WriteString("\n")
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// countWriter 包装 ResponseWriter 统计流式响应写出字节数。
type countWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

func (c *countWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
