// chatlog_api.go 面板调用记录接口：/admin/api/chatlogs（分页查询）+ /admin/api/chatlogs/stats。
package server

import (
	"net/http"
	"strconv"
)

// atoiDefault 解析失败返回 def。
func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// adminChatLogs GET /admin/api/chatlogs?model=&status=&uid=&q=&page=&page_size=
// 倒序分页返回记录 + 筛选下拉用的 models/uids 列表。
func (h *Handler) adminChatLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pageSize := atoiDefault(q.Get("page_size"), 50)
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	f := ChatLogFilter{
		Model:    q.Get("model"),
		Status:   q.Get("status"),
		UID:      q.Get("uid"),
		Q:        q.Get("q"),
		Page:     page,
		PageSize: pageSize,
	}
	writeJSON(w, http.StatusOK, h.chatlog.Query(f))
}

// adminChatLogStats GET /admin/api/chatlogs/stats：今日调用/成功率/token + 存储占用。
func (h *Handler) adminChatLogStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.chatlog.Stats())
}
