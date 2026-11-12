// index.go 面板静态资源与安全响应头。
//
// 前端是独立的 React 应用（web/，Vite 构建），产物输出到仓库根的 public/。运行时
// 这里把 public/ 整个目录映射到 /panel/ 下——**不再 go:embed**。取舍：
//   - 换前端只需替换 public/，二进制不动（容器里重新挂载即可，不必重建镜像）；
//   - 构建产物带内容 hash、体积大，打进二进制只会让二进制虚胖、每次改动都触发
//     全量重新编译；
//   - 代价是部署必须带上前端产物：目录里没有 index.html 时这里回一个可读的
//     说明页（提示先构建），而不是空白 404。
//
// 安全头对「面板页面、静态资源与全部 /panel/api/* 响应」统一生效（见 ServeHTTP）：
// CSP 限制脚本只能来自本服务，禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，
// 并声明不泄露 Referer 出去。
package panel

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（Vite 产物 /assets/*.js）
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：React 组件会给进度条宽度、图表尺寸写 style="..." 属性，
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图（QR 码是 data: SVG）
//   - font-src 'self' data:     万一将来引入 self-host 字体，不必再改 CSP
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; font-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// staticDir 前端产物目录的绝对路径（Config.PublicDir，缺省 ./public）。
func (p *Panel) staticDir() string {
	dir := p.cfg.PublicDir
	if dir == "" {
		dir = "public"
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// missingIndexHint 产物缺失时的说明页。刻意是 200 + 纯文本：面板打不开时运维第一
// 反应是「服务坏了」，这里直接把真实原因和修法写在页面上。
func missingIndexHint(dir string) string {
	return "前端产物未找到：" + filepath.Join(dir, "index.html") + "\n\n" +
		"面板前端已改为独立的 React 应用（web/），需要先构建再启动：\n" +
		"  npm --prefix web install\n" +
		"  npm --prefix web run build   # 产物输出到 public/\n\n" +
		"Docker 镜像已包含构建阶段，无需手动构建；\n" +
		"若自定义了目录，请检查 config.json 的 panel.public_dir。\n"
}

// staticHandler 把 public/ 映射到 /panel/。
//
// 缓存策略按 Vite 的产物约定：
//   - index.html（引用带 hash 的资源）必须 no-store，否则新页面配旧资源；
//   - /assets/* 文件名含内容 hash，可以 immutable 长缓存；
//   - 其余（favicon 等）用 no-cache 走 304 校验。
//
// SPA 用 hash 路由（#/accounts），没有路径路由，所以不需要 history fallback：
// 未知路径就是真的不存在，交给 FileServer 回 404。
func (p *Panel) staticHandler() http.Handler {
	dir := p.staticDir()
	files := http.StripPrefix("/panel", http.FileServer(http.Dir(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/panel")
		switch {
		case rel == "" || rel == "/" || rel == "/index.html":
			w.Header().Set("Cache-Control", "no-store")
		case strings.HasPrefix(rel, "/assets/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		// 入口页不存在 = 没构建过前端：给说明页，别让 FileServer 回 404 纯文本。
		if rel == "" || rel == "/" || rel == "/index.html" {
			if st, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || st.IsDir() {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, missingIndexHint(dir))
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
