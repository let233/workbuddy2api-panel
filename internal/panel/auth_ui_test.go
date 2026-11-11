package panel

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestPanelLoginUIShape 钉住「面板没有全局密钥」在前端的两个可见后果：
//  1. 登录层默认就是展开的（.veil.on）：未登录的人先看到账号密码，不会先闪内部界面；
//  2. 密钥输入框、API 密钥配置项、显示/隐藏按钮全部消失——留着任何一个都等于
//     暗示「面板靠密钥进入」，而密钥已经不存在了。
//
// 这类断言只能防回归（渲染细节），所以只看 id/class 这些稳定锚点。
func TestPanelLoginUIShape(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("GET /panel/ = %d", rec.Code)
	}
	for _, want := range []string{
		"class=\"veil on\" id=\"loginVeil\"",
		"id=\"loginUser\"",
		"id=\"loginPass\"",
		"id=\"btnLogin\"",
		"id=\"pwOld\"",
		"id=\"btnPw\"",
		"id=\"btnLogout\"",
		// 提示默认隐藏：只有还在用初始密码的实例才该看见「admin / 123456」。
		"id=\"loginHint\" hidden",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index.html 缺少 %s", want)
		}
	}
	for _, gone := range []string{
		"id=\"keyInput\"",
		"id=\"btnKey\"",
		"id=\"cfgKey\"",
		"id=\"btnEye\"",
		"name=\"api_key\"",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("index.html 仍残留 %s（全局密钥已删除）", gone)
		}
	}
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(js)
	for _, want := range []string{
		"function showLogin(",
		"/panel/api/auth/session",
		"/panel/api/auth/login",
		"auth/password",
		"default_credentials",
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js 缺少 %s", want)
		}
	}
	if strings.Contains(app, "LS_KEY") || strings.Contains(app, "Bearer") {
		t.Error("app.js 不应再持有本地密钥或拼 Authorization")
	}
}
