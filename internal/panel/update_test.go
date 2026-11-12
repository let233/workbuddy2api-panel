package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/updater"
)

// 更新接口必须要求登录：无会话 401，带会话才走到「未装配 updater」的 501。
func TestUpdateAPIRequiresAuthentication(t *testing.T) {
	for _, endpoint := range []struct{ method, path string }{
		{http.MethodGet, "/panel/api/update"},
		{http.MethodPost, "/panel/api/update/check"},
		{http.MethodPost, "/panel/api/update/install"},
	} {
		p := newTestPanel()
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(endpoint.method, endpoint.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d", endpoint.path, rec.Code)
		}
		req := httptest.NewRequest(endpoint.method, endpoint.path, nil)
		req.AddCookie(authSession(t, p))
		rec = httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("unconfigured %s: got %d", endpoint.path, rec.Code)
		}
	}
}

func TestUpdateStatusAndUnsupportedInstall(t *testing.T) {
	p := New(Config{Updater: updater.New("dev", "", "", updater.Settings{AutoEnabled: true, AutoTime: "06:30"})})
	req := httptest.NewRequest(http.MethodGet, "/panel/api/update", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	var status updater.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || status.CurrentVersion != "dev" || status.CurrentKnown || status.CanInstall || !status.AutoEnabled || status.AutoTime != "06:30" {
		t.Fatalf("unexpected status: %d %+v", rec.Code, status)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["current_known"] != false {
		t.Fatalf("API must expose current_known=false: %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/panel/api/update/install", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || p.cfg.Updater.Status().Installing {
		t.Fatalf("dev install must fail without pending state: %d %s", rec.Code, rec.Body.String())
	}
}
