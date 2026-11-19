package gatewaykeys

import (
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// 「有没有 Key」必须以库为准。内存清单可能只是因为读库失败才是空的，按内存判断
// 会在这种时刻再播种一把新 Key——旧 Key 全部 401、还多出一把没人知道的 Key。
func TestEnsureSystemKeyTrustsStoreOverEmptyMemory(t *testing.T) {
	db := storetest.Open(t)
	seed := NewManager(db)
	defer seed.Close()
	if _, err := seed.Create(Key{Name: "已有"}); err != nil {
		t.Fatal(err)
	}

	// 模拟「读库失败后内存为空」的实例：同一个库，内存清单是空的。
	m := &Manager{
		keys:      map[string]*Key{},
		templates: map[string]*Template{},
		db:        db,
		dirty:     map[string]struct{}{},
	}
	if len(m.List()) != 0 {
		t.Fatal("前置：内存清单应为空")
	}
	if _, created, err := m.EnsureSystemKey(); err != nil || created {
		t.Fatalf("库里有 Key 时不得播种: created=%v err=%v", created, err)
	}
	if n, err := store.Count[keyRow](db); err != nil || n != 1 {
		t.Fatalf("库里 Key 数量=%d err=%v，want 1", n, err)
	}
}

// 读库失败必须能被上层看见（LoadError/Reload），而不是静默变成空清单。
func TestLoadErrorSurfacesStoreFailure(t *testing.T) {
	db := storetest.Open(t)
	m := NewManager(db)
	defer m.Close()
	if err := m.LoadError(); err != nil {
		t.Fatalf("正常库不该有 LoadError: %v", err)
	}
	_ = db.Close()

	if err := m.Reload(); err == nil {
		t.Fatal("库关闭后 Reload 必须报错")
	}
	if err := m.LoadError(); err == nil {
		t.Fatal("库关闭后 LoadError 必须非 nil")
	}
	// 读不到库时播种必须失败，绝不能凭空造一把新 Key。
	if _, created, err := m.EnsureSystemKey(); err == nil || created {
		t.Fatalf("读库失败时不得播种: created=%v err=%v", created, err)
	}
}
