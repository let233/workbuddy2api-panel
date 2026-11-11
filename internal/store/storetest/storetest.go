// Package storetest 测试用的数据库工厂。
//
// 面板的数据集都落在数据库里，各业务包的测试同样需要一个库。这里统一开一个
// 独立的临时 SQLite 文件（与线上默认驱动一致，且不需要外部服务），测试结束自动关闭。
package storetest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// Open 在 t.TempDir() 下开一个 SQLite 库（测试结束自动关闭）。
func Open(t testing.TB) *store.Store {
	t.Helper()
	return OpenDir(t, t.TempDir())
}

// OpenDir 在指定目录下开一个 SQLite 库（需要与旧文件同目录的导入测试用）。
func OpenDir(t testing.TB, dir string) *store.Store {
	t.Helper()
	// 目录可能还不存在（upgEnv 里先写 state.json 再开库的路径就是这样）。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("创建测试目录失败: %v", err)
	}
	st, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
