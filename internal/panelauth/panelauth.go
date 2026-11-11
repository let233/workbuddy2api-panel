// Package panelauth 面板登录账号与会话。
//
// 背景：面板与网关原本共用一个全局 api_key（Authorization: Bearer）。改成账号密码
// 登录后，面板只认会话 Cookie，网关只认「客户端 API Key」（internal/gatewaykeys），
// 两者不再共享任何静态密钥。
//
// 账号密码用 bcrypt 派生后落库；会话只存 token 的 sha256，明文 token 只出现在
// 签发那一刻的 Set-Cookie 里——库泄露也拿不到可直接登录的凭证。
package panelauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

const (
	// SessionCookieName 面板会话 Cookie 名。
	SessionCookieName = "wb2api_session"
	// SessionTTL 会话有效期（面板是低频操作，给足 30 天，到期需重新登录）。
	SessionTTL = 30 * 24 * time.Hour
	// DefaultAdminUsername / DefaultAdminPassword 首次部署的初始账号。
	DefaultAdminUsername = "admin"
	DefaultAdminPassword = "123456"

	minPasswordLen = 6
	maxPasswordLen = 72 // bcrypt 只取前 72 字节，超出直接拒绝而不是静默截断
	flushInterval  = 3 * time.Second
	touchInterval  = time.Minute
	usernameMin    = 2
	usernameMax    = 32
)

// 领域错误：面板据此映射 HTTP 状态码。
var (
	ErrInvalid     = errors.New("参数不合法")
	ErrDuplicate   = errors.New("账号已存在")
	ErrNotFound    = errors.New("账号不存在")
	ErrBadPassword = errors.New("账号或密码不正确")
)

