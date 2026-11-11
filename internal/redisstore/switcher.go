// switcher.go 运行期可替换的 Store 委托器。
//
// 为什么需要它：upstash url/token 是面板可改的配置项，而 Store 在装配期被注入到
// pool / session 两处（SetStore、session.Config.Store）。若不换一层间接，面板保存
// 新的 url/token 只能靠重启进程才生效——与「配置全热生效」的目标冲突。
//
// 语义：Switcher 自身就是 Store（实现全部 6 个方法），内部用 atomic.Pointer 持有
// 当前真实实现，Swap 之后所有调用立刻打到新实现。main 装配期**总是**用 Switcher
// 包一层（哪怕当前是 Noop），热换就只剩一次 Swap。
package redisstore

import (
	"log"
	"sync/atomic"
	"time"
)

// Switcher 可热换的 Store 委托器。零值可用（等价于 Noop）。
type Switcher struct {
	cur atomic.Pointer[Store]
}

// NewSwitcher 用 initial 包一层；initial 为 nil 时退化为 Noop（纯内存）。
func NewSwitcher(initial Store) *Switcher {
	if initial == nil {
		initial = Noop{}
	}
	s := &Switcher{}
	s.cur.Store(&initial)
	return s
}

// Current 返回当前底层实现（永不为 nil）。
func (s *Switcher) Current() Store {
	if s == nil {
		return Noop{}
	}
	p := s.cur.Load()
	if p == nil || *p == nil {
		return Noop{}
	}
	return *p
}

// Swap 按新的 url/token 重建实现并替换，返回新实现（调用方据此决定是否 LoadState）。
//
// 刻意**不** Close 旧实现：Swap 发生在面板保存配置的瞬间，此刻可能还有
// fire-and-forget 的异步写在旧 Upstash 上排队（粘性镜像、状态快照）。Close 会关掉
// 底层连接，在途请求直接失败——换配置不该顺手掐掉一笔正在写的镜像。旧实现随
// 进程存活（每次 Swap 泄漏一个 redis.Client 连接池，但面板改 upstash 配置是极低频
// 操作，量级可忽略；进程退出时整个进程的连接一起释放）。
func (s *Switcher) Swap(url, token string) Store {
	if s == nil {
		return Noop{}
	}
	next := New(url, token)
	s.cur.Store(&next)
	log.Printf("[redisstore] 已切换持久化后端（%T）", next)
	return next
}

// 以下 6 个方法把 Store 接口原样转发给当前实现。

func (s *Switcher) SetBind(key, uid string, ttl time.Duration) { s.Current().SetBind(key, uid, ttl) }
func (s *Switcher) DelBind(key string)                         { s.Current().DelBind(key) }
func (s *Switcher) LoadBinds() map[string]string               { return s.Current().LoadBinds() }
func (s *Switcher) SaveState(data []byte)                      { s.Current().SaveState(data) }
func (s *Switcher) LoadState() ([]byte, bool)                  { return s.Current().LoadState() }

// Close 关停当前实现（幂等由底层保证）。Swap 出去的旧实现不再持有引用，不会被
// 这里关掉——见 Swap 的注释。
func (s *Switcher) Close() error { return s.Current().Close() }
