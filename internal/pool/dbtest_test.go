package pool

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// openTestDB 在 dir 下开一个 sqlite 库（同一 dir 复用同一文件 = 同一份"磁盘状态"，
// 用于模拟"重启后从库重载"）。库文件随 t.TempDir 一起清理。
func openTestDB(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(dir, "wb2api.db")})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// tempTestDB 每次一个新库（不需要跨重启共享时用）。
func tempTestDB(t *testing.T) *store.Store {
	t.Helper()
	return openTestDB(t, t.TempDir())
}

// dbStateJSON 把库里的池状态还原成旧 state.json 的 JSON 形态：既有断言大量按
// 字段子串匹配落盘内容，保留这层还原能让它们逐字复用（也顺带钉住"库→JSON 视图"不丢字段）。
func dbStateJSON(t *testing.T, st *store.Store) []byte {
	t.Helper()
	sf := stateFile{Accounts: map[string]stateAccount{}}
	accs, err := store.LoadAll[stateAccount](st)
	if err != nil {
		t.Fatalf("读取 pool_accounts 失败: %v", err)
	}
	for _, a := range accs {
		sf.Accounts[a.UID] = a
	}
	wins, err := store.LoadAll[stateModelWindow](st)
	if err != nil {
		t.Fatalf("读取 pool_windows 失败: %v", err)
	}
	if len(wins) > 0 {
		sf.Windows = map[string]stateModelWindow{}
		for _, w := range wins {
			sf.Windows[windowKey(w.UID, w.Model)] = w
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		t.Fatalf("序列化库内状态失败: %v", err)
	}
	return raw
}

// seedStateDB 把旧 state.json 形态的内容直接写进库（模拟"从旧文件迁移过来"）。
func seedStateDB(t *testing.T, st *store.Store, sf stateFile) {
	t.Helper()
	accs := make([]stateAccount, 0, len(sf.Accounts))
	for uid, a := range sf.Accounts {
		a.UID = uid
		accs = append(accs, a)
	}
	if err := store.ReplaceAll(st, accs); err != nil {
		t.Fatalf("写入 pool_accounts 失败: %v", err)
	}
	wins := make([]stateModelWindow, 0, len(sf.Windows))
	for _, w := range sf.Windows {
		wins = append(wins, w)
	}
	if err := store.ReplaceAll(st, wins); err != nil {
		t.Fatalf("写入 pool_windows 失败: %v", err)
	}
	if err := store.KVSet(st, poolSavedAtKey, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("写入落盘时刻失败: %v", err)
	}
}

// seedStateJSON 同 seedStateDB，但入口是旧 state.json 的原始字节（兼容性用例直接贴 JSON）。
func seedStateJSON(t *testing.T, st *store.Store, raw string) {
	t.Helper()
	var sf stateFile
	if err := json.Unmarshal([]byte(raw), &sf); err != nil {
		t.Fatalf("解析旧 state.json 失败: %v", err)
	}
	seedStateDB(t, st, sf)
}
