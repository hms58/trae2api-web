// Package scheduler 定时任务：每日签到 + token 预刷新（保活）。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
// 每次执行写一条 Record 到 Recorder（JSONL 文件，按保留天数裁剪）。
//
// 可靠性设计：
//   - 启动即补跑一次保活（幂等，只刷临期），消除宕机跨过刷新窗口的漏保活；
//   - 每轮任务前重扫 auths 目录，手改的凭证文件自动生效；
//   - 硬禁用（session dead）账号在保活轮次仍尝试 refresh，成功即复活；
//   - 触发时刻可加随机抖动（jitter_minutes，0 = 准点，上限 30），避免同一时刻打点。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"trae2api-web/internal/auth"
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
	AuthDir       string        // 凭证目录；非空时每轮任务前重扫（手改凭证自动生效）
	CheckinEnabled bool         // 签到开关（config 默认 true）
	JitterMinutes  int          // 触发随机抖动分钟数，0 = 准点，>30 按 30 封顶
}

// ScheduleRuntime 页面可热更新的调度参数。
type ScheduleRuntime struct {
	CheckinEnabled bool
	CheckinHour    int
	RefreshHours   []int
	JitterMinutes  int
}

// Scheduler 调度器。
type Scheduler struct {
	mu  sync.RWMutex // 保护 cfg 的热更新字段
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
	if cfg.JitterMinutes < 0 {
		cfg.JitterMinutes = 0
	}
	if cfg.JitterMinutes > 30 {
		cfg.JitterMinutes = 30
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

// runtimeOf 提取可热更新字段。
func runtimeOf(cfg Config) ScheduleRuntime {
	return ScheduleRuntime{
		CheckinEnabled: cfg.CheckinEnabled,
		CheckinHour:    cfg.CheckinHour,
		RefreshHours:   append([]int{}, cfg.RefreshHours...),
		JitterMinutes:  cfg.JitterMinutes,
	}
}

// UpdateRuntime 热更新调度参数（页面保存后调用，主循环下一轮生效）。
func (s *Scheduler) UpdateRuntime(rt ScheduleRuntime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rt.JitterMinutes < 0 {
		rt.JitterMinutes = 0
	}
	if rt.JitterMinutes > 30 {
		rt.JitterMinutes = 30
	}
	if rt.CheckinHour < 0 {
		rt.CheckinHour = 0
	}
	if rt.CheckinHour > 23 {
		rt.CheckinHour = 23
	}
	if len(rt.RefreshHours) == 0 {
		rt.RefreshHours = []int{3}
	}
	s.cfg.CheckinEnabled = rt.CheckinEnabled
	s.cfg.CheckinHour = rt.CheckinHour
	s.cfg.RefreshHours = append([]int{}, rt.RefreshHours...)
	s.cfg.JitterMinutes = rt.JitterMinutes
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

// jitterDelay 随机抖动时长：n <= 0 返回 0（准点）；n > 0 返回 [0, n) 分钟内随机，封顶 30 分钟。
func jitterDelay(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	if n > 30 {
		n = 30
	}
	return time.Duration(rand.IntN(n*60)) * time.Second
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	// 启动补跑：立即做一次保活（幂等，只刷临期/复活探测），
	// 消除进程宕机/重启跨过刷新窗口导致的漏保活。
	s.RunRefreshNow()
	for {
		cfg := s.snapshot()
		all := append(append([]int{}, cfg.RefreshHours...), cfg.CheckinHour)
		next := nextFire(time.Now(), all)
		if d := jitterDelay(cfg.JitterMinutes); d > 0 {
			next = next.Add(d) // 抖动 < 60min，不改变触发小时
		}
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
			if cfg.CheckinEnabled && cfg.CheckinHour == h {
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

// rescanAuths 重扫凭证目录并同步进池：手改的 trae-*.json 自动生效。
func (s *Scheduler) rescanAuths() {
	if s.cfg.AuthDir == "" {
		return
	}
	auths, err := auth.LoadDir(s.cfg.AuthDir)
	if err != nil {
		log.Printf("rescan auth dir %s: %v", s.cfg.AuthDir, err)
		return
	}
	s.cfg.Pool.SyncToDir(auths)
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用/软关闭的跳过。
func (s *Scheduler) RunCheckinNow() {
	s.rescanAuths()
	start := time.Now()
	summary, result := s.doCheckin()
	s.record("checkin", start, result, summary)
}

func (s *Scheduler) doCheckin() (summary, result string) {
	var ok, already, failed, skipped int
	var errs []string
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled || !st.Enabled {
			skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			skipped++
			continue
		}
		acctFailed := false
		counted := false // 该账号是否已计入 ok/already（防 ent-usage 失败时双重计 failed 或重复计成功）
		// 签到（status → 未签到则 claim）；失败不阻断解冻
		checkedIn, _, enable, err := s.cfg.Upstream.CheckinStatus(a)
		if err != nil {
			acctFailed = true
			errs = append(errs, fmt.Sprintf("%s status: %v", shortUID(st.UID), err))
		} else if !checkedIn && enable {
			if err := s.cfg.Upstream.CheckinClaim(a); err != nil {
				acctFailed = true
				errs = append(errs, fmt.Sprintf("%s claim: %v", shortUID(st.UID), err))
			} else {
				ok++
				counted = true
				log.Printf("checkin %s: ok", st.UID)
			}
		} else if checkedIn {
			// 已签到（人工在 TRAE 端签过，或此前任务已领取）→ 不重复领取，单独计数并记录原因
			already++
			counted = true
			log.Printf("checkin %s: already checked in (manual or previous run)", st.UID)
		}
		// 查积分 + 解冻
		remain, err := s.cfg.Upstream.UserEntUsage(a)
		if err != nil {
			if !acctFailed {
				acctFailed = true
				errs = append(errs, fmt.Sprintf("%s ent-usage: %v", shortUID(st.UID), err))
			}
		} else {
			s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		}
		if acctFailed {
			failed++
			continue
		}
		if !counted {
			ok++ // 无签到动作但全流程顺利（如上游关闭签到 enable=false）
		}
	}
	return buildSummary(ok, already, failed, skipped, 0, errs), classify(ok+already, failed, skipped)
}

// RunRefreshNow 立即对所有账号刷新 token（保活）。
// 硬禁用账号同样尝试 refresh：成功 = 凭证有效 → 复活；失败保持禁用。
func (s *Scheduler) RunRefreshNow() {
	s.rescanAuths()
	start := time.Now()
	summary, result := s.doRefresh()
	s.record("refresh", start, result, summary)
}

func (s *Scheduler) doRefresh() (summary, result string) {
	var ok, revived, failed, skipped int
	var errs []string
	for _, st := range s.cfg.Pool.List() {
		if !st.Enabled {
			skipped++ // 用户明确停用，不参与
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			skipped++
			continue
		}
		// 健康账号只刷临期；禁用账号始终尝试（复活探测，凭证换新后自动恢复）
		need := a.NeedsRefresh(s.cfg.RefreshSkew)
		if st.Disabled {
			need = true
		}
		if !need {
			skipped++
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("%s: %v", shortUID(st.UID), err))
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead && !st.Disabled {
				s.cfg.Pool.Disable(st.UID, "session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
		if st.Disabled {
			s.cfg.Pool.Revive(st.UID)
			revived++
			log.Printf("refresh %s: revived (token valid again)", st.UID)
		} else {
			ok++
		}
	}
	return buildSummary(ok, 0, failed, skipped, revived, errs), classify(ok+revived, failed, skipped)
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

// buildSummary 生成执行总结：账号数 + 成功/已签到/复活/失败/跳过 + 错误摘要（截断）。
func buildSummary(ok, already, failed, skipped, revived int, errs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "账号：成功 %d", ok)
	if already > 0 {
		fmt.Fprintf(&b, "、已签到 %d（人工或此前任务已领取，未重复计）", already)
	}
	if revived > 0 {
		fmt.Fprintf(&b, "、复活 %d", revived)
	}
	fmt.Fprintf(&b, "、失败 %d、跳过 %d", failed, skipped)
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
