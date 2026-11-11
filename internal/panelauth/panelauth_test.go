package panelauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(storetest.Open(t))
	t.Cleanup(m.Close)
	return m
}

func TestEnsureDefaultAdmin(t *testing.T) {
	m := newTestManager(t)
	u, created, err := m.EnsureDefaultAdmin()
	if err != nil || !created {
		t.Fatalf("首次应创建管理员: created=%v err=%v", created, err)
	}
	if u.Username != DefaultAdminUsername || !u.IsAdmin || !u.MustChangePassword {
		t.Fatalf("初始账号字段不对: %+v", u)
	}
	if u.PasswordHash == "" || u.PasswordHash == DefaultAdminPassword {
		t.Fatalf("密码必须哈希存储, got %q", u.PasswordHash)
	}
	if _, again, err := m.EnsureDefaultAdmin(); err != nil || again {
		t.Fatalf("重复调用不应再创建: created=%v err=%v", again, err)
	}
	if _, ok := m.Authenticate(DefaultAdminUsername, DefaultAdminPassword); !ok {
		t.Fatal("admin/123456 应能登录")
	}
	if _, ok := m.Authenticate(DefaultAdminUsername, "wrong"); ok {
		t.Fatal("错误密码不应通过")
	}
	if _, ok := m.Authenticate("nobody", "123456"); ok {
		t.Fatal("不存在的账号不应通过")
	}
}

func TestSessionLifecycle(t *testing.T) {
	m := newTestManager(t)
	u, _, _ := m.EnsureDefaultAdmin()
	tok, err := m.IssueSession(u.ID, "10.0.0.1", "ua/1")
	if err != nil || tok == "" {
		t.Fatalf("签发失败: %v", err)
	}
	got, ok := m.SessionUser(tok)
	if !ok || got.ID != u.ID {
		t.Fatalf("会话应命中: ok=%v got=%+v", ok, got)
	}
	if got.PasswordHash != "" {
		t.Fatal("会话返回的账号不应带密码哈希")
	}
	if _, ok := m.SessionUser("garbage"); ok {
		t.Fatal("伪造 token 不应命中")
	}
	m.DropSession(tok)
	if _, ok := m.SessionUser(tok); ok {
		t.Fatal("注销后会话应失效")
	}
	// 过期会话
	tok2, _ := m.IssueSession(u.ID, "", "")
	m.mu.Lock()
	s := m.sessions[hashToken(tok2)]
	s.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	m.sessions[hashToken(tok2)] = s
	m.mu.Unlock()
	if _, ok := m.SessionUser(tok2); ok {
		t.Fatal("过期会话不应命中")
	}
}

