package panel

// 静态资源服务（internal/panel/index.go）：前端产物不再 go:embed，运行时把
// Config.PublicDir 整目录映射到 /panel/。这里钉住行为契约——缓存策略、产物缺失时
// 的说明页、以及「静态资源不需要会话但 /panel/api/* 仍然要」。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/panelauth"
)

// newStaticPanel 造一个只关心静态资源的面板：PublicDir 指向给定目录。
// users 为 nil 时不鉴权（纯静态行为）；非 nil 时打开登录闸门。
func newStaticPanel(dir string, users *panelauth.Manager) *Panel {
	return New(Config{Version: "test", PublicDir: dir, Users: users})
}

// writePublic 在临时目录里造一份最小前端产物：index.html + 一个带 hash 的资源。
func writePublic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"index.html":             "<!doctype html><html><body>panel-boot</body></html>",
		"assets/index-abc123.js": "console.log('panel')",
		"favicon.ico":            "ico",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// GET /panel/ 返回 public/index.html，且必须 no-store（新页面配旧资源的经典事故）。
func TestStaticServesIndexAtPanelRoot(t *testing.T) {
	dir := writePublic(t)
	p := newStaticPanel(dir, nil)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/panel/: code=%d want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "panel-boot") {
		t.Errorf("/panel/: 返回的不是 public/index.html 内容: %q", body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/panel/: Cache-Control=%q want no-store", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("/panel/: Content-Type=%q want text/html", ct)
	}

	// 显式 /index.html 由 http.FileServer 自己做规范化跳转（和 net/http 一致），
	// 落到 /panel/——这里只钉「不是 200 双份内容、且跳转目标是面板根」。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/index.html", nil))
	if rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusFound {
		t.Fatalf("/panel/index.html: code=%d want 301/302 跳转", rec.Code)
	}
	// net/http 用的是相对跳转 "./"（等价于 /panel/）。
	if loc := rec.Header().Get("Location"); loc != "./" && !strings.HasSuffix(loc, "/panel/") {
		t.Errorf("/panel/index.html: Location=%q want ./ 或 /panel/", loc)
	}
}

// /assets/* 的文件名含内容 hash，长缓存 immutable；其他静态文件用 no-cache 走 304 校验。
func TestStaticCachePolicyPerPath(t *testing.T) {
	dir := writePublic(t)
	p := newStaticPanel(dir, nil)

	cases := []struct{ path, wantCC string }{
		{"/panel/assets/index-abc123.js", "public, max-age=31536000, immutable"},
		{"/panel/favicon.ico", "no-cache"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code=%d want 200", c.path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != c.wantCC {
			t.Errorf("%s: Cache-Control=%q want %q", c.path, cc, c.wantCC)
		}
	}
}

// 产物缺失（没构建前端）时，/panel/ 回 200 + 纯文本说明页，把真实原因和修法写在页面上。
func TestStaticMissingIndexGivesHint(t *testing.T) {
	dir := t.TempDir() // 空目录：没有 index.html
	p := newStaticPanel(dir, nil)

	for _, path := range []string{"/panel/", "/panel/index.html"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code=%d want 200（说明页而非 404）", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
			t.Errorf("%s: Content-Type=%q want text/plain", path, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control=%q want no-store", path, cc)
		}
		body := rec.Body.String()
		for _, want := range []string{"前端产物未找到", "npm --prefix web run build", "panel.public_dir"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: 说明页缺少 %q:\n%s", path, want, body)
			}
		}
	}
}

// SPA 用 hash 路由，不需要 history fallback：未知路径就是真的 404，不能拿 index 兜底
// （否则打错的 API/资源地址会静默变成 200 HTML，调用方更难排查）。
func TestStaticUnknownPathIs404(t *testing.T) {
	dir := writePublic(t)
	p := newStaticPanel(dir, nil)

	for _, path := range []string{"/panel/assets/missing.js", "/panel/nope/deep"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d want 404", path, rec.Code)
		}
	}
}

// 匿名可拿静态资源（登录页要先能加载脚本），但 /panel/api/* 非白名单路径仍 401。
func TestStaticAnonymousOKButAPINeedsSession(t *testing.T) {
	dir := writePublic(t)
	p := newStaticPanel(dir, authUsers())

	// 静态资源：无 Cookie 也要 200。
	for _, path := range []string{"/panel/", "/panel/assets/index-abc123.js"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s 匿名: code=%d want 200", path, rec.Code)
		}
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s 匿名: 缺少 CSP 安全头", path)
		}
	}

	// API：非白名单一律 401（失效方向是「看得见的 401」，不是静默公开）。
	for _, path := range []string{"/panel/api/overview", "/panel/api/nonexistent"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 匿名: code=%d want 401", path, rec.Code)
		}
	}

	// 白名单里的会话查询免会话（自己返回未登录 JSON，而不是被 401 短路）。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/auth/session", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Error("/panel/api/auth/session 应在白名单内，不该被 401 短路")
	}
}
