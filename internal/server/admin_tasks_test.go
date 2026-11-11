package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

type fakeTasks struct{ block chan struct{} }

func (f *fakeTasks) RunCheckinNow()   { <-f.block }
func (f *fakeTasks) RunActivityNow()  {}
func (f *fakeTasks) RunKeepaliveNow() {}
func (f *fakeTasks) RunTravelNow()    {}
func (f *fakeTasks) RunGrowthNow()    {}

func TestAdminTaskRunOnce(t *testing.T) {
	audit, err := NewAuditLog(storetest.Open(t))
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	h := NewHandler(Config{
		AdminEnabled: true,
		Tasks:        &fakeTasks{block: block},
		Audit:        audit,
	})
	req := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
		r.Header.Set("Authorization", "Bearer k")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if req().Code != http.StatusAccepted {
		t.Fatal("first run should be 202")
	}
	if req().Code != http.StatusConflict {
		t.Fatal("second run should be 409 while the first is blocked")
	}
	close(block)
	unknown := httptest.NewRequest(http.MethodPost, "/admin/tasks/school/run", nil)
	unknown.Header.Set("Authorization", "Bearer k")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, unknown)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown task %d", w.Code)
	}
	rows, err := audit.Recent(50)
	if err != nil {
		t.Fatalf("读审计: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.Action == "task.run" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit missing task.run: %+v", rows)
	}
}

func TestMetricsOffIs404(t *testing.T) {
	h := NewHandler(Config{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("metrics off got %d", w.Code)
	}
}
