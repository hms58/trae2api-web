package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trae2api-web/internal/auth"
	"trae2api-web/internal/pool"
)

func TestRecorderFilePersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sched.jsonl")
	r := NewRecorder(path, 0)
	r.Add(Record{StartedAt: time.Now().Format(timeLayout), Task: "checkin", Result: "success", DurationMS: 120, Summary: "账号：成功 1、失败 0、跳过 0"})
	r.Add(Record{StartedAt: time.Now().Format(timeLayout), Task: "refresh", Result: "skipped", DurationMS: 5, Summary: "账号：成功 0、失败 0、跳过 1"})

	if got := len(r.List()); got != 2 {
		t.Fatalf("in-memory records=%d want 2", got)
	}
	if r.List()[0].Task != "refresh" {
		t.Errorf("newest should be first, got %s", r.List()[0].Task)
	}
	// 重新加载：文件内容一致
	r2 := NewRecorder(path, 0)
	if got := len(r2.List()); got != 2 {
		t.Fatalf("reloaded records=%d want 2", got)
	}
	if r2.List()[1].Summary == "" || !strings.Contains(r2.List()[1].Summary, "成功 1") {
		t.Errorf("summary lost: %q", r2.List()[1].Summary)
	}
}

func TestRecorderPrunesExpiredOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sched.jsonl")
	old := time.Now().AddDate(0, 0, -60).Format(timeLayout)
	fresh := time.Now().Format(timeLayout)
	content := `{"started_at":"` + old + `","task":"checkin","result":"success","duration_ms":1,"summary":"old"}` + "\n" +
		`{"started_at":"` + fresh + `","task":"checkin","result":"success","duration_ms":1,"summary":"fresh"}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// 保留 30 天：60 天前的应被裁剪并重写落盘
	r := NewRecorder(path, 30)
	recs := r.List()
	if len(recs) != 1 || recs[0].Summary != "fresh" {
		t.Fatalf("records=%+v want only fresh", recs)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "old") {
		t.Errorf("file should be rewritten without expired entry: %s", raw)
	}
	if strings.Contains(string(raw), "fresh") {
		t.Logf("file content ok")
	} else {
		t.Errorf("fresh entry missing in file: %s", raw)
	}
}

func TestRecorderBadLinesTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sched.jsonl")
	if err := os.WriteFile(path, []byte("{broken json\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRecorder(path, 0)
	if got := len(r.List()); got != 0 {
		t.Errorf("bad lines should be skipped, got %d", got)
	}
}

func TestSchedulerUpdateHotReload(t *testing.T) {
	s := New(Config{})
	s.Update(21, []int{5, 15})
	cfg := s.snapshot()
	if cfg.CheckinHour != 21 {
		t.Errorf("checkin_hour=%d want 21", cfg.CheckinHour)
	}
	if len(cfg.RefreshHours) != 2 || cfg.RefreshHours[0] != 5 || cfg.RefreshHours[1] != 15 {
		t.Errorf("refresh_hours=%v want [5 15]", cfg.RefreshHours)
	}
}

func TestRunCheckinWritesRecord(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	recs := s.Records()
	if len(recs) != 1 {
		t.Fatalf("records=%d want 1", len(recs))
	}
	rec := recs[0]
	if rec.Task != "checkin" {
		t.Errorf("task=%s want checkin", rec.Task)
	}
	if rec.Result != "success" {
		t.Errorf("result=%s want success", rec.Result)
	}
	if rec.DurationMS < 0 {
		t.Errorf("duration_ms=%d", rec.DurationMS)
	}
	if !strings.Contains(rec.Summary, "成功 1") {
		t.Errorf("summary=%q want contains 成功 1", rec.Summary)
	}
	if _, err := time.ParseInLocation(timeLayout, rec.StartedAt, time.Local); err != nil {
		t.Errorf("started_at=%q parse: %v", rec.StartedAt, err)
	}
}
