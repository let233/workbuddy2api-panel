package pool

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// 表名与注册：pool 自己不依赖任何上层包，只往 store 里自注册两张表，
// store.Open 的 AutoMigrate 会据此建表/补列。
func (stateAccount) TableName() string { return "pool_accounts" }

func (stateModelWindow) TableName() string { return "pool_windows" }

func init() {
	store.RegisterModels(&stateAccount{}, &stateModelWindow{})
	store.RegisterLegacy(store.LegacyImport{Key: "pool", Name: "state.json", Fn: importLegacyState})
}

// importLegacyState 把旧 data/state.json 一次性搬进 pool_accounts / pool_windows。
//
// 旧格式的账号以 map 键（uid）索引、JSON 里没有 uid 字段，窗口表的键把 (uid, model)
// 用 \x00 拼在一起，所以两个方向都要还原。导入后登录态 auths/*.json 仍由
// auth.LoadDir 读取（那一层不换存储），余额/冷却/用量计数都在这里接上。
func importLegacyState(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, "state.json")
	if !ok {
		return nil
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return err
	}
	accounts := make([]stateAccount, 0, len(sf.Accounts))
	for uid, acc := range sf.Accounts {
		acc.UID = uid
		accounts = append(accounts, acc)
	}
	windows := make([]stateModelWindow, 0, len(sf.Windows))
	for key, w := range sf.Windows {
		if w.UID == "" || w.Model == "" {
			if i := strings.IndexByte(key, 0); i > 0 {
				w.UID, w.Model = key[:i], key[i+1:]
			}
		}
		if w.UID == "" || w.Model == "" {
			continue
		}
		windows = append(windows, w)
	}
	if err := store.ReplaceAll(s, accounts); err != nil {
		return err
	}
	if err := store.ReplaceAll(s, windows); err != nil {
		return err
	}
	log.Printf("pool: 已从旧文件 state.json 导入 %d 个账号 / %d 个额度窗口", len(accounts), len(windows))
	return nil
}

// poolSavedAtKey 池状态最后一次落盘的时刻（RFC3339Nano），Redis 快照择新恢复要与
// 快照里的 saved_at 比较，所以必须单独留一行。
const poolSavedAtKey = "pool:saved_at"
