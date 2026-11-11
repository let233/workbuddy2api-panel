package gatewaykeys

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

func newTestDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.Config{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "keys.db")})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestCreateDefaultsUnlimitedAndInherit(t *testing.T) {
	m := NewManager(nil)
	k, err := m.Create(Key{Name: "  默认  "})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.Name != "默认" || k.ID == "" {
		t.Fatalf("bad name/id: %+v", k)
	}
	if !strings.HasPrefix(k.Secret, "sk-") || len(k.Secret) < 20 {
		t.Fatalf("secret 未自动生成: %q", k.Secret)
	}
	if !k.Enabled {
		t.Fatal("新建 Key 应默认启用")
	}
	if len(k.Models) != 0 {
		t.Fatalf("模型空=全部，得到 %v", k.Models)
	}
	if k.PromptMode != ModeInherit {
		t.Fatalf("提示词默认跟随全局，得到 %q", k.PromptMode)
	}
	if k.StatusAt(time.Now()) != StatusOK {
		t.Fatalf("状态应为 ok")
	}
	if !k.AllowsModel("cn", "glm-5.2") {
		t.Fatal("空模型列表应允许全部模型")
	}
	if _, ok := m.Resolve(k.Secret); !ok {
		t.Fatal("生成后应能按 secret 解析")
	}
}

func TestStatusAndQuota(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	base := Key{Enabled: true}
	if got := (Key{Enabled: false}).StatusAt(now); got != StatusDisabled {
		t.Fatalf("disabled: %q", got)
	}
	exp := base
	exp.ExpiresAt = now.Unix() - 1
	if got := exp.StatusAt(now); got != StatusExpired {
		t.Fatalf("expired: %q", got)
	}
	quota := Key{Enabled: true, CreditLimit: 10, CreditUsed: 10}
	if got := quota.StatusAt(now); got != StatusQuotaExceeded {
		t.Fatalf("quota: %q", got)
	}
	if got := (Key{Enabled: true, CreditLimit: 10, CreditUsed: 9.5}).StatusAt(now); got != StatusOK {
		t.Fatalf("quota 未满: %q", got)
	}
}

func TestAllowsModelRealmSemantics(t *testing.T) {
	k := Key{Models: []string{"cn:glm-5.2", "global:claude-x", "kimi-k2"}}
	cases := []struct {
		realm, bare string
		want        bool
	}{
		{"cn", "glm-5.2", true},
		{"global", "glm-5.2", false},
		{"global", "claude-x", true},
		{"cn", "claude-x", false},
		{"global", "kimi-k2", true},
		{"cn", "kimi-k2", true},
		{"cn", "other", false},
	}
	for _, c := range cases {
		if got := k.AllowsModel(c.realm, c.bare); got != c.want {
			t.Fatalf("AllowsModel(%s,%s)=%v want %v", c.realm, c.bare, got, c.want)
		}
	}
	if !(Key{Models: []string{"*"}}).AllowsModel("cn", "anything") {
		t.Fatal("\"*\" 应视为全部")
	}
}

