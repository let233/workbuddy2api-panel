package proxypool

import (
	"encoding/json"
	"log"
	"sort"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// 存储布局（三条小表 + 一个 KV 键）：
//   - proxy_nodes     代理节点
//   - proxy_bindings  账号级出口绑定（uid → 绑定）
//   - proxy_realms    realm 级默认出口（cn/global → 绑定）
//   - app_kv[proxypool:settings]  探活设置
//
// 沿用旧的「整份状态整体重写」语义：表都很小（几十条），整体替换比逐行 diff
// 简单且不会漏删（与旧 proxy-pool.json 覆写一致）。
const (
	// legacyFileName 旧的 JSON 存储文件名（与 state 文件同目录）。
	legacyFileName = "proxy-pool.json"

	// settingsKVKey 探活设置的键。
	settingsKVKey = "proxypool:settings"
)

// TableName 固定表名（避免 gorm 复数化带来的歧义）。
func (node) TableName() string { return "proxy_nodes" }

// bindingRow 账号级出口绑定。
type bindingRow struct {
	UID     string `gorm:"column:uid;primaryKey;size:191"`
	Mode    string `gorm:"column:mode;size:16"`
	ProxyID string `gorm:"column:proxy_id;size:64"`
}

func (bindingRow) TableName() string { return "proxy_bindings" }

// realmRow realm 级默认出口。
type realmRow struct {
	Realm   string `gorm:"column:realm;primaryKey;size:16"`
	Mode    string `gorm:"column:mode;size:16"`
	ProxyID string `gorm:"column:proxy_id;size:64"`
}

func (realmRow) TableName() string { return "proxy_realms" }

func init() {
	store.RegisterModels(&node{}, &bindingRow{}, &realmRow{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "proxypool",
		Name: legacyFileName,
		Fn:   importLegacyProxies,
	})
}

// load 从数据库读取节点、绑定、域默认与探活设置。
func (m *Manager) load() error {
	nodes, err := store.LoadAll[node](m.db)
	if err != nil {
		return err
	}
	if nodes == nil {
		nodes = []node{}
	}
	m.data.Nodes = nodes

	binds, err := store.LoadAll[bindingRow](m.db)
	if err != nil {
		return err
	}
	m.data.Bindings = make(map[string]Binding, len(binds))
	for _, b := range binds {
		m.data.Bindings[b.UID] = Binding{Mode: b.Mode, ProxyID: b.ProxyID}
	}

	realms, err := store.LoadAll[realmRow](m.db)
	if err != nil {
		return err
	}
	m.data.Realms = make(map[string]Binding, len(realms))
	for _, r := range realms {
		m.data.Realms[r.Realm] = Binding{Mode: r.Mode, ProxyID: r.ProxyID}
	}

	raw, ok, err := store.KVGet(m.db, settingsKVKey)
	if err != nil {
		return err
	}
	if ok {
		var st Settings
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			// 坏设置不该让服务起不来：回落默认值（下方 validate 会再校验一次）。
			log.Printf("[proxy] 探活设置解析失败（回落默认值）: %v", err)
			st = DefaultSettings()
		}
		if st.URL != "" {
			m.data.Settings = st
		}
	}
	return nil
}

// persistLocked 把整份状态写入数据库（调用方持有 m.mu）。
func (m *Manager) persistLocked(d disk) error {
	if m.db == nil {
		return nil
	}
	nodes := make([]node, len(d.Nodes))
	copy(nodes, d.Nodes)
	if err := store.ReplaceAll(m.db, nodes); err != nil {
		return err
	}

	binds := make([]bindingRow, 0, len(d.Bindings))
	for uid, b := range d.Bindings {
		binds = append(binds, bindingRow{UID: uid, Mode: b.Mode, ProxyID: b.ProxyID})
	}
	sort.Slice(binds, func(i, j int) bool { return binds[i].UID < binds[j].UID })
	if err := store.ReplaceAll(m.db, binds); err != nil {
		return err
	}

	realms := make([]realmRow, 0, len(d.Realms))
	for realm, b := range d.Realms {
		realms = append(realms, realmRow{Realm: realm, Mode: b.Mode, ProxyID: b.ProxyID})
	}
	sort.Slice(realms, func(i, j int) bool { return realms[i].Realm < realms[j].Realm })
	if err := store.ReplaceAll(m.db, realms); err != nil {
		return err
	}

	raw, err := json.Marshal(d.Settings)
	if err != nil {
		return err
	}
	return store.KVSet(m.db, settingsKVKey, string(raw))
}

// importLegacyProxies 把旧的 proxy-pool.json 一次性导入数据库。
func importLegacyProxies(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, legacyFileName)
	if !ok {
		return nil
	}
	var d disk
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	nodes := d.Nodes
	if nodes == nil {
		nodes = []node{}
	}
	if err := store.ReplaceAll(s, nodes); err != nil {
		return err
	}
	binds := make([]bindingRow, 0, len(d.Bindings))
	for uid, b := range d.Bindings {
		binds = append(binds, bindingRow{UID: uid, Mode: b.Mode, ProxyID: b.ProxyID})
	}
	sort.Slice(binds, func(i, j int) bool { return binds[i].UID < binds[j].UID })
	if err := store.ReplaceAll(s, binds); err != nil {
		return err
	}
	realms := make([]realmRow, 0, len(d.Realms))
	for realm, b := range d.Realms {
		realms = append(realms, realmRow{Realm: realm, Mode: b.Mode, ProxyID: b.ProxyID})
	}
	sort.Slice(realms, func(i, j int) bool { return realms[i].Realm < realms[j].Realm })
	if err := store.ReplaceAll(s, realms); err != nil {
		return err
	}
	settings := d.Settings
	if settings.URL == "" {
		settings = DefaultSettings()
	}
	rawSettings, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := store.KVSet(s, settingsKVKey, string(rawSettings)); err != nil {
		return err
	}
	log.Printf("[proxy] 已从旧文件 %s 导入 %d 个代理节点 / %d 条账号绑定", legacyFileName, len(nodes), len(binds))
	return nil
}
