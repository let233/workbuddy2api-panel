package panel

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/panelauth"
)

// TestSessionReportsDefaultCredentialsUntilPasswordChanged 钉住登录框提示的开关来源：
// 「首次部署的默认账号 admin / 123456」只在还是初始口令时出现。改过密码后会话接口
// 必须回 false，否则每个访问者都会照着这句话去试一次必然失败的登录。
func TestSessionReportsDefaultCredentialsUntilPasswordChanged(t *testing.T) {
	m := panelauth.NewManager(nil)
	u, created, err := m.EnsureDefaultAdmin()
	if err != nil || !created {
		t.Fatalf("EnsureDefaultAdmin created=%v err=%v", created, err)
	}
	p := New(Config{Version: "test", Users: m})

	session := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/auth/session", nil))
		if rec.Code != 200 {
			t.Fatalf("GET auth/session = %d", rec.Code)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("session body 不是 JSON: %v (%s)", err, rec.Body.String())
		}
		return got
	}

	// 未登录也要带这个字段：登录框得在登录之前就决定要不要显示提示。
	if got := session(); got["default_credentials"] != true {
		t.Fatalf("初始口令未改，default_credentials 应为 true，实得 %v", got["default_credentials"])
	}
	if err := m.ChangePassword(u.ID, panelauth.DefaultAdminPassword, "newpass789"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if got := session(); got["default_credentials"] != false {
		t.Fatalf("改密后 default_credentials 应为 false，实得 %v", got["default_credentials"])
	}
}
