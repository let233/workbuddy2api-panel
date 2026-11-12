package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/panelauth"
)

// newTestPanel 造一个开了登录闸门的面板（内存账号库 + 默认管理员）：没有会话
// Cookie 的 /panel/api/* 一律 401，不会走进依赖 Pool/Upstream 的 handler。
func newTestPanel() *Panel {
	return New(Config{Version: "test", Users: authUsers()})
}

// authUsers 内存账号库 + 播种默认管理员：Config.Users 非 nil 即打开登录闸门。
func authUsers() *panelauth.Manager {
	m := panelauth.NewManager(nil)
	if _, _, err := m.EnsureDefaultAdmin(); err != nil {
		panic(err) // 内存库播种不可能失败；失败说明 panelauth 被改坏了
	}
	return m
}

// authSession 直接向账号库签发一个会话，返回可挂到请求上的 Cookie。
// 走 Manager 而不是 HTTP 登录，是为了让「已经登录」这件事与登录限速/JSON 解析解耦。
func authSession(t *testing.T, pn *Panel) *http.Cookie {
	t.Helper()
	u, ok := pn.cfg.Users.Authenticate(panelauth.DefaultAdminUsername, panelauth.DefaultAdminPassword)
	if !ok {
		t.Fatal("默认管理员无法登录 —— 账号库播种坏了")
	}
	token, err := pn.cfg.Users.IssueSession(u.ID, "127.0.0.1", "test")
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	rec := httptest.NewRecorder()
	panelauth.SetSessionCookie(rec, token, false)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("SetSessionCookie 没有写出 Cookie")
	}
	return cookies[0]
}

// 默认拒绝：页面静态资源与登录/登出端点之外一律要会话，包括尚未注册的路径。
// 将来新增路由即便忘了包鉴权，也不会静默公开。
func TestPanelDefaultDeny(t *testing.T) {
	p := newTestPanel()
	for _, path := range []string{"/panel/api/overview", "/panel/api/config", "/panel/api/nonexistent"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: code=%d, want 401", path, rec.Code)
		}
	}
	// 页面与静态资源本身不含秘密，登录页也要先能加载脚本：匿名请求不得 401。
	// 产物目录缺失时 /panel/ 回的是「先构建前端」的说明页（仍是 200 + 纯文本）；
	// 其余静态路径按 FileServer 语义回 404，但同样不能是 401。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/panel/: code=%d, want 200", rec.Code)
	}
	for _, path := range []string{"/panel/assets/index-abc123.js", "/panel/favicon.ico"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s: 静态资源不应要求会话，got 401", path)
		}
	}
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/assets/index-abc123.js"}, // 静态资源（404 也要带头）
		{"GET", "/panel/favicon.ico"},
		{"GET", "/panel/api/overview"}, // 401（未登录）
		{"POST", "/panel/api/config"},  // 401
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 无会话的 API 请求必须 401；持有有效会话则通过鉴权层（不再是 401）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: code=%d want 401", rec.Code)
	}
	// 用不存在的路由验证"已登录即过鉴权"（避免触碰依赖 nil 的 handler）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/nonexistent", nil)
	req2.AddCookie(authSession(t, p))
	p.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("valid session must pass the auth layer")
	}
}
