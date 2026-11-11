// switcher_test.go 热换持久化后端：委托、替换、零值兜底。
package redisstore

import (
	"testing"
	"time"
)

// fakeStore 纯内存假实现（不联网），记录调用次数。
type fakeStore struct {
	setBinds int
	delBinds int
	saves    int
	binds    map[string]string
	state    []byte
}

func newFakeStore() *fakeStore { return &fakeStore{binds: map[string]string{}} }

func (f *fakeStore) SetBind(key, uid string, _ time.Duration) {
	f.setBinds++
	f.binds[key] = uid
}
func (f *fakeStore) DelBind(key string) {
	f.delBinds++
	delete(f.binds, key)
}
func (f *fakeStore) LoadBinds() map[string]string {
	out := make(map[string]string, len(f.binds))
	for k, v := range f.binds {
		out[k] = v
	}
	return out
}
func (f *fakeStore) SaveState(data []byte) {
	f.saves++
	f.state = append([]byte(nil), data...)
}
func (f *fakeStore) LoadState() ([]byte, bool) {
	if f.state == nil {
		return nil, false
	}
	return f.state, true
}
func (f *fakeStore) Close() error { return nil }

// TestSwitcherDelegates 未替换时所有调用原样转发给 initial。
func TestSwitcherDelegates(t *testing.T) {
	f := newFakeStore()
	s := NewSwitcher(f)
	if s.Current() != Store(f) {
		t.Fatalf("Current() 应返回 initial，got %T", s.Current())
	}
	s.SetBind("c1", "a1", time.Minute)
	s.SaveState([]byte("{}"))
	if f.setBinds != 1 || f.saves != 1 || f.binds["c1"] != "a1" {
		t.Fatalf("委托失败: setBinds=%d saves=%d binds=%v", f.setBinds, f.saves, f.binds)
	}
	if binds := s.LoadBinds(); binds["c1"] != "a1" {
		t.Fatalf("LoadBinds 委托失败: %v", binds)
	}
	s.DelBind("c1")
	if f.delBinds != 1 {
		t.Fatalf("DelBind 委托失败: %d", f.delBinds)
	}
	if _, ok := s.LoadState(); !ok {
		t.Fatal("LoadState 委托失败")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 委托失败: %v", err)
	}
}

// TestSwitcherSwapReplacesBackend Swap 之后所有调用打到新实现；url 为空时退化为
// Noop（纯内存），且不会碰到旧实现（旧实现不被 Close：在途写还要跑完）。
func TestSwitcherSwapReplacesBackend(t *testing.T) {
	f := newFakeStore()
	s := NewSwitcher(f)
	s.SetBind("c1", "a1", time.Minute)

	next := s.Swap("", "")
	if _, ok := next.(Noop); !ok {
		t.Fatalf("url 为空应退化为 Noop，got %T", next)
	}
	if _, ok := s.Current().(Noop); !ok {
		t.Fatalf("Swap 后 Current() 应为 Noop，got %T", s.Current())
	}
	// Swap 之后写不再落到旧实现。
	s.SetBind("c2", "a2", time.Minute)
	if f.setBinds != 1 {
		t.Fatalf("Swap 后旧实现不该再收到写：setBinds=%d want 1", f.setBinds)
	}
	if binds := s.LoadBinds(); len(binds) != 0 {
		t.Fatalf("Noop.LoadBinds 应为空，got %v", binds)
	}
}

// TestSwitcherZeroValue 零值与 nil initial 都要安全（main 装配前/测试里可能直接用）。
func TestSwitcherZeroValue(t *testing.T) {
	var zero Switcher
	if _, ok := zero.Current().(Noop); !ok {
		t.Fatalf("零值 Switcher 应回落 Noop，got %T", zero.Current())
	}
	if _, ok := NewSwitcher(nil).Current().(Noop); !ok {
		t.Fatal("NewSwitcher(nil) 应回落 Noop")
	}
	var nilSwitcher *Switcher
	if _, ok := nilSwitcher.Current().(Noop); !ok {
		t.Fatal("nil *Switcher 应回落 Noop（不 panic）")
	}
	nilSwitcher.SetBind("k", "u", time.Minute) // 不 panic
	nilSwitcher.DelBind("k")
	nilSwitcher.SaveState(nil)
	if _, ok := nilSwitcher.LoadBinds()["k"]; ok {
		t.Fatal("nil Switcher 的 LoadBinds 应为空")
	}
	if _, ok := nilSwitcher.LoadState(); ok {
		t.Fatal("nil Switcher 的 LoadState 应为 false")
	}
	if err := nilSwitcher.Close(); err != nil {
		t.Fatalf("nil Switcher Close 应无错: %v", err)
	}
}
