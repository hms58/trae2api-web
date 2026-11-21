package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trae2api-web/internal/scheduler"
)

func TestSettingsStoreUpdateValidation(t *testing.T) {
	s := NewSettingsStore(EditableSettings{
		CheckinHour: 9, RefreshHours: []int{3}, TimeoutSeconds: 120, RetentionDays: 30,
	}, nil, nil)

	// 非法值：内存值不变
	if err := s.Update(EditableSettings{CheckinHour: 25, RefreshHours: []int{3}, TimeoutSeconds: 120, RetentionDays: 30}); err == nil {
		t.Fatal("checkin_hour=25 should fail")
	}
	if err := s.Update(EditableSettings{CheckinHour: 9, RefreshHours: []int{24}, TimeoutSeconds: 120, RetentionDays: 30}); err == nil {
		t.Fatal("refresh_hours 24 should fail")
	}
	if err := s.Update(EditableSettings{CheckinHour: 9, RefreshHours: []int{3}, TimeoutSeconds: 1, RetentionDays: 30}); err == nil {
		t.Fatal("timeout_seconds=1 should fail")
	}
	if err := s.Update(EditableSettings{CheckinHour: 9, RefreshHours: []int{3, 3}, TimeoutSeconds: 120, RetentionDays: 30}); err == nil {
		t.Fatal("duplicate hours should fail")
	}
	if err := s.Update(EditableSettings{CheckinHour: 9, RefreshHours: []int{3}, TimeoutSeconds: 120, RetentionDays: 30, JitterMinutes: 31}); err == nil {
		t.Fatal("jitter_minutes=31 should fail")
	}
	if got := s.Get(); got.CheckinHour != 9 || got.TimeoutSeconds != 120 {
		t.Errorf("current mutated after failed updates: %+v", got)
	}

	// 合法值：生效
	applied := false
	s2 := NewSettingsStore(EditableSettings{CheckinHour: 9, RefreshHours: []int{3}, TimeoutSeconds: 120, RetentionDays: 30},
		func(v EditableSettings) error { applied = true; return nil }, nil)
	if err := s2.Update(EditableSettings{CheckinHour: 21, RefreshHours: []int{5, 15}, TimeoutSeconds: 300, RetentionDays: 7}); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Error("apply callback not called")
	}
	if got := s2.Get(); got.CheckinHour != 21 || got.TimeoutSeconds != 300 || got.RetentionDays != 7 {
		t.Errorf("get=%+v want updated", got)
	}
}

func TestSettingsStorePersistFailureKeepsMemory(t *testing.T) {
	s := NewSettingsStore(EditableSettings{CheckinHour: 9, RefreshHours: []int{3}, TimeoutSeconds: 120, RetentionDays: 30},
		nil, func(v EditableSettings) error { return errFakePersist })
	if err := s.Update(EditableSettings{CheckinHour: 10, RefreshHours: []int{3}, TimeoutSeconds: 120, RetentionDays: 30}); err == nil {
		t.Fatal("persist failure should surface")
	}
	if got := s.Get(); got.CheckinHour != 9 {
		t.Errorf("memory should be unchanged on persist failure, got %+v", got)
	}
}

var errFakePersist = &fakePersistError{}

type fakePersistError struct{}

func (*fakePersistError) Error() string { return "fake persist failure" }

func TestSettingsEndpoints(t *testing.T) {
	s := NewSettingsStore(EditableSettings{CheckinHour: 9, RefreshHours: []int{3}, TimeoutSeconds: 120, RetentionDays: 30}, nil, nil)
	h := NewHandler(Config{Settings: s})

	// GET
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/settings", nil))
	if rec.Code != 200 {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body)
	}
	var got EditableSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CheckinHour != 9 {
		t.Errorf("checkin_hour=%d", got.CheckinHour)
	}

	// PATCH 合法
	rec = httptest.NewRecorder()
	body := `{"checkin_hour":21,"refresh_hours":[5,15],"timeout_seconds":300,"retention_days":7}`
	h.ServeHTTP(rec, httptest.NewRequest("PATCH", "/admin/api/settings", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("PATCH code=%d body=%s", rec.Code, rec.Body)
	}
	if s.Get().CheckinHour != 21 {
		t.Errorf("after PATCH checkin_hour=%d want 21", s.Get().CheckinHour)
	}

	// PATCH 非法 → 400
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("PATCH", "/admin/api/settings",
		strings.NewReader(`{"checkin_hour":99,"refresh_hours":[3],"timeout_seconds":120,"retention_days":30}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid PATCH code=%d want 400 body=%s", rec.Code, rec.Body)
	}
}

func TestSettingsEndpointsNilStore(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/settings", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("code=%d want 501", rec.Code)
	}
}

func TestScheduleLogsEndpoint(t *testing.T) {
	h := NewHandler(Config{Scheduler: scheduler.New(scheduler.Config{})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/api/schedule/logs", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var d struct {
		Records []scheduler.Record `json:"records"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Records == nil || len(d.Records) != 0 {
		t.Errorf("records=%+v want empty non-nil", d.Records)
	}
}