func TestChangePassword(t *testing.T) {
	m := newTestManager(t)
	u, _, _ := m.EnsureDefaultAdmin()
	if err := m.ChangePassword(u.ID, "bad", "newpass123"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("旧密码错应报错, got %v", err)
	}
	if err := m.ChangePassword(u.ID, DefaultAdminPassword, "123"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("过短密码应拒绝, got %v", err)
	}
	if err := m.ChangePassword(u.ID, DefaultAdminPassword, "newpass123"); err != nil {
		t.Fatalf("改密失败: %v", err)
	}
	if _, ok := m.Authenticate(DefaultAdminUsername, DefaultAdminPassword); ok {
		t.Fatal("旧密码应失效")
	}
	got, ok := m.Authenticate(DefaultAdminUsername, "newpass123")
	if !ok {
		t.Fatal("新密码应可登录")
	}
	if got.MustChangePassword {
		t.Fatal("改密后应清掉待改密标记")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	db := storetest.Open(t)
	m := NewManager(db)
	u, _, err := m.EnsureDefaultAdmin()
	if err != nil {
		t.Fatalf("建号失败: %v", err)
	}
	if err := m.ChangePassword(u.ID, DefaultAdminPassword, "s3cret-pass"); err != nil {
		t.Fatalf("改密失败: %v", err)
	}
	tok, err := m.IssueSession(u.ID, "", "")
	if err != nil {
		t.Fatalf("签会话失败: %v", err)
	}
	m.Flush()

	again := NewManager(db)
	defer again.Close()
	if _, ok := again.Authenticate(DefaultAdminUsername, "s3cret-pass"); !ok {
		t.Fatal("重启后账号应仍在")
	}
	if _, ok := again.SessionUser(tok); !ok {
		t.Fatal("重启后会话应仍有效（否则每次重启都要重登）")
	}
}

func TestCreateUserValidation(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.CreateUser("admin", "123456"); err != nil {
		t.Fatalf("第一个账号: %v", err)
	}
	if _, err := m.CreateUser("ADMIN", "123456"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("重名（大小写不同）应报 ErrDuplicate, got %v", err)
	}
	if _, err := m.CreateUser("bad name", "123456"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("非法用户名应拒绝, got %v", err)
	}
	if _, err := m.CreateUser("ok-user", "123"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("短密码应拒绝, got %v", err)
	}
	if n := len(m.ListUsers()); n != 1 {
		t.Fatalf("账号数应为 1, got %d", n)
	}
}

func TestCookieHelpers(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "tok123", true)
	res := rec.Result()
	cookies := res.Cookies()
	if len(cookies) != 1 || cookies[0].Name != SessionCookieName || cookies[0].Value != "tok123" {
		t.Fatalf("Cookie 不对: %+v", cookies)
	}
	if !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("Cookie 安全属性缺失: %+v", cookies[0])
	}
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.AddCookie(cookies[0])
	if got := TokenFromRequest(req); got != "tok123" {
		t.Fatalf("取 token 失败: %q", got)
	}
	req2 := httptest.NewRequest("GET", "/", nil)
	if got := TokenFromRequest(req2); got != "" {
		t.Fatalf("无 Cookie 应返回空, got %q", got)
	}
	rec2 := httptest.NewRecorder()
	ClearSessionCookie(rec2)
	if c := rec2.Result().Cookies()[0]; c.MaxAge >= 0 {
		t.Fatalf("清 Cookie 应置负 MaxAge, got %d", c.MaxAge)
	}
}

// TestCredentialsDurableImmediately 钉住「账号/口令的变更不等定时器」：
// 建号与改密都必须当场落盘。否则容器被 kill 的那 3 秒窗口里，重启后会出现
// 「初始密码还能登」或者「刚改的密码丢了」——两者都是安全属性上的倒退。
//
// 这里刻意不调用 Flush：模拟进程被强杀（只来得及把已经写进去的读回来）。
func TestCredentialsDurableImmediately(t *testing.T) {
	db := storetest.Open(t)
	m := NewManager(db)
	if _, created, err := m.EnsureDefaultAdmin(); err != nil || !created {
		t.Fatalf("EnsureDefaultAdmin created=%v err=%v", created, err)
	}
	m2 := NewManager(db) // 同一个库，新的 Manager = 重启
	u, ok := m2.Authenticate(DefaultAdminUsername, DefaultAdminPassword)
	if !ok {
		t.Fatal("首启建的 admin 没落盘：进程在 3 秒内重启就再也登不进去")
	}
	if err := m2.ChangePassword(u.ID, DefaultAdminPassword, "newpass789"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	m3 := NewManager(db)
	if _, ok := m3.Authenticate(DefaultAdminUsername, DefaultAdminPassword); ok {
		t.Fatal("旧密码在重启后仍有效：改密没有立即落盘")
	}
	if u2, ok := m3.Authenticate(DefaultAdminUsername, "newpass789"); !ok || u2.MustChangePassword {
		t.Fatalf("新密码登录失败或待改密标记没清：ok=%v mustChange=%v", ok, u2.MustChangePassword)
	}
}

// TestDefaultCredentialsPending 只关心「默认管理员是否还用着初始口令」这一位：
// 播种后为 true，改密后为 false；没有任何账号时也不能崩。
func TestDefaultCredentialsPending(t *testing.T) {
	var nilM *Manager
	if nilM.DefaultCredentialsPending() {
		t.Fatal("nil Manager 应回 false")
	}
	m := NewManager(nil)
	if m.DefaultCredentialsPending() {
		t.Fatal("空账号库不该报「还在用默认口令」")
	}
	u, _, err := m.EnsureDefaultAdmin()
	if err != nil {
		t.Fatal(err)
	}
	if !m.DefaultCredentialsPending() {
		t.Fatal("刚播种的 admin 应算作「仍在用初始口令」")
	}
	if err := m.ChangePassword(u.ID, DefaultAdminPassword, "newpass789"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if m.DefaultCredentialsPending() {
		t.Fatal("改密之后不该再提示初始口令")
	}
}
