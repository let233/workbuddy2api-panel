// Package gatewaykeys 客户端 API Key 的多 Key 管理。
//
// 与面板「添加 API Key 账号」完全是两回事：那是池子里的**上游**静态凭证
// （ck_/pt_ 前缀，一个账号一份），本包管的是**下发给客户端**的网关 Key——
// 每个 Key 有自己的到期时间、可用模型、积分额度、系统提示词覆盖，
// 以及可复用的分组模板。
//
// 设计要点：
//   - 一个 Key 一条记录，JSON 存在 gateway_keys.payload，改一条只写一行；
//   - 内存里持有全量副本，鉴权路径零 DB 读；用量计数异步回写（Flush/定时器），
//     避免每个请求都开一次事务；
//   - Secret 比较用 SHA-256 + 常量时间比较；
//   - 空配置即「不限」：模型列表空 = 全部模型，提示词模式空 = 跟随全局。
package gatewaykeys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// 提示词模式：与全局配置（server/prompt）同名字面量；空串代表跟随全局。
const (
	ModeInherit     = ""
	ModeCustom      = "custom"
	ModePassthrough = "passthrough"
	ModeAppend      = "append"
)

// 生命周期状态（鉴权时按当前时间现算，不落库）。
const (
	StatusOK            = "ok"
	StatusDisabled      = "disabled"
	StatusExpired       = "expired"
	StatusQuotaExceeded = "quota_exceeded"
)

var (
	// ErrNotFound 目标 Key/模板不存在。
	ErrNotFound = errors.New("记录不存在")
	// ErrDuplicate 生成的或填写的 secret 与已有 Key 重复。
	ErrDuplicate = errors.New("该 API Key 已存在")
	// ErrInvalid 输入不合法（名称为空、额度为负、模式未知等）。
	ErrInvalid = errors.New("参数不合法")
)

// Key 一个下发给客户端的网关 Key。
type Key struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Secret      string   `json:"secret"`
	Enabled     bool     `json:"enabled"`
	ExpiresAt   int64    `json:"expires_at"`   // Unix 秒；0 = 永不过期
	CreditLimit float64  `json:"credit_limit"` // 0 = 不限
	CreditUsed  float64  `json:"credit_used"`
	Models      []string `json:"models"`      // 空 = 全部模型；支持 "cn:x"/"global:x" 限定 realm
	PromptMode  string   `json:"prompt_mode"` // 空 = 跟随全局
	PromptText  string   `json:"prompt_text"`
	TemplateID  string   `json:"template_id"`
	Note        string   `json:"note"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	LastUsedAt  int64    `json:"last_used_at"`
	Requests    int64    `json:"requests"`
	Tokens      int64    `json:"tokens"`
}

// Clone 深拷贝（Models 切片不能与内部状态共享）。
func (k Key) Clone() Key {
	out := k
	out.Models = append([]string(nil), k.Models...)
	return out
}

// StatusAt 返回当前生命周期状态。
func (k Key) StatusAt(now time.Time) string {
	if !k.Enabled {
		return StatusDisabled
	}
	if k.ExpiresAt > 0 && now.Unix() >= k.ExpiresAt {
		return StatusExpired
	}
	if k.CreditLimit > 0 && k.CreditUsed >= k.CreditLimit {
		return StatusQuotaExceeded
	}
	return StatusOK
}

// AllowsModel 判断该 Key 能否调用某个模型。
//
// 口径与全局白名单一致：空列表 = 全部允许；条目可写 "cn:model"/"global:model"
// 限定 realm，裸模型名两边都算；"*" 视为全部。
func (k Key) AllowsModel(realm, bareModel string) bool {
	if len(k.Models) == 0 {
		return true
	}
	realm = strings.ToLower(strings.TrimSpace(realm))
	bareModel = strings.TrimSpace(bareModel)
	for _, raw := range k.Models {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		if e == "*" {
			return true
		}
		if strings.Contains(e, ":") {
			er, em, ok := strings.Cut(e, ":")
			if ok && strings.EqualFold(strings.TrimSpace(er), realm) && strings.EqualFold(strings.TrimSpace(em), bareModel) {
				return true
			}
			continue
		}
		if strings.EqualFold(e, bareModel) {
			return true
		}
	}
	return false
}

// Template 分组模板：一组可复用的 Key 配置。
type Template struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Models      []string `json:"models"`
	CreditLimit float64  `json:"credit_limit"`
	PromptMode  string   `json:"prompt_mode"`
	PromptText  string   `json:"prompt_text"`
	ExpiryDays  int      `json:"expiry_days"` // 0 = 不设
	Note        string   `json:"note"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
}

