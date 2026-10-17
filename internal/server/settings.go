// settings.go 管理面板的运行配置：页面可编辑项（config.json 的 schedule/upstream 段）
// + 定时任务执行记录查询。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"trae2api-web/internal/scheduler"
)

// EditableSettings 页面可编辑配置（config.json schedule/upstream 段的运行时视图）。
type EditableSettings struct {
	CheckinHour    int   `json:"checkin_hour"`    // 签到小时 0-23
	RefreshHours   []int `json:"refresh_hours"`   // token 预刷新小时列表
	TimeoutSeconds int   `json:"timeout_seconds"` // 上游超时（秒），含首字节兜底
	RetentionDays  int   `json:"retention_days"`  // 执行记录保留天数，0 = 永久
}

// validateSettings 校验可编辑配置的合法性与取值范围。
func validateSettings(v EditableSettings) error {
	if v.CheckinHour < 0 || v.CheckinHour > 23 {
		return fmt.Errorf("checkin_hour 取值 0-23，当前 %d", v.CheckinHour)
	}
	if len(v.RefreshHours) == 0 {
		return fmt.Errorf("refresh_hours 不能为空")
	}
	if len(v.RefreshHours) > 24 {
		return fmt.Errorf("refresh_hours 最多 24 个")
	}
	seen := map[int]bool{}
	for _, h := range v.RefreshHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("refresh_hours 取值 0-23，含 %d", h)
		}
		if seen[h] {
			return fmt.Errorf("refresh_hours 含重复小时 %d", h)
		}
		seen[h] = true
	}
	if v.TimeoutSeconds < 5 || v.TimeoutSeconds > 3600 {
		return fmt.Errorf("timeout_seconds 取值 5-3600，当前 %d", v.TimeoutSeconds)
	}
	if v.RetentionDays < 0 || v.RetentionDays > 3650 {
		return fmt.Errorf("retention_days 取值 0-3650（0 = 永久），当前 %d", v.RetentionDays)
	}
	return nil
}

// SettingsStore 可编辑配置的内存权威值；Update 时先 apply（运行时生效）再 persist（落盘 config.json）。
// apply/persist 任一失败则本次更新不生效（内存值不变）。
type SettingsStore struct {
	mu      sync.RWMutex
	current EditableSettings
	apply   func(EditableSettings) error // 运行时生效：scheduler 热更新 + upstream 超时
	persist func(EditableSettings) error // 持久化：补丁写回 config.json
}

// NewSettingsStore 构建；apply/persist 允许为 nil（仅内存态，测试用）。
func NewSettingsStore(initial EditableSettings, apply, persist func(EditableSettings) error) *SettingsStore {
	return &SettingsStore{current: initial, apply: apply, persist: persist}
}

// Get 当前生效值。
func (s *SettingsStore) Get() EditableSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.current
	out.RefreshHours = append([]int{}, s.current.RefreshHours...)
	return out
}

// Update 校验 → 运行时生效 → 持久化 → 提交内存值。
func (s *SettingsStore) Update(next EditableSettings) error {
	if err := validateSettings(next); err != nil {
		return err
	}
	if s.apply != nil {
		if err := s.apply(next); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
	}
	if s.persist != nil {
		if err := s.persist(next); err != nil {
			return fmt.Errorf("save: %w", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next.RefreshHours = append([]int{}, next.RefreshHours...)
	s.current = next
	return nil
}

// adminGetSettings GET /admin/api/settings：当前可编辑配置。
func (h *Handler) adminGetSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "settings_unavailable", "settings store not configured")
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Settings.Get())
}

// adminUpdateSettings PATCH /admin/api/settings：整体替换可编辑配置。
func (h *Handler) adminUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Settings == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "settings_unavailable", "settings store not configured")
		return
	}
	var next EditableSettings
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&next); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "decode body: "+err.Error())
		return
	}
	if err := h.cfg.Settings.Update(next); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_settings", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Settings.Get())
}

// adminScheduleLogs GET /admin/api/schedule/logs：定时任务执行记录（最新在前）。
func (h *Handler) adminScheduleLogs(w http.ResponseWriter, r *http.Request) {
	var recs []scheduler.Record
	if h.cfg.Scheduler != nil {
		recs = h.cfg.Scheduler.Records()
	}
	if recs == nil {
		recs = []scheduler.Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": recs})
}