// User 面板登录账号。PasswordHash 只存在于库里，出站前一律走 Public()。
type User struct {
	ID                 string `json:"id"`
	Username           string `json:"username"`
	PasswordHash       string `json:"password_hash,omitempty"`
	IsAdmin            bool   `json:"is_admin"`
	MustChangePassword bool   `json:"must_change_password"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
	LastLoginAt        int64  `json:"last_login_at"`
}

// Public 去掉密码哈希的副本，供响应体使用。
func (u User) Public() User {
	u.PasswordHash = ""
	return u
}

// Session 一次登录会话。TokenHash = sha256(token) 的小写十六进制。
type Session struct {
	TokenHash  string `json:"token_hash"`
	UserID     string `json:"user_id"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	LastSeenAt int64  `json:"last_seen_at"`
	ClientIP   string `json:"client_ip,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
}

// Manager 账号与会话的唯一入口。db 为 nil 时纯内存（进程重启即丢，
// 用于无库模式与单测）；有库时脏行每 3s 落盘，Close 前也会 Flush。
type Manager struct {
	db *store.Store

	mu       sync.Mutex
	users    map[string]User    // by ID
	sessions map[string]Session // by TokenHash
	dirty    bool

	dummyOnce sync.Once
	dummyHash []byte

	stop chan struct{}
	done chan struct{}
}

// NewManager 打开账号与会话存储；db 为 nil 时退化为内存实现。
func NewManager(db *store.Store) *Manager {
	m := &Manager{
		db:       db,
		users:    make(map[string]User, 4),
		sessions: make(map[string]Session, 8),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if db != nil {
		m.load()
		go m.loop()
	} else {
		close(m.done)
	}
	m.purgeExpiredLocked(time.Now())
	return m
}

// Close 停后台落盘并做最后一次 Flush。
func (m *Manager) Close() {
	if m == nil {
		return
	}
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
	m.Flush()
}

func (m *Manager) loop() {
	defer close(m.done)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.Flush()
		}
	}
}

// Flush 把脏行写到库里。
func (m *Manager) Flush() {
	if m == nil || m.db == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushLocked()
}

// flushLocked 在持锁状态下立刻落盘（调用方必须已持有 m.mu）。
//
// 为什么要有同步版本：账号与口令的变更不能等 3 秒的定时器。容器被 kill、
// 断电、OOM 都发生在「用户以为已经生效」之后——那时旧密码还能登进去、
// 或者刚改的新密码丢了，是安全属性上的倒退。会话的写仍然可以攒（丢了
// 只是重新登录一次），但 users 表跟着一起写，代价只有一次 ms 级 ReplaceAll。
func (m *Manager) flushLocked() {
	if m.db == nil || !m.dirty {
		return
	}
	userRows := make([]userRow, 0, len(m.users))
	for _, u := range m.users {
		userRows = append(userRows, userRow{ID: u.ID, Payload: mustJSON(u), UpdatedAt: time.Now()})
	}
	sessionRows := make([]sessionRow, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessionRows = append(sessionRows, sessionRow{TokenHash: s.TokenHash, Payload: mustJSON(s), ExpiresAt: s.ExpiresAt})
	}
	m.dirty = false
	if err := store.ReplaceAll(m.db, userRows); err != nil {
		m.dirty = true
		return
	}
	if err := store.ReplaceAll(m.db, sessionRows); err != nil {
		m.dirty = true
	}
}

func (m *Manager) markDirty() {
	m.mu.Lock()
	m.dirty = true
	m.mu.Unlock()
}

func (m *Manager) load() {
	if rows, err := store.LoadAll[userRow](m.db); err == nil {
		for _, r := range rows {
			var u User
			if decodeJSON(r.Payload, &u) && u.ID != "" {
				m.users[u.ID] = u
			}
		}
	}
	if rows, err := store.LoadAll[sessionRow](m.db); err == nil {
		for _, r := range rows {
			var s Session
			if decodeJSON(r.Payload, &s) && s.TokenHash != "" {
				m.sessions[s.TokenHash] = s
			}
		}
	}
}

// EnsureDefaultAdmin 首次部署创建 admin/123456（已有一个账号时什么都不做）。
// 返回 created=true 表示这是首次创建，调用方应打印一次初始密码提示。
func (m *Manager) EnsureDefaultAdmin() (User, bool, error) {
	return m.EnsureAdmin(DefaultAdminUsername, DefaultAdminPassword)
}

// EnsureAdmin 在没有账号时创建指定管理员。已有账号时返回 (零值, false, nil)。
func (m *Manager) EnsureAdmin(username, password string) (User, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.users) > 0 {
		return User{}, false, nil
	}
	u, err := m.createLocked(username, password, true, true)
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

// DefaultCredentialsPending 报告默认管理员是否还在用初始化口令。
// 只服务面板登录框：决定要不要显示「首次部署的默认账号 admin / 密码 123456」。
// 这句话在改过密码之后就是错的——照它试只会白挨一次 401，所以必须能关掉。
//
// 判定看 MustChangePassword（播种置 true、首次改密清掉），而不是每次拿默认口令
// 走一遍 bcrypt：那是一次 60ms 级开销，还要把默认口令塞进会话轮询的热路径。
func (m *Manager) DefaultCredentialsPending() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if strings.EqualFold(u.Username, DefaultAdminUsername) {
			return u.MustChangePassword
		}
	}
	return false
}

// CreateUser 新建普通账号（面板暂不暴露用户管理 UI，留给运维/脚本用）。
func (m *Manager) CreateUser(username, password string) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createLocked(username, password, false, false)
}

func (m *Manager) createLocked(username, password string, admin, mustChange bool) (User, error) {
	name, err := normalizeUsername(username)
	if err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	for _, u := range m.users {
		if strings.EqualFold(u.Username, name) {
			return User{}, ErrDuplicate
		}
	}
	now := time.Now().Unix()
	u := User{
		ID:                 "u_" + randomToken(9),
		Username:           name,
		PasswordHash:       hash,
		IsAdmin:            admin,
		MustChangePassword: mustChange,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	m.users[u.ID] = u
	m.dirty = true
	m.flushLocked() // 首次部署的 admin 不能只活在内存里：重启窗口内进程被杀 = 面板再也进不去
	return u, nil
}

// HashPassword bcrypt 派生（默认 cost）。
func HashPassword(password string) (string, error) {
	if len(password) < minPasswordLen || len(password) > maxPasswordLen {
		return "", ErrInvalid
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// Authenticate 校验账号密码；成功时刷新 LastLoginAt。
//
// 账号不存在也走一次 bcrypt 比对，避免用响应时间判断账号是否存在。
func (m *Manager) Authenticate(username, password string) (User, bool) {
	name := strings.ToLower(strings.TrimSpace(username))
	m.mu.Lock()
	var found User
	ok := false
	for _, u := range m.users {
		if strings.EqualFold(u.Username, name) {
			found, ok = u, true
			break
		}
	}
	m.mu.Unlock()

	if !ok {
		bcrypt.CompareHashAndPassword(m.dummy(), []byte(password))
		return User{}, false
	}
	if bcrypt.CompareHashAndPassword([]byte(found.PasswordHash), []byte(password)) != nil {
		return User{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, still := m.users[found.ID]; still {
		cur.LastLoginAt = time.Now().Unix()
		m.users[found.ID] = cur
		m.dirty = true
		found = cur
	}
	return found.Public(), true
}

func (m *Manager) dummy() []byte {
	m.dummyOnce.Do(func() {
		h, err := bcrypt.GenerateFromPassword([]byte("wb2api-dummy-password"), bcrypt.DefaultCost)
		if err != nil {
			h = []byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvali")
		}
		m.dummyHash = h
	})
	return m.dummyHash
}

// IssueSession 为账号签发新会话，返回明文 token（只此一次）。
func (m *Manager) IssueSession(userID, clientIP, userAgent string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[userID]; !ok {
		return "", ErrNotFound
	}
	m.purgeExpiredLocked(time.Now())
	token := randomToken(32)
	now := time.Now()
	m.sessions[hashToken(token)] = Session{
		TokenHash:  hashToken(token),
		UserID:     userID,
		CreatedAt:  now.Unix(),
		ExpiresAt:  now.Add(SessionTTL).Unix(),
		LastSeenAt: now.Unix(),
		ClientIP:   clientIP,
		UserAgent:  truncate(userAgent, 120),
	}
	m.dirty = true
	return token, nil
}

// SessionUser 按 token 取账号（过期/已失效返回 false）。
func (m *Manager) SessionUser(token string) (User, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return User{}, false
	}
	key := hashToken(token)
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[key]
	if !ok {
		return User{}, false
	}
	if now.Unix() >= s.ExpiresAt {
		delete(m.sessions, key)
		m.dirty = true
		return User{}, false
	}
	u, ok := m.users[s.UserID]
	if !ok {
		delete(m.sessions, key)
		m.dirty = true
		return User{}, false
	}
	// 每分钟最多写一次 LastSeenAt：面板会轮询，别把每次请求都变成一次落盘。
	if now.Unix()-s.LastSeenAt >= int64(touchInterval/time.Second) {
		s.LastSeenAt = now.Unix()
		m.sessions[key] = s
		m.dirty = true
	}
	return u.Public(), true
}

// DropSession 注销单个会话。
func (m *Manager) DropSession(token string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	key := hashToken(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[key]; ok {
		delete(m.sessions, key)
		m.dirty = true
	}
}

// DropUserSessions 踢掉某账号的全部会话（改密后用）。
func (m *Manager) DropUserSessions(userID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, s := range m.sessions {
		if s.UserID == userID {
			delete(m.sessions, key)
			m.dirty = true
		}
	}
}

// ChangePassword 改自己的密码；旧密码不对返回 ErrBadPassword。
// 默认口令的「待改密」标记在这里清掉。
func (m *Manager) ChangePassword(userID, oldPassword, newPassword string) error {
	m.mu.Lock()
	u, ok := m.users[userID]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPassword)) != nil {
		return ErrBadPassword
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, still := m.users[userID]
	if !still {
		return ErrNotFound
	}
	cur.PasswordHash = hash
	cur.MustChangePassword = false
	cur.UpdatedAt = time.Now().Unix()
	m.users[userID] = cur
	m.dirty = true
	// 立即落盘：改密是「安全操作」，不能等 3 秒定时器。旧密码在重启后还能用的
	// 那段时间里，运维会以为已经收紧了入口。
	m.flushLocked()
	return nil
}

// UserByID 取账号（Public 副本）。
func (m *Manager) UserByID(id string) (User, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return User{}, false
	}
	return u.Public(), true
}

// ListUsers 全部账号（Public 副本），按创建时间升序。
func (m *Manager) ListUsers() []User {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, u.Public())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].Username < out[j].Username
	})
	return out
}

func (m *Manager) purgeExpiredLocked(now time.Time) {
	n := now.Unix()
	for key, s := range m.sessions {
		if n >= s.ExpiresAt {
			delete(m.sessions, key)
			m.dirty = true
		}
	}
}

// SetSessionCookie 下发会话 Cookie。secure 在 HTTPS（含反代 X-Forwarded-Proto）
// 场景应为 true。
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(SessionTTL / time.Second),
	})
}

// ClearSessionCookie 让浏览器立刻丢掉会话 Cookie。
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

// TokenFromRequest 从请求里取会话 token（只认 Cookie）。
func TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

func normalizeUsername(s string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(s))
	if len(name) < usernameMin || len(name) > usernameMax {
		return "", ErrInvalid
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return "", ErrInvalid
		}
	}
	return name, nil
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败在现代系统上几乎不可能；退化成时间戳也让调用方拿到 token，
		// 但绝不能返回空串（空 token 等于无凭据）。
		return base64.RawURLEncoding.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
