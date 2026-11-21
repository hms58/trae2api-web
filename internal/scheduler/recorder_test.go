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

// 父目录不存在（如 ./data）时自动创建，记录正常落盘。
func TestRecorderCreatesMissingDataDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "nested", "schedule-log.jsonl")
	r := NewRecorder(path, 0)
	r.Add(Record{StartedAt: time.Now().Format(timeLayout), Task: "refresh", Result: "success", DurationMS: 10, Summary: "startup"})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file should exist after Add: %v", err)
	}
	if !strings.Contains(string(raw), "startup") {
		t.Errorf("record missing in file: %s", raw)
	}
}

// 空池不执行任务、不产生记录（启动补跑/定时到点均如此）。
func TestEmptyPoolSkipsTasksAndRecords(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	s := newTestScheduler(f, pool.New(""), srv)
	s.RunRefreshNow()
	s.RunCheckinNow()
	if got := len(s.Records()); got != 0 {
		t.Errorf("empty pool should produce no records, got %d", got)
	}
	if f.refreshCalls.Load() != 0 || f.checkinCalls.Load() != 0 {
		t.Error("empty pool should not call upstream")
	}
}

func TestSchedulerUpdateHotReload(t *testing.T) {
	s := New(Config{})
	s.UpdateRuntime(ScheduleRuntime{CheckinEnabled: false, CheckinHour: 21, RefreshHours: []int{5, 15}, JitterMinutes: 7})
	cfg := s.snapshot()
	if cfg.CheckinEnabled {
		t.Error("checkin should be disabled after update")
	}
	if cfg.CheckinHour != 21 {
		t.Errorf("checkin_hour=%d want 21", cfg.CheckinHour)
	}
	if len(cfg.RefreshHours) != 2 || cfg.RefreshHours[0] != 5 || cfg.RefreshHours[1] != 15 {
		t.Errorf("refresh_hours=%v want [5 15]", cfg.RefreshHours)
	}
	if cfg.JitterMinutes != 7 {
		t.Errorf("jitter_minutes=%d want 7", cfg.JitterMinutes)
	}
	// 抖动越界封顶
	s.UpdateRuntime(ScheduleRuntime{CheckinEnabled: true, CheckinHour: 9, RefreshHours: []int{3}, JitterMinutes: 99})
	if got := s.snapshot().JitterMinutes; got != 30 {
		t.Errorf("jitter clamped=%d want 30", got)
	}
}

func TestJitterDelay(t *testing.T) {
	if d := jitterDelay(0); d != 0 {
		t.Errorf("jitter 0 should be exact, got %v", d)
	}
	if d := jitterDelay(-5); d != 0 {
		t.Errorf("negative jitter should be 0, got %v", d)
	}
	for i := 0; i < 50; i++ {
		d := jitterDelay(10)
		if d < 0 || d >= 10*time.Minute {
			t.Fatalf("jitter out of range: %v", d)
		}
	}
	// 上限封顶 30 分钟
	for i := 0; i < 20; i++ {
		if d := jitterDelay(99); d >= 30*time.Minute {
			t.Fatalf("jitter should clamp to 30min, got %v", d)
		}
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

// 已被人工签到过的账号：不重复领取，记录中单独计数并说明原因。
func TestRunCheckinAlreadyCheckedIn(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500, checkedIn: true}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)

	s := newTestScheduler(f, p, srv)
	s.RunCheckinNow()
	if f.claimCalls.Load() != 0 {
		t.Errorf("already checked in should not claim, calls=%d", f.claimCalls.Load())
	}
	rec := s.Records()[0]
	if rec.Result != "success" {
		t.Errorf("result=%s want success", rec.Result)
	}
	if !strings.Contains(rec.Summary, "已签到 1") {
		t.Errorf("summary=%q want contains 已签到 1", rec.Summary)
	}
	if !strings.Contains(rec.Summary, "人工") {
		t.Errorf("summary=%q want reason 人工", rec.Summary)
	}
}

// 硬禁用账号在保活轮次仍尝试 refresh：成功 = 复活。
func TestRunRefreshRevivesDisabledAccount(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, ApiHost: srv.URL}
	p.Add(a)
	p.Disable("u1", "session dead")

	s := newTestScheduler(f, p, srv)
	s.RunRefreshNow()

	st, _ := p.Status("u1")
	if st.Disabled {
		t.Errorf("disabled account should be revived after successful refresh: %+v", st)
	}
	if a.AccessToken != "newat" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
	recs := s.Records()
	if len(recs) != 1 || !strings.Contains(recs[0].Summary, "复活 1") {
		t.Errorf("summary should contain 复活 1: %+v", recs)
	}
	if recs[0].Result != "success" {
		t.Errorf("result=%s want success", recs[0].Result)
	}
}

// 每轮任务前重扫 auths 目录：手改凭证文件自动生效。
func TestRescanAuthsPicksUpNewFile(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	dir := t.TempDir()
	p := pool.New("")
	s := newTestScheduler(f, p, srv)
	s.cfg.AuthDir = dir

	if p.AuthByUID("u9") != nil {
		t.Fatal("u9 should not exist before rescan")
	}
	// 手写一个凭证文件（扁平形）
	content := `{"accessToken":"at9","refreshToken":"rt9","uid":"u9","expiresAt":9999999999}`
	if err := os.WriteFile(filepath.Join(dir, "trae-u9.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s.RunCheckinNow() // 任务入口触发 rescan
	if p.AuthByUID("u9") == nil {
		t.Error("rescan should import new auth file")
	}
}
