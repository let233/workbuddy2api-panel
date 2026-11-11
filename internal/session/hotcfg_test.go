// hotcfg_test.go 运行期热配置：开关 / TTL / GC 间隔 / 镜像后端。
//
// 这四条是「面板保存配置后立刻生效」在会话粘性上的全部出口，任何一条退回
// 启动期快照都会表现为「改了没反应，得重启」。
package session

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/redisstore"
)

// TestSetEnabledDisablesResolve 关掉粘性后 Resolve 一律 false 且清空绑定；
// 关着时 Bind 也不能把绑定偷偷写回来（否则重新启用会带一批停用期的旧绑定复活）。
func TestSetEnabledDisablesResolve(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	if u, ok := r.Resolve("c1"); !ok || u == "" {
		t.Fatalf("前置：启用时应能分配，got %q ok=%v", u, ok)
	}
	if !r.Enabled() {
		t.Fatal("New 之后应默认启用（引入开关前行为不变）")
	}

	r.SetEnabled(false)
	if r.Enabled() {
		t.Fatal("SetEnabled(false) 后 Enabled() 应为 false")
	}
	if _, ok := r.Resolve("c1"); ok {
		t.Fatal("粘性关掉后 Resolve 必须返回 \"\", false")
	}
	if _, ok := r.ResolveForModel("c1", "glm-5.3"); ok {
		t.Fatal("粘性关掉后 ResolveForModel 也必须返回 false")
	}
	if n := r.Count(); n != 0 {
		t.Fatalf("关掉时应清空 entries，count=%d", n)
	}
	r.Bind("c2", "a1")
	if n := r.Count(); n != 0 {
		t.Fatalf("关着时 Bind 不该建绑定，count=%d", n)
	}
	if r.Unbind("c2") {
		t.Fatal("关着时 Unbind 应返回 false（本来就没有绑定）")
	}

	// 重新启用：从零开始分配（不是把停用前的绑定复活）。
	r.SetEnabled(true)
	u, ok := r.Resolve("c1")
	if !ok || u == "" {
		t.Fatalf("重新启用后应能分配，got %q ok=%v", u, ok)
	}
	if r.Count() != 1 {
		t.Fatalf("重新启用后 count=%d want 1", r.Count())
	}
}

// TestReconfigureTTLHot TTL 热改后立刻生效：gcOnce（后台/惰性过期共用同一读点）
// 必须按新 TTL 判过期，而不是沿用 New 时的旧值。
func TestReconfigureTTLHot(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Hour)
	r.Bind("c1", "a1")
	r.Reconfigure(10*time.Millisecond, time.Hour) // TTL 改短
	time.Sleep(20 * time.Millisecond)
	if removed := r.gcOnce(time.Now()); removed != 1 {
		t.Fatalf("TTL 热改未生效：gc 清掉 %d 条，want 1", removed)
	}
	if r.Count() != 0 {
		t.Fatalf("count=%d want 0", r.Count())
	}

	// 反向：TTL 改回长值 → 刚过 20ms 的绑定不该再被判过期。
	r.Bind("c2", "a1")
	r.Reconfigure(time.Hour, time.Hour)
	time.Sleep(20 * time.Millisecond)
	if removed := r.gcOnce(time.Now()); removed != 0 {
		t.Fatalf("TTL 改长后不该清任何绑定，清了 %d 条", removed)
	}
	if _, ok := r.boundUID("c2"); !ok {
		t.Fatal("TTL 改长后 c2 绑定应还在")
	}
}

// TestReconfigureGCIntervalImmediate GC 间隔热改必须**立刻**重设 timer：
// 只写原子量不唤醒的话，新间隔要等旧 ticker 到期（这里旧值是 1 小时）才生效。
func TestReconfigureGCIntervalImmediate(t *testing.T) {
	r := New(Config{
		TTL:        20 * time.Millisecond,
		GCInterval: time.Hour, // 旧间隔：不热改就要等 1 小时
		Available:  func() []string { return []string{"a1"} },
	})
	r.StartGC()
	defer r.StopGC()
	r.Bind("c1", "a1")

	r.Reconfigure(20*time.Millisecond, 10*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.Count() == 0 {
			return // GC 已按新间隔跑起来
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("GC 间隔热改未立刻生效：等了 5s 绑定仍在（count=%d）", r.Count())
}

// TestSetStoreSwapsBackend 运行期换镜像后端：换完之后读写都打到新 store，
// 旧 store 不再收到镜像。
func TestSetStoreSwapsBackend(t *testing.T) {
	st1, st2 := newCountingStore(), newCountingStore()
	r := routerWith(st1, []string{"a1", "a2"}, time.Minute)
	r.Resolve("c1")

	st1.mu.Lock()
	n1 := st1.setBinds
	st1.mu.Unlock()
	if n1 == 0 {
		t.Fatal("前置：解析应已镜像到初始 store")
	}

	r.SetStore(st2)
	if r.Store() != redisstore.Store(st2) {
		t.Fatalf("Store() 应返回新后端，got %T", r.Store())
	}
	r.Resolve("c2")

	st2.mu.Lock()
	n2 := st2.setBinds
	st2.mu.Unlock()
	if n2 == 0 {
		t.Fatal("换后端后的写入没有打到新 store")
	}
	st1.mu.Lock()
	after := st1.setBinds
	st1.mu.Unlock()
	if after != n1 {
		t.Fatalf("旧 store 不该再收到镜像：%d → %d", n1, after)
	}

	// nil 视为纯内存（Noop），不 panic。
	r2 := routerWith(nil, []string{"a1"}, time.Minute)
	r2.SetStore(nil)
	if _, ok := r2.Store().(redisstore.Noop); !ok {
		t.Fatalf("SetStore(nil) 应回落 Noop，got %T", r2.Store())
	}
}
