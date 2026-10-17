// recorder.go 定时任务执行记录：本地 JSONL 文件 + 保留天数裁剪。
// 一行一条记录，启动时加载并裁剪过期条目，超期条目在下次写入时重写文件去掉。
package scheduler

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// Record 一次定时任务的执行记录（管理面板展示）。
type Record struct {
	StartedAt  string `json:"started_at"`  // "2006-01-02 15:04:05"
	Task       string `json:"task"`        // checkin | refresh
	Result     string `json:"result"`      // success | partial | failed | skipped
	DurationMS int64  `json:"duration_ms"` // 执行耗时（毫秒）
	Summary    string `json:"summary"`     // 执行总结（账号数 + 明细 + 错误摘要）
}

const timeLayout = "2006-01-02 15:04:05"

// Recorder 文件型执行记录器（并发安全）。
type Recorder struct {
	mu      sync.Mutex
	path    string
	retDays int      // 保留天数，<=0 = 永久保留
	records []Record // 内存副本，最新在前
}

// NewRecorder 构建记录器并加载已有文件。path 为空时退化为纯内存（不落盘）。
// retDays <= 0 表示永久保留；>0 时加载即裁剪超期条目。
func NewRecorder(path string, retDays int) *Recorder {
	r := &Recorder{path: path, retDays: retDays}
	r.load()
	return r
}

// cutoff 返回保留起点时间；retDays <= 0 时返回零值（不裁剪）。
func (r *Recorder) cutoff() time.Time {
	if r.retDays <= 0 {
		return time.Time{}
	}
	return time.Now().AddDate(0, 0, -r.retDays)
}

// load 读入文件全部条目（跳过坏行），按需裁剪后重写。
// 注意：必须先关闭文件再做 rewrite（Windows 上 rename 替换被占用文件会失败）。
func (r *Recorder) load() {
	if r.path == "" {
		return
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return // 文件不存在 = 首次运行
	}
	var recs []Record
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // 容忍半行/坏行
		}
		recs = append(recs, rec)
	}
	r.records = reverse(prune(recs, r.cutoff())) // 裁剪超期 + 文件序旧→新统一为最新在前
	if len(r.records) != len(recs) {
		_ = r.rewrite() // 有裁剪 → 立即重写落盘
	}
}

// reverse 原地反转切片（新切片返回，避免动调用方底层数组）。
func reverse(recs []Record) []Record {
	out := make([]Record, len(recs))
	for i, rec := range recs {
		out[len(recs)-1-i] = rec
	}
	return out
}

// prune 过滤掉早于 cutoff 的条目（cutoff 零值 = 全部保留）。
func prune(recs []Record, cutoff time.Time) []Record {
	if cutoff.IsZero() {
		return recs
	}
	out := recs[:0:0]
	for _, rec := range recs {
		t, err := time.ParseInLocation(timeLayout, rec.StartedAt, time.Local)
		if err != nil || !t.Before(cutoff) {
			out = append(out, rec)
		}
	}
	return out
}

// Add 追加一条记录：内存置顶 + 追加文件行；有裁剪时重写文件。
// 文件追加失败不阻塞任务执行，记录仍进内存（页面可见）。
func (r *Recorder) Add(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path != "" {
		_ = appendLine(r.path, rec)
	}
	r.records = append([]Record{rec}, r.records...)
	if kept := prune(r.records, r.cutoff()); len(kept) != len(r.records) {
		r.records = kept
		if r.path != "" {
			_ = r.rewrite()
		}
	}
}

// List 返回全部记录（最新在前，副本）。
func (r *Recorder) List() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, len(r.records))
	copy(out, r.records)
	return out
}

// SetRetention 修改保留天数（页面/配置热更新），并立即裁剪。
func (r *Recorder) SetRetention(days int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retDays = days
	if kept := prune(r.records, r.cutoff()); len(kept) != len(r.records) {
		r.records = kept
		if r.path != "" {
			_ = r.rewrite()
		}
	}
}

// appendLine 追加一行 JSON（O_APPEND，并发追加安全）。
func appendLine(path string, rec Record) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return nil
}

// rewrite 用内存记录全量重写文件（裁剪后调用）。调用方需持锁。
func (r *Recorder) rewrite() error {
	if r.path == "" {
		return nil
	}
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, rec := range r.records {
		raw, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
