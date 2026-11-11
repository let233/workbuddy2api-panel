// hotcfg.go 会话粘性路由的运行期热配置入口：开关 / TTL / GC 间隔 / 镜像后端。
//
// 为什么单独成文件：session.go 的主干是请求快路径（每个 chat 请求都要过），热改
// 入口只在面板保存配置时调用一次。分开放，快路径里只剩「读一个原子量」的成本。
package session

import (
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/redisstore"
)

// 热改字段的兜底默认值（与 New 的回落口径一致）：Reconfigure 收到 <=0 视为
// 「未配置」→ 回落默认，而不是把 TTL 改成 0（全部绑定当场过期）或把 GC 间隔
// 改成 0（timer 立刻反复触发 = 忙等）。
const (
	defaultTTL        = 30 * time.Minute
	defaultGCInterval = 5 * time.Minute
)

// SetEnabled 热改粘性开关（session_sticky.enabled）。
//
// 关掉时立即清空内存绑定：停用期间不该还留着「谁绑了谁」——否则重新启用的一瞬间
// 旧绑定集体复活，把停用前已经冷却/换号的会话又钉回去。Redis 镜像不主动删：那会是
// 一次全量 DEL 风暴，而镜像的用途只是「进程重启后恢复」，与运行期开关无关。
func (r *Router) SetEnabled(on bool) {
	if r == nil {
		return
	}
	r.enabled.Store(on)
	if on {
		return
	}
	r.mu.Lock()
	r.entries = map[string]entry{}
	r.mu.Unlock()
}

// Enabled 当前粘性开关。
func (r *Router) Enabled() bool {
	if r == nil {
		return false
	}
	return r.enabled.Load()
}

// Reconfigure 热改 TTL 与后台 GC 间隔（session_sticky.ttl / gc_interval）。
//
// 只写原子量 + poke rearm 是不够的一半：GC goroutine 里的 timer 是「上一次设定的
// 间隔」，不唤醒它就得等旧的到期（最长一整个旧间隔，默认 5 分钟）新值才生效，
// 面板上看起来就是「保存了没反应」。rearm 让运行中的循环立刻重设 timer。
func (r *Router) Reconfigure(ttl, gcInterval time.Duration) {
	if r == nil {
		return
	}
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if gcInterval <= 0 {
		gcInterval = defaultGCInterval
	}
	r.ttl.Store(int64(ttl))
	r.gcInterval.Store(int64(gcInterval))
	poke(r.rearm)
}

// SetStore 运行期换镜像后端（upstash url/token 热改后由 main 调用）。nil 视为
// 纯内存（Noop），与 New 的 nil 语义一致。
func (r *Router) SetStore(s redisstore.Store) {
	if r == nil {
		return
	}
	if s == nil {
		s = redisstore.Noop{}
	}
	r.store.Store(&s)
}

// Store 当前镜像后端（永不为 nil）。
func (r *Router) Store() redisstore.Store {
	if r == nil {
		return redisstore.Noop{}
	}
	return r.storeRef()
}

// ttlDur 当前 TTL（零值 Router 兜底默认）。
func (r *Router) ttlDur() time.Duration {
	if v := r.ttl.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultTTL
}

// gcEvery 当前 GC 间隔（零值 Router 兜底默认：否则 timer 以 0 间隔立刻反复触发）。
func (r *Router) gcEvery() time.Duration {
	if v := r.gcInterval.Load(); v > 0 {
		return time.Duration(v)
	}
	return defaultGCInterval
}

// storeRef 当前镜像后端（未 SetStore 的零值 Router 走 Noop，不 panic）。
func (r *Router) storeRef() redisstore.Store {
	p := r.store.Load()
	if p == nil || *p == nil {
		return redisstore.Noop{}
	}
	return *p
}

// poke 非阻塞唤醒（已有待处理信号则忽略）：GC 循环只有一个消费者，
// 信号合并是安全语义（多改几次配置只需唤醒一次重设 timer）。
func poke(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}