func TestCreateUpdateDeletePersist(t *testing.T) {
	db := newTestDB(t)
	m := NewManager(db)
	k, err := m.Create(Key{Name: "persist", CreditLimit: 5, Models: []string{"cn:glm-5.2"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	upd := k.Clone()
	upd.Name = "改名"
	upd.CreditUsed = 999 // 服务端保留旧值
	upd.Enabled = false
	got, err := m.Update(upd)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Name != "改名" || got.Enabled {
		t.Fatalf("更新未生效: %+v", got)
	}
	if got.CreditUsed != 0 {
		t.Fatalf("CreditUsed 不该被客户端覆盖: %v", got.CreditUsed)
	}

	m.Charge(k.ID, 1.25, true, 100)
	if err := m.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	reloaded := NewManager(db)
	defer reloaded.Close()
	rk, ok := reloaded.Get(k.ID)
	if !ok {
		t.Fatal("重载后找不到 Key")
	}
	if rk.CreditUsed != 1.25 || rk.Requests != 1 || rk.Tokens != 100 || rk.LastUsedAt == 0 {
		t.Fatalf("用量未持久化: %+v", rk)
	}
	if rk.Name != "改名" || rk.Enabled {
		t.Fatalf("配置未持久化: %+v", rk)
	}
	if _, ok := reloaded.Resolve(k.Secret); !ok {
		t.Fatal("secret 未持久化")
	}

	if _, err := reloaded.ResetUsage(k.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := reloaded.Delete(k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	after := NewManager(db)
	defer after.Close()
	if len(after.List()) != 0 {
		t.Fatal("删除未持久化")
	}
}

func TestDuplicateSecretRejected(t *testing.T) {
	m := NewManager(nil)
	k, err := m.Create(Key{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(Key{Name: "b", Secret: k.Secret}); err != ErrDuplicate {
		t.Fatalf("重复 secret 应拒绝，得到 %v", err)
	}
	if _, err := m.Create(Key{Name: "c", Secret: "bad secret"}); err != ErrInvalid {
		t.Fatalf("含空格的 secret 应拒绝，得到 %v", err)
	}
	if _, err := m.Create(Key{Name: "d", CreditLimit: -1}); err != ErrInvalid {
		t.Fatalf("负额度应拒绝，得到 %v", err)
	}
	if _, err := m.Create(Key{Name: "e", PromptMode: "wat"}); err != ErrInvalid {
		t.Fatalf("未知提示词模式应拒绝，得到 %v", err)
	}
	if _, err := m.Update(Key{ID: "nope"}); err != ErrNotFound {
		t.Fatalf("更新不存在应 ErrNotFound，得到 %v", err)
	}
}

// TestCreateWithDisabled 明确要求停用时不能落成启用。
func TestCreateWithDisabled(t *testing.T) {
	m := NewManager(nil)
	k, err := m.CreateWith(Key{Name: "off"}, CreateOptions{Disabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.Enabled || k.StatusAt(time.Now()) != StatusDisabled {
		t.Fatalf("应建成停用, got %+v", k)
	}
	if raw, _ := m.Create(Key{Name: "on"}); !raw.Enabled {
		t.Fatal("没说停用时应默认启用")
	}
}

func TestTemplatesCRUDAndApply(t *testing.T) {
	db := newTestDB(t)
	m := NewManager(db)
	tpl, err := m.SaveTemplate(Template{
		Name:        "高配额",
		Models:      []string{"cn:glm-5.2, kimi-k2", "cn:glm-5.2"},
		CreditLimit: 100,
		PromptMode:  ModeAppend,
		PromptText:  "你是助手",
		ExpiryDays:  7,
	})
	if err != nil {
		t.Fatalf("save template: %v", err)
	}
	if len(tpl.Models) != 2 {
		t.Fatalf("模型应去重并拆分逗号: %v", tpl.Models)
	}

	now := time.Unix(1_800_000_000, 0)
	k := ApplyTemplate(tpl, Key{Name: "x"}, now)
	if k.CreditLimit != 100 || k.PromptMode != ModeAppend || k.PromptText != "你是助手" {
		t.Fatalf("模板未套用: %+v", k)
	}
	want := now.AddDate(0, 0, 7).Unix()
	if k.ExpiresAt != want {
		t.Fatalf("到期时间 = %d want %d", k.ExpiresAt, want)
	}

	if _, err := m.SaveTemplate(Template{ID: tpl.ID, Name: "改名", PromptMode: ModeCustom}); err != nil {
		t.Fatalf("update template: %v", err)
	}
	reloaded := NewManager(db)
	defer reloaded.Close()
	got, ok := reloaded.GetTemplate(tpl.ID)
	if !ok || got.Name != "改名" || got.CreatedAt != tpl.CreatedAt {
		t.Fatalf("模板未持久化: %+v", got)
	}
	if err := reloaded.DeleteTemplate(tpl.ID); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	if _, err := m.SaveTemplate(Template{ID: "nope", Name: "x"}); err != ErrNotFound {
		t.Fatalf("更新不存在模板应 ErrNotFound，得到 %v", err)
	}
}
