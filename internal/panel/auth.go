// auth.go 面板账号登录：会话探测 / 登录 / 登出 / 改密 + 登录失败限速。
//
// 面板不再有全局 api_key：每个请求按 Cookie 里的会话 token 现查 panelauth.Manager，
// 网关侧也只认客户端 Key（见 internal/gatewaykeys）。会话 token 只存 SHA-256，
// Cookie 是 HttpOnly + SameSite=Lax；登录失败按 IP 限速，挡在线爆破。
package panel

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/panelauth"
)

// 登录失败限速窗口与阈值：同一 IP 一分钟内失败 5 次就冷却到窗口末尾。
// bcrypt 本身就慢（默认 cost），这里只是把「无限次尝试」压成「一分钟 5 次」。
const (
	loginFailWindow = time.Minute
	loginFailLimit  = 5
)

// loginGuard 按 IP 统计近期登录失败次数。
//
// 只用内存：面板是常驻单进程，重启后计数清零可以接受（爆破者拿不到加速）。
type loginGuard struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

// blocked 判断该 IP 是否还在冷却期内。
func (g *loginGuard) blocked(ip string, now time.Time) bool {
	if ip == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	recent := g.pruneLocked(ip, now)
	return len(recent) >= loginFailLimit
}

// fail 记一次失败。
func (g *loginGuard) fail(ip string, now time.Time) {
	if ip == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits[ip] = append(g.pruneLocked(ip, now), now)
}

// reset 登录成功或改密成功后清空该 IP 的失败计数。
func (g *loginGuard) reset(ip string) {
	if ip == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.hits, ip)
}

// pruneLocked 丢掉窗口外的记录并顺手回收空条目（调用方持锁）。
func (g *loginGuard) pruneLocked(ip string, now time.Time) []time.Time {
	if g.hits == nil {
		g.hits = make(map[string][]time.Time)
	}
	cutoff := now.Add(-loginFailWindow)
	in := g.hits[ip]
	out := in[:0]
	for _, at := range in {
		if at.After(cutoff) {
			out = append(out, at)
		}
	}
	if len(out) == 0 {
		delete(g.hits, ip)
		return nil
	}
	g.hits[ip] = out
	return out
}

// requestIP 取调用方 IP（仅本机/反代场景：优先 X-Forwarded-For 首段）。
func requestIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr)); err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

// secureRequest 判断是否 HTTPS（决定会话 Cookie 是否带 Secure）。
// 反代终止 TLS 时看 X-Forwarded-Proto；两者都没有就按明文 HTTP 处理。
func secureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// sessionUser 取当前请求的登录用户；未登录返回 ok=false。
//
// cfg.Users 为 nil 表示「不鉴权」（纯内存模式/单测），此时调用方不该走这里。
func (p *Panel) sessionUser(r *http.Request) (panelauth.User, bool) {
	if p.cfg.Users == nil {
		return panelauth.User{}, false
	}
	token := panelauth.TokenFromRequest(r)
	if token == "" {
		return panelauth.User{}, false
	}
	return p.cfg.Users.SessionUser(token)
}

// authSession 探测会话状态（公开接口：前端启动时据此决定显示登录层还是主界面）。
func (p *Panel) authSession(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Users == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated":        true,
			"username":             "",
			"is_admin":             true,
			"must_change_password": false,
			"default_credentials":  false,
			"login_required":       false,
		})
		return
	}
	u, ok := p.sessionUser(r)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated":       false,
			"default_credentials": p.cfg.Users.DefaultCredentialsPending(),
			"login_required":      true,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":        true,
		"username":             u.Username,
		"is_admin":             u.IsAdmin,
		"must_change_password": u.MustChangePassword,
		"default_credentials":  p.cfg.Users.DefaultCredentialsPending(),
		"login_required":       true,
	})
}

// authLogin 账号密码登录，成功后下发会话 Cookie。
func (p *Panel) authLogin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Users == nil {
		writeErr(w, http.StatusNotImplemented, "auth_not_configured")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body")
		return
	}
	ip := requestIP(r)
	now := time.Now()
	if p.loginGuard.blocked(ip, now) {
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
		return
	}
	u, ok := p.cfg.Users.Authenticate(strings.TrimSpace(body.Username), body.Password)
	if !ok {
		p.loginGuard.fail(ip, now)
		writeErr(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	token, err := p.cfg.Users.IssueSession(u.ID, ip, r.UserAgent())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "session_failed")
		return
	}
	p.loginGuard.reset(ip)
	panelauth.SetSessionCookie(w, token, secureRequest(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":        true,
		"username":             u.Username,
		"is_admin":             u.IsAdmin,
		"must_change_password": u.MustChangePassword,
		"default_credentials":  p.cfg.Users.DefaultCredentialsPending(),
	})
}

// authLogout 退出登录：删服务端会话 + 清 Cookie。
//
// 公开接口：会话早已失效时也要能「退出」，否则前端会卡在 401 循环里。
func (p *Panel) authLogout(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Users != nil {
		if token := panelauth.TokenFromRequest(r); token != "" {
			p.cfg.Users.DropSession(token)
		}
	}
	panelauth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// authPassword 修改自己的密码；成功后换发会话（旧 token 全部作废）。
func (p *Panel) authPassword(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Users == nil {
		writeErr(w, http.StatusNotImplemented, "auth_not_configured")
		return
	}
	u, ok := p.sessionUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "login_required")
		return
	}
	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body")
		return
	}
	if err := p.cfg.Users.ChangePassword(u.ID, body.OldPassword, body.NewPassword); err != nil {
		if errors.Is(err, panelauth.ErrBadPassword) {
			writeErr(w, http.StatusBadRequest, "invalid_old_password")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid_new_password")
		return
	}
	// 改密即踢掉所有旧会话，再把当前浏览器换成新 token：
	// 密码外泄时「改密」必须真的把攻击者的会话掐掉。
	p.cfg.Users.DropUserSessions(u.ID)
	ip := requestIP(r)
	token, err := p.cfg.Users.IssueSession(u.ID, ip, r.UserAgent())
	if err != nil {
		panelauth.ClearSessionCookie(w)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "relogin": true})
		return
	}
	p.loginGuard.reset(ip)
	panelauth.SetSessionCookie(w, token, secureRequest(r))
	// 把「旧会话已作废 + 新会话已签发」一起落盘：否则进程在 3 秒窗口内重启，
	// 被踢掉的旧 token 会从库里复活，改密等于没踢。
	p.cfg.Users.Flush()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
