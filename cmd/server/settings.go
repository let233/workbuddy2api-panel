// settings.go 运行期配置的持久化与进程内快照。
//
// 分工（本次改造的核心约定）：
//   - 配置文件只留「进程起来之前就必须知道」的东西：监听端口、数据目录、数据库连接。
//     这三样改了要重开库/重开监听，属于引导参数。
//   - 其余全部配置（上游参数、池策略、排程、提示词、告警、预算……）存数据库
//     （app_kv 的 config:settings 单键），面板保存即落库并热应用，重启不再是生效条件。
//   - api_key 不再写进配置文件：首次部署自动生成 sk- 前缀随机 key 入库，
//     避免「配置文件里躺着一个明文弱 key」这种最糟糕的组合。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/safefile"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// settingsKVKey 运行期配置在 app_kv 里的键。整块读写，不拆细键：
// 配置是一份整体（校验、归一化、热应用都按整份走），拆细键只会在部分写入时
// 留下一份「半新半旧」的配置，比整块覆盖难恢复得多。
const settingsKVKey = "config:settings"

// bootstrapKeys 是唯一允许留在配置文件里的字段。其余键出现在文件里一律当成
// 旧版遗留，迁移时搬进数据库并从文件里删掉。
var bootstrapKeys = []string{"listen", "state_file", "database"}

func isBootstrapKey(k string) bool {
	for _, b := range bootstrapKeys {
		if k == b {
			return true
		}
	}
	return false
}

// settingsHub 是进程内唯一的运行期配置快照 + 落库入口。
//
// 读侧（面板出配置、请求热路径里的少数一次性取值）走 Current() 原子读，
// 写侧（面板保存 / 旧配置迁移）串行化，保证「落库成功」与「内存可见」不会
// 出现「库里有新值、进程还在用旧值」这种半生效状态。
type settingsHub struct {
	db   *store.Store
	path string
	mu   sync.Mutex
	cur  atomic.Pointer[Config]
}

func newSettingsHub(db *store.Store, path string) *settingsHub {
	h := &settingsHub{db: db, path: path}
	h.cur.Store(Default())
	return h
}

// Current 当前生效配置（只读，调用方不得改写其字段）。
func (h *settingsHub) Current() *Config {
	if c := h.cur.Load(); c != nil {
		return c
	}
	return Default()
}

// Store 只更新内存快照（启动装配期用）。
func (h *settingsHub) Store(c *Config) { h.cur.Store(c) }

// Load 从库里读出运行期配置，覆盖在「文件 + 默认值」之上。
//
// legacyFile=true 表示文件里还写着全量配置（旧版部署 / 手写文件）：整份搬进库，
// 再由 Save 把文件改写成只剩端口与数据库，用户升级后不会「配置全丢回默认值」。
func (h *settingsHub) Load(base *Config, legacyFile bool) (migrated bool, err error) {
	raw, ok, err := store.KVGet(h.db, settingsKVKey)
	if err != nil {
		return false, fmt.Errorf("load settings: %w", err)
	}
	if ok && strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), base); err != nil {
			return false, fmt.Errorf("parse settings from db: %w", err)
		}
		// 派生字段（都是 json:"-" 的 *Dur / PromptText 之类）不落库，必须按库里的
		// 原始值重算，否则重启后「配置里显示 120s、实际生效 0s」。
		// 环境变量优先级高于库值，故 applyEnv 放在归一化之前。
		applyEnv(base)
		if err := base.normalize(); err != nil {
			return false, fmt.Errorf("normalize settings from db: %w", err)
		}
		h.cur.Store(base)
		return false, nil
	}
	if legacyFile {
		if err := h.Save(base); err != nil {
			return false, err
		}
		return true, nil
	}
	h.cur.Store(base)
	return false, nil
}

// Save 落库 + 落文件（只写引导字段）+ 更新内存快照。顺序固定：
// 先写库（真正的持久化），再写文件（引导参数），最后才让新值对读侧可见。
//
// 文件写失败不算保存失败：数据库里的设置已经生效，只有「端口/数据目录下次重启
// 是否还记得」这一点受影响。只读文件系统的容器里这是常态，为此让整次保存失败
// （连库里的设置都不落）才是真的糟。
func (h *settingsHub) Save(c *Config) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	payload, err := settingsPayload(c)
	if err != nil {
		return err
	}
	if err := store.KVSet(h.db, settingsKVKey, string(payload)); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	if h.path != "" {
		if err := WriteBootstrap(h.path, c); err != nil {
			log.Printf("[config] 写配置文件失败（设置已入库生效）：%v", err)
		}
	}
	h.cur.Store(c)
	return nil
}

// settingsPayload 生成入库 JSON：整份配置减掉配置文件专有的引导字段。
// 走 map 中转而不是另建一套 DTO：配置结构体已经很长，再加一份镜像结构
// 迟早与它漂移（新增字段忘了同步 = 面板保存一次就丢一次设置）。
func settingsPayload(c *Config) ([]byte, error) {
	full, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal settings: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(full, &doc); err != nil {
		return nil, fmt.Errorf("marshal settings: %w", err)
	}
	for _, k := range bootstrapKeys {
		delete(doc, k)
	}
	// PromptText 是 json:"-"/omitempty 的派生字段（normalize 时按 prompt 文件读出来），
	// 即使没入库也不会丢：重启后按 prompt.mode/file 重新读。
	return json.Marshal(doc)
}

// WriteBootstrap 把「端口 + 数据目录 + 数据库」写回配置文件。
//
// safefile.WriteKeep：保留上一份为 .bak，掉电留下半截正文也能回退。
func WriteBootstrap(path string, c *Config) error {
	doc := map[string]any{
		"_说明": "本文件只放启动期参数：监听端口、数据目录、数据库连接。" +
			"其余配置（上游/池/排程/提示词/告警/预算/api_key 等）已入库，请在面板里改，保存即生效、无需重启。",
		"listen":     c.Listen,
		"state_file": c.StateFile,
		"database": map[string]any{
			"driver": c.Database.Driver,
			"dsn":    c.Database.DSN,
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	return safefile.WriteKeep(path, out, 0o600)
}

// hasLegacySettings 判断一份「文件原始 JSON」里是否还有本该入库的字段。
func hasLegacySettings(raw []byte) bool {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	for k := range doc {
		if strings.HasPrefix(k, "_") || isBootstrapKey(k) {
			continue
		}
		return true
	}
	return false
}

// EnsureAPIKey 保证进程有一个可用的 api_key。
//
// 首次部署自动生成 sk- 前缀随机 key（crypto/rand 18 字节 → 24 字符 base64url），
// 与旧版 WriteDefault 生成的形态一致：随机 key 是安全默认，比示例占位符强得多，
// 而且现在它躺在库里（不是配置文件），面板里能看到、能改。
// 返回 true 表示本次是新生成的（调用方负责入库并打印到启动日志）。
func EnsureAPIKey(c *Config) bool {
	if strings.TrimSpace(c.APIKey) != "" {
		return false
	}
	c.APIKey = newAPIKey()
	return true
}

func newAPIKey() string {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand 在当前平台不可用极罕见；退回纳秒时间戳也强过空 key
		// （空 key = 网关完全不鉴权，而 listen 可能已经不是回环）。
		return "sk-" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(raw)
}
