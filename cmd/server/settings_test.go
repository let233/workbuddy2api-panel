package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

func openTestDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(store.Config{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// 首次部署：没有 api_key 时必须自动生成 sk- 前缀的随机 key（入库），两次生成不能相同。
func TestEnsureAPIKeyGeneratesRandomSK(t *testing.T) {
	c := Default()
	c.APIKey = "  "
	if !EnsureAPIKey(c) {
		t.Fatal("空 api_key 应触发生成")
	}
	if !strings.HasPrefix(c.APIKey, "sk-") {
		t.Fatalf("key=%q 缺少 sk- 前缀", c.APIKey)
	}
	// 18 字节 → base64url 无填充 24 字符
	if len(c.APIKey) != len("sk-")+24 {
		t.Fatalf("key=%q 长度 %d，期望 %d", c.APIKey, len(c.APIKey), len("sk-")+24)
	}
	if EnsureAPIKey(c) {
		t.Fatal("已有 key 时不该重新生成（面板改过的 key 不能被抹掉）")
	}
	other := Default()
	EnsureAPIKey(other)
	if other.APIKey == c.APIKey {
		t.Fatal("两次生成的 key 相同，随机源有问题")
	}
}

// 入库 payload 里不允许出现引导参数：那三样留在文件里，重复存一份只会让「文件改端口、
// 库里改端口」互相打架。
func TestSettingsPayloadDropsBootstrapKeys(t *testing.T) {
	c := Default()
	c.APIKey = "k"
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	payload, err := settingsPayload(c)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range bootstrapKeys {
		if _, ok := doc[k]; ok {
			t.Errorf("入库 payload 里出现了引导参数 %q", k)
		}
	}
	for _, k := range []string{"api_key", "upstream", "pool", "server"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("入库 payload 缺少运行期配置 %q", k)
		}
	}
}

// 引导参数文件只剩三样（外加一句说明）：写进文件却要重启才生效的键，是这次改造要
// 彻底消灭的形态。
func TestWriteBootstrapOnlyKeepsBootstrapKeys(t *testing.T) {
	c := Default()
	c.Listen = "127.0.0.1:7777"
	c.StateFile = "/tmp/state.db"
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	if err := WriteBootstrap(path, c); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("文件不是合法 JSON: %v", err)
	}
	for k := range doc {
		if strings.HasPrefix(k, "_") || isBootstrapKey(k) {
			continue
		}
		t.Errorf("引导文件里不该出现 %q", k)
	}
	if doc["listen"] != "127.0.0.1:7777" {
		t.Errorf("listen=%v", doc["listen"])
	}
	if db, ok := doc["database"].(map[string]any); !ok || db["driver"] == nil {
		t.Errorf("database=%v", doc["database"])
	}
}

// 旧版部署（config.json 里还写着全量配置）升级后：整份搬进库，文件改写成只剩引导
// 参数，且 api_key 跟着一起搬（否则升级即丢 key，面板全部会话失效）。
func TestSettingsMigrateLegacyFile(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	legacy := Default()
	legacy.APIKey = "sk-legacykey1234"
	legacy.Upstream.UserAgent = "legacy-agent"
	legacy.Server.ReadTimeout = "42s"
	if err := legacy.normalize(); err != nil {
		t.Fatal(err)
	}
	rawLegacy, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rawLegacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasLegacySettings(rawLegacy) {
		t.Fatal("全量配置文件应被判定为遗留形态")
	}

	// 与 main 同序：Load 文件 → hub.Load(base, legacyFile=true) → 迁移入库
	base, err := ParseConfig(rawLegacy)
	if err != nil {
		t.Fatal(err)
	}
	hub := newSettingsHub(db, path)
	migrated, err := hub.Load(base, true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !migrated {
		t.Fatal("遗留文件未被迁移")
	}
	if got := hub.Current().Upstream.UserAgent; got != "legacy-agent" {
		t.Fatalf("迁移后 UA = %q", got)
	}
	if got := hub.Current().APIKey; got != "sk-legacykey1234" {
		t.Fatalf("迁移后 api_key = %q（升级丢 key 是致命的）", got)
	}

	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if hasLegacySettings(rewritten) {
		t.Fatal("迁移后文件里仍留着运行期配置")
	}

	// 重启：库里那份优先，且派生字段（*Dur）要按库里原值重算。
	hub2 := newSettingsHub(db, path)
	if _, err := hub2.Load(Default(), false); err != nil {
		t.Fatal(err)
	}
	cur := hub2.Current()
	if cur.Upstream.UserAgent != "legacy-agent" {
		t.Fatalf("从库读回 UA = %q", cur.Upstream.UserAgent)
	}
	if cur.ServerReadTimeoutDur != 42*time.Second {
		t.Fatalf("从库读回 read_timeout = %v，期望 42s", cur.ServerReadTimeoutDur)
	}
}

// 已经是引导参数形态的文件不该再被判成遗留（否则每次启动都重写一遍文件）。
func TestHasLegacySettings(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	payload, err := settingsPayload(c)
	if err != nil {
		t.Fatal(err)
	}
	// payload 是「库里那份」：它当然全是运行期配置，正是 hasLegacySettings 要认出的东西
	// （main 用它判断磁盘文件里还剩不剩该搬进库的键）。
	if !hasLegacySettings(payload) {
		t.Fatal("运行期 payload 应被判为「有配置要入库」")
	}
	if hasLegacySettings([]byte(`{"_说明":"x","listen":":7863","state_file":"s.db","database":{"driver":"sqlite","dsn":"d.db"}}`)) {
		t.Fatal("引导参数形式的文件被误判为遗留")
	}
	if !hasLegacySettings([]byte(`{"listen":":7863","upstream":{"user_agent":"x"}}`)) {
		t.Fatal("带 upstream 的文件应判为遗留")
	}
}
