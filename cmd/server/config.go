// config.go 加载 JSON 配置 + TW2A_* 环境变量覆盖。
// APIKey 只从环境变量 TW2A_API_KEY 读取（SPEC §0 脱敏纪律：key 走 env，不落盘 git）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"trae2api-web/internal/server"
)

// Config 顶层配置。
type Config struct {
	Listen        string `json:"listen"`         // ":7864"
	CallbackPort  string `json:"callback_port"`  // "18080"（TRAE 登录回调监听端口，0 = 不起）
	APIKey        string `json:"-"`              // 只读 env TW2A_API_KEY（不读 json）
	AuthDir       string `json:"auth_dir"`       // "./auths"
	StateFile     string `json:"state_file"`     // "./data/state.json"
	DefaultModel  string `json:"default_model"`  // "glm-5.2"
	ChatLogFile   string `json:"chat_log_file"`  // 调用记录 JSONL，默认 ./data/chatlog.jsonl（空 = 不记录）

	Cooldown struct {
		PlanCredit  string `json:"plan_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHour   int    `json:"checkin_hour"`   // 9
		RefreshHours  []int  `json:"refresh_hours"`  // [3]
		RetentionDays int    `json:"retention_days"` // 执行记录保留天数，默认 30，0 = 永久
		LogFile       string `json:"log_file"`       // 执行记录文件（JSONL），默认 ./data/schedule-log.jsonl
		CheckinEnabled *bool `json:"checkin_enabled"` // 签到开关，缺省 true
		JitterMinutes  *int  `json:"jitter_minutes"`  // 触发随机抖动分钟数，缺省 10；0 = 准点，上限 30
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 120
	} `json:"upstream"`

	// 解析后的 duration / schedule 运行值。
	PlanCreditDur         time.Duration `json:"-"`
	SoftRateDur           time.Duration `json:"-"`
	ErrCooldownDur        time.Duration `json:"-"`
	ScheduleCheckinEnable bool          `json:"-"` // CheckinEnabled 解析值（缺省 true）
	ScheduleJitterMinutes int           `json:"-"` // JitterMinutes 解析值（缺省 10，封顶 30）
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Listen:       ":7864",
		CallbackPort: "18080",
		APIKey:       "",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "glm-5.2",
		ChatLogFile:  "./data/chatlog.jsonl",
	}
	c.Cooldown.PlanCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinHour = 9
	c.Schedule.RefreshHours = []int{3}
	c.Schedule.RetentionDays = 30
	c.Schedule.LogFile = "./data/schedule-log.jsonl"
	c.Upstream.TimeoutSeconds = 120
	return c
}

// Load 从 path 读配置，再用 TW2A_* env 覆盖。path 为空或不存在时用默认 + env。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// 配置文件可选：不存在 → 纯默认 + env
				c = Default()
			} else {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("TW2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("TW2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("TW2A_CALLBACK_PORT"); v != "" {
		c.CallbackPort = v
	}
	if v := os.Getenv("TW2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("TW2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("TW2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("TW2A_CHAT_LOG_FILE"); v != "" {
		c.ChatLogFile = v
	}
	if v := os.Getenv("TW2A_PLAN_CREDIT"); v != "" {
		c.Cooldown.PlanCredit = v
	}
	if v := os.Getenv("TW2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("TW2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("TW2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("TW2A_CHECKIN_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.CheckinHour = n
		}
	}
	if v := os.Getenv("TW2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.PlanCreditDur, err = time.ParseDuration(c.Cooldown.PlanCredit); err != nil {
		return fmt.Errorf("cooldown.plan_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	if c.Listen == "" {
		c.Listen = ":7864"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// CallbackPort：空/未设 → 默认 18080；显式 "0" → 不起回调 server（纯手动粘贴模式）
	if c.CallbackPort == "" {
		c.CallbackPort = "18080"
	}
	// Schedule 解析值：缺省签到开、抖动 10 分钟；显式 0 = 关闭抖动
	c.ScheduleCheckinEnable = true
	if c.Schedule.CheckinEnabled != nil {
		c.ScheduleCheckinEnable = *c.Schedule.CheckinEnabled
	}
	c.ScheduleJitterMinutes = 10
	if c.Schedule.JitterMinutes != nil {
		c.ScheduleJitterMinutes = *c.Schedule.JitterMinutes
	}
	if c.ScheduleJitterMinutes < 0 {
		c.ScheduleJitterMinutes = 0
	}
	if c.ScheduleJitterMinutes > 30 {
		c.ScheduleJitterMinutes = 30
	}
	return nil
}

// patchConfigFile 只改写 config.json 中页面可编辑字段（schedule/upstream 段），
// 其余字段与注释外格式尽量保留（JSON 重排不可避免，注释会丢失——Go 标准库限制）。
// 原子写：tmp + rename。文件不存在时报错（页面保存依赖已存在的配置文件）。
func patchConfigFile(path string, s server.EditableSettings) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("config file %s not found (copy config.example.json first)", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	sched, _ := doc["schedule"].(map[string]any)
	if sched == nil {
		sched = map[string]any{}
		doc["schedule"] = sched
	}
	sched["checkin_hour"] = s.CheckinHour
	rh := make([]int, len(s.RefreshHours))
	copy(rh, s.RefreshHours)
	sched["refresh_hours"] = rh
	sched["retention_days"] = s.RetentionDays
	sched["checkin_enabled"] = s.CheckinEnabled
	sched["jitter_minutes"] = s.JitterMinutes
	up, _ := doc["upstream"].(map[string]any)
	if up == nil {
		up = map[string]any{}
		doc["upstream"] = up
	}
	up["timeout_seconds"] = s.TimeoutSeconds
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