// Clone 深拷贝。
func (t Template) Clone() Template {
	out := t
	out.Models = append([]string(nil), t.Models...)
	return out
}

// Manager Key/模板的并发安全仓库。
type Manager struct {
	mu        sync.RWMutex
	keys      map[string]*Key
	templates map[string]*Template
	db        *store.Store
	dirty     map[string]struct{}

	stop chan struct{}
	done chan struct{}
}

// NewManager 从库加载全量 Key 与模板；db 为 nil 时纯内存（测试用）。
// 返回的 Manager 会自动启动用量异步回写（有库时）。
func NewManager(db *store.Store) *Manager {
	m := &Manager{
		keys:      map[string]*Key{},
		templates: map[string]*Template{},
		db:        db,
		dirty:     map[string]struct{}{},
	}
	m.load()
	if db != nil {
		m.stop = make(chan struct{})
		m.done = make(chan struct{})
		go m.loop()
	}
	return m
}

// Close 停止回写协程并把未落库的计数刷进库。
func (m *Manager) Close() {
	if m == nil || m.stop == nil {
		return
	}
	close(m.stop)
	<-m.done
}

func (m *Manager) loop() {
	defer close(m.done)
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			_ = m.Flush()
			return
		case <-t.C:
			if err := m.Flush(); err != nil {
				log.Printf("gatewaykeys: 回写失败: %v", err)
			}
		}
	}
}

