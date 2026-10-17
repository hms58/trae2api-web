// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
// 每次执行写一条 Record 到 Recorder（JSONL 文件，按保留天数裁剪）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"trae2api-web/internal/pool"
	"trae2api-web/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool          *pool.Pool
	Upstream      *upstream.Client
	CheckinHour   int           // 每日签到小时，默认 9
	RefreshHours  []int         // token 预刷新小时，默认 [3]
	RefreshSkew   time.Duration // 预刷新窗口，默认 24h
	LogFile       string        // 执行记录文件（JSONL），空 = 不落盘
	RetentionDays int           // 执行记录保留天数，默认 30，<=0 永久
}

// Scheduler 调度器。
type Scheduler struct {
	mu  sync.RWMutex // 保护 cfg 的 CheckinHour/RefreshHours/RetentionDays 热更新
	cfg Config
	rec *Recorder
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.CheckinHour < 0 {
		cfg.CheckinHour = 9
	}
	if len(cfg.RefreshHours) == 0 {
		cfg.RefreshHours = []int{3}
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	if cfg.RetentionDays == 0 {
		cfg.RetentionDays = 30
	}
	return &Scheduler{
		cfg: cfg,
		rec: NewRecorder(cfg.LogFile, cfg.RetentionDays),
	}
}

// snapshot 返回当前调度配置副本（热更新安全）。
func (s *Scheduler) snapshot() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update 热更新签到/刷新小时（页面保存后调用，主循环下一轮生效）。
func (s *Scheduler) Update(checkinHour int, refreshHours []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.CheckinHour = checkinHour
	s.cfg.RefreshHours = append([]int{}, refreshHours...)
}

// SetRetention 热更新执行记录保留天数，并立即裁剪。
func (s *Scheduler) SetRetention(days int) {
	s.mu.Lock()
	s.cfg.RetentionDays = days
	s.mu.Unlock()
	if s.rec != nil {
		s.rec.SetRetention(days)
	}
}

// Records 返回执行记录（最新在前）。
func (s *Scheduler) Records() []Record {
	if s.rec == nil {
		return nil
	}
	return s.rec.List()
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		cfg := s.snapshot()
		all := append(append([]int{}, cfg.RefreshHours...), cfg.CheckinHour)
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			cfg := s.snapshot()
			h := time.Now().Hour()
			if contains(cfg.RefreshHours, h) {
				s.RunRefreshNow()
			}
			if cfg.CheckinHour == h {
				s.RunCheckinNow()
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	start := time.Now()
	summary, result := s.doCheckin()
	s.record("checkin", start, result, summary)
}

func (s *Scheduler) doCheckin() (summary, result string) {
	var ok, already, failed, skipped int
	var errs []string
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			skipped++
			continue
		}
		// 签到（status → 未签到则 claim）
		checkedIn, _, enable, err := s.cfg.Upstream.CheckinStatus(a)
		if err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s status: %v", shortUID(st.UID), err))
		} else if !checkedIn && enable {
			if err := s.cfg.Upstream.CheckinClaim(a); err != nil {
				failed++
				errs = append(errs, fmt.Sprintf("%s claim: %v", shortUID(st.UID), err))
			} else {
				ok++
				log.Printf("checkin %s: ok", st.UID)
			}
		} else if checkedIn {
			already++
			log.Printf("checkin %s: already checked in", st.UID)
		}
		// 查积分 + 解冻
		remain, err := s.cfg.Upstream.UserEntUsage(a)
		if err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s ent-usage: %v", shortUID(st.UID), err))
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
	}
	return buildSummary(ok+already, failed, skipped, errs), classify(ok+already, failed, skipped)
}

// RunRefreshNow 立即对所有账号刷新 token；session 失效的自动禁用。
func (s *Scheduler) RunRefreshNow() {
	start := time.Now()
	summary, result := s.doRefresh()
	s.record("refresh", start, result, summary)
}

func (s *Scheduler) doRefresh() (summary, result string) {
	var ok, failed, skipped int
	var errs []string
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			skipped++
			continue
		}
		if !a.NeedsRefresh(s.cfg.RefreshSkew) {
			skipped++
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s: %v", shortUID(st.UID), err))
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
		ok++
	}
	return buildSummary(ok, failed, skipped, errs), classify(ok, failed, skipped)
}

// record 写一条执行记录（并发安全，文件追加失败不 panic）。
func (s *Scheduler) record(task string, start time.Time, result, summary string) {
	if s.rec == nil {
		return
	}
	s.rec.Add(Record{
		StartedAt:  start.Format(timeLayout),
		Task:       task,
		Result:     result,
		DurationMS: time.Since(start).Milliseconds(),
		Summary:    summary,
	})
}

// classify 汇总结果：success / partial / failed / skipped。
func classify(ok, failed, skipped int) string {
	if ok+failed == 0 {
		return "skipped"
	}
	if failed == 0 {
		return "success"
	}
	if ok == 0 {
		return "failed"
	}
	return "partial"
}

// buildSummary 生成执行总结：账号数 + 成功/失败/跳过 + 错误摘要（截断）。
func buildSummary(ok, failed, skipped int, errs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "账号：成功 %d、失败 %d、跳过 %d", ok, failed, skipped)
	if len(errs) > 0 {
		msg := strings.Join(errs, "；")
		const max = 300
		if len(msg) > max {
			msg = msg[:max] + "…"
		}
		fmt.Fprintf(&b, "｜错误：%s", msg)
	}
	return b.String()
}

// shortUID 账号 uid 截断展示（保留前 8 位）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