// List 返回全部 Key 的副本（新建的在前）。
func (m *Manager) List() []Key {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	out := make([]Key, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, k.Clone())
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get 按 ID 取副本。
func (m *Manager) Get(id string) (Key, bool) {
	if m == nil {
		return Key{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keys[id]
	if !ok {
		return Key{}, false
	}
	return k.Clone(), true
}

// Resolve 按明文 secret 查 Key（常量时间比较，不提前返回）。
func (m *Manager) Resolve(secret string) (Key, bool) {
	if m == nil || secret == "" {
		return Key{}, false
	}
	target := digest(secret)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var hit *Key
	for _, k := range m.keys {
		if subtle.ConstantTimeCompare(digest(k.Secret), target) == 1 {
			hit = k
		}
	}
	if hit == nil {
		return Key{}, false
	}
	return hit.Clone(), true
}

// 首次部署自动播种的「系统 Key」：积分不限、模型不限、提示词跟随全局。
const (
	SystemKeyName = "系统 Key"
	SystemKeyNote = "首次部署自动生成：不限额度、不限模型、提示词跟随全局配置"
)

// EnsureSystemKey 首次部署时播种一把最高级别 Key。
//
// 已经有任意 Key（比如管理员手工建过）时不动，返回 created=false——
// 免得每次重启都多出一把能用的万能 Key。
func (m *Manager) EnsureSystemKey() (Key, bool, error) {
	if m == nil {
		return Key{}, false, ErrInvalid
	}
	if len(m.List()) > 0 {
		return Key{}, false, nil
	}
	k, err := m.Create(Key{Name: SystemKeyName, Note: SystemKeyNote, PromptMode: ModeInherit})
	if err != nil {
		return Key{}, false, err
	}
	return k, true, nil
}

// CreateOptions 新建时机上的开关。
type CreateOptions struct {
	// Disabled 明确要求建一把停用的 Key。缺省（零值）= 启用。
	Disabled bool
}

// Create 新建 Key：缺省 secret 自动生成，id 自动分配，默认启用。
func (m *Manager) Create(in Key) (Key, error) {
	return m.CreateWith(in, CreateOptions{})
}

// CreateWith 同 Create，但可以让调用方明确要求「建出来就是停用」。
// 单靠 Key.Enabled 分不清「没传」与「传了 false」（bool 零值），
// 所以停用要单独说一句，免得面板上取消勾选「启用」却建出一把能用的 Key。
func (m *Manager) CreateWith(in Key, opt CreateOptions) (Key, error) {
	if m == nil {
		return Key{}, ErrInvalid
	}
	k, err := normalizeKey(in)
	if err != nil {
		return Key{}, err
	}
	k.Enabled = !opt.Disabled
	now := time.Now().Unix()
	k.ID = "gk_" + randomToken(12)
	k.CreatedAt, k.UpdatedAt = now, now
	k.CreditUsed = 0
	k.Requests, k.Tokens, k.LastUsedAt = 0, 0, 0
	if k.Secret == "" {
		k.Secret = "sk-" + randomToken(24)
	}
	m.mu.Lock()
	if m.secretTakenLocked(k.Secret, "") {
		m.mu.Unlock()
		return Key{}, ErrDuplicate
	}
	m.keys[k.ID] = &k
	stored := k.Clone()
	m.mu.Unlock()
	if err := m.persist(stored); err != nil {
		m.mu.Lock()
		delete(m.keys, k.ID)
		m.mu.Unlock()
		return Key{}, err
	}
	return stored, nil
}

// Update 更新配置字段；用量计数（CreditUsed/Requests/Tokens/LastUsedAt）与创建时间由服务端保留。
func (m *Manager) Update(in Key) (Key, error) {
	if m == nil {
		return Key{}, ErrInvalid
	}
	in.ID = strings.TrimSpace(in.ID)
	if in.ID == "" {
		return Key{}, ErrInvalid
	}
	k, err := normalizeKey(in)
	if err != nil {
		return Key{}, err
	}
	m.mu.Lock()
	cur, ok := m.keys[k.ID]
	if !ok {
		m.mu.Unlock()
		return Key{}, ErrNotFound
	}
	if k.Secret == "" {
		k.Secret = cur.Secret
	}
	if m.secretTakenLocked(k.Secret, k.ID) {
		m.mu.Unlock()
		return Key{}, ErrDuplicate
	}
	k.CreatedAt = cur.CreatedAt
	k.CreditUsed = cur.CreditUsed
	k.Requests = cur.Requests
	k.Tokens = cur.Tokens
	k.LastUsedAt = cur.LastUsedAt
	k.UpdatedAt = time.Now().Unix()
	*cur = k
	stored := cur.Clone()
	m.mu.Unlock()
	if err := m.persist(stored); err != nil {
		return Key{}, err
	}
	return stored, nil
}

// Delete 删除 Key。
func (m *Manager) Delete(id string) error {
	if m == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	if _, ok := m.keys[id]; !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.keys, id)
	delete(m.dirty, id)
	m.mu.Unlock()
	return store.DeleteIn[keyRow](m.db, "id", []string{id})
}

// ResetUsage 清零积分用量与流量计数（额度重新可用）。
func (m *Manager) ResetUsage(id string) (Key, error) {
	if m == nil {
		return Key{}, ErrNotFound
	}
	m.mu.Lock()
	k, ok := m.keys[id]
	if !ok {
		m.mu.Unlock()
		return Key{}, ErrNotFound
	}
	k.CreditUsed, k.Requests, k.Tokens = 0, 0, 0
	k.UpdatedAt = time.Now().Unix()
	stored := k.Clone()
	m.mu.Unlock()
	if err := m.persist(stored); err != nil {
		return Key{}, err
	}
	return stored, nil
}

// Charge 记一次上游尝试：credit 仅在 hasCredit 时累计（上游没给可信额度就不扣）。
func (m *Manager) Charge(id string, credit float64, hasCredit bool, tokens int64) {
	if m == nil || id == "" {
		return
	}
	m.mu.Lock()
	k, ok := m.keys[id]
	if ok {
		if hasCredit && credit > 0 {
			k.CreditUsed += credit
		}
		k.Requests++
		if tokens > 0 {
			k.Tokens += tokens
		}
		k.LastUsedAt = time.Now().Unix()
		m.dirty[id] = struct{}{}
	}
	m.mu.Unlock()
}

// Flush 把变更过的 Key 写回库。写失败的行重新标脏，下轮再试。
func (m *Manager) Flush() error {
	if m == nil || m.db == nil {
		return nil
	}
	m.mu.Lock()
	rows := make([]keyRow, 0, len(m.dirty))
	ids := make([]string, 0, len(m.dirty))
	for id := range m.dirty {
		k, ok := m.keys[id]
		if !ok {
			continue
		}
		raw, err := json.Marshal(k)
		if err != nil {
			continue
		}
		rows = append(rows, keyRow{ID: id, Payload: string(raw), UpdatedAt: time.Unix(k.UpdatedAt, 0)})
		ids = append(ids, id)
	}
	for _, id := range ids {
		delete(m.dirty, id)
	}
	m.mu.Unlock()
	if len(rows) == 0 {
		return nil
	}
	if err := store.Upsert[keyRow](m.db, rows, []string{"id"}, []string{"payload", "updated_at"}); err != nil {
		m.mu.Lock()
		for _, id := range ids {
			m.dirty[id] = struct{}{}
		}
		m.mu.Unlock()
		return err
	}
	return nil
}

// ---- 模板 ----

// ListTemplates 返回全部模板副本（新建的在前）。
func (m *Manager) ListTemplates() []Template {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	out := make([]Template, 0, len(m.templates))
	for _, t := range m.templates {
		out = append(out, t.Clone())
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// GetTemplate 按 ID 取模板。
func (m *Manager) GetTemplate(id string) (Template, bool) {
	if m == nil {
		return Template{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.templates[id]
	if !ok {
		return Template{}, false
	}
	return t.Clone(), true
}

// SaveTemplate 新建（ID 空）或更新模板。
func (m *Manager) SaveTemplate(in Template) (Template, error) {
	if m == nil {
		return Template{}, ErrInvalid
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return Template{}, ErrInvalid
	}
	if in.CreditLimit < 0 || in.ExpiryDays < 0 {
		return Template{}, ErrInvalid
	}
	in.Models = normalizeModels(in.Models)
	in.PromptMode = strings.ToLower(strings.TrimSpace(in.PromptMode))
	if !validMode(in.PromptMode) {
		return Template{}, ErrInvalid
	}
	now := time.Now().Unix()
	in.ID = strings.TrimSpace(in.ID)
	m.mu.Lock()
	if in.ID == "" {
		in.ID = "gt_" + randomToken(10)
		in.CreatedAt = now
	} else if cur, ok := m.templates[in.ID]; ok {
		in.CreatedAt = cur.CreatedAt
	} else {
		m.mu.Unlock()
		return Template{}, ErrNotFound
	}
	in.UpdatedAt = now
	stored := in.Clone()
	cp := stored.Clone()
	m.templates[in.ID] = &cp
	m.mu.Unlock()
	if err := m.persistTemplate(stored); err != nil {
		return Template{}, err
	}
	return stored, nil
}

// DeleteTemplate 删除模板。已套用该模板的 Key 不受影响（配置已复制过去）。
func (m *Manager) DeleteTemplate(id string) error {
	if m == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	if _, ok := m.templates[id]; !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.templates, id)
	m.mu.Unlock()
	return store.DeleteIn[templateRow](m.db, "id", []string{id})
}

// ApplyTemplate 把模板配置写进 Key（不覆盖显式传入的名称/备注/secret）。
// 到期时间：Key 没设且模板有 ExpiryDays 时，按天数从 now 起算。
func ApplyTemplate(tpl Template, k Key, now time.Time) Key {
	k.Models = normalizeModels(tpl.Models)
	k.CreditLimit = tpl.CreditLimit
	k.PromptMode = tpl.PromptMode
	k.PromptText = tpl.PromptText
	if k.TemplateID == "" {
		k.TemplateID = tpl.ID
	}
	if k.ExpiresAt == 0 && tpl.ExpiryDays > 0 {
		k.ExpiresAt = now.AddDate(0, 0, tpl.ExpiryDays).Unix()
	}
	return k
}

// ---- 内部 ----

func normalizeKey(in Key) (Key, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "未命名 Key"
	}
	if len(in.Name) > 64 {
		in.Name = in.Name[:64]
	}
	in.Secret = strings.TrimSpace(in.Secret)
	if strings.ContainsAny(in.Secret, " \t\r\n") {
		return Key{}, ErrInvalid
	}
	if in.CreditLimit < 0 {
		return Key{}, ErrInvalid
	}
	if in.ExpiresAt < 0 {
		return Key{}, ErrInvalid
	}
	in.Models = normalizeModels(in.Models)
	in.PromptMode = strings.ToLower(strings.TrimSpace(in.PromptMode))
	if !validMode(in.PromptMode) {
		return Key{}, ErrInvalid
	}
	in.Note = strings.TrimSpace(in.Note)
	return in, nil
}

func validMode(m string) bool {
	switch m {
	case ModeInherit, ModeCustom, ModePassthrough, ModeAppend:
		return true
	}
	return false
}

func normalizeModels(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
			e := strings.TrimSpace(part)
			if e == "" {
				continue
			}
			if _, ok := seen[strings.ToLower(e)]; ok {
				continue
			}
			seen[strings.ToLower(e)] = struct{}{}
			out = append(out, e)
		}
	}
	return out
}

func (m *Manager) secretTakenLocked(secret, exceptID string) bool {
	for id, k := range m.keys {
		if id == exceptID {
			continue
		}
		if k.Secret == secret {
			return true
		}
	}
	return false
}

func (m *Manager) load() {
	if m.db == nil {
		return
	}
	rows, err := store.LoadAll[keyRow](m.db)
	if err != nil {
		log.Printf("gatewaykeys: 读取失败: %v", err)
	}
	for _, row := range rows {
		var k Key
		if err := json.Unmarshal([]byte(row.Payload), &k); err != nil || k.ID == "" {
			continue
		}
		kk := k
		m.keys[k.ID] = &kk
	}
	trows, err := store.LoadAll[templateRow](m.db)
	if err != nil {
		log.Printf("gatewaykeys: 读取模板失败: %v", err)
	}
	for _, row := range trows {
		var t Template
		if err := json.Unmarshal([]byte(row.Payload), &t); err != nil || t.ID == "" {
			continue
		}
		tt := t
		m.templates[t.ID] = &tt
	}
}

func (m *Manager) persist(k Key) error {
	if m.db == nil {
		return nil
	}
	raw, err := json.Marshal(k)
	if err != nil {
		return err
	}
	return store.Upsert[keyRow](m.db, []keyRow{{ID: k.ID, Payload: string(raw), UpdatedAt: time.Unix(k.UpdatedAt, 0)}}, []string{"id"}, []string{"payload", "updated_at"})
}

func (m *Manager) persistTemplate(t Template) error {
	if m.db == nil {
		return nil
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return store.Upsert[templateRow](m.db, []templateRow{{ID: t.ID, Payload: string(raw), UpdatedAt: time.Unix(t.UpdatedAt, 0)}}, []string{"id"}, []string{"payload", "updated_at"})
}

func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func randomToken(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return base64.RawURLEncoding.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
