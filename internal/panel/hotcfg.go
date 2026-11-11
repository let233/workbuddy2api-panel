// hotcfg.go 面板运行期可热改参数：面板保存配置后立即生效，不重启进程。
//
// 为什么不直接改 p.cfg：cfg 是装配期快照，就地写会与并发请求读同一块内存
// （作者要求原子化消除数据竞争），也分不清「配置里这一项到底是 0 还是没配」。
// 因此槽位带 set 标志：**设过**才覆盖静态字段，没设过一律回退 cfg——这样裸建
// Panel 的老用法（含单测里直接改 p.cfg.AuthDir）语义不变。
package panel

import (
	"sync/atomic"
)

// hotString 可热改字符串槽位（带 set 标志）。atomic.Pointer[string] 才能表达
// 「没设置过」与「设置为空串」的区别——空串是合法取值（如清空账号目录）。
type hotString struct{ v atomic.Pointer[string] }

// Store 写入热改值（此后以它为准）。
func (h *hotString) Store(s string) { h.v.Store(&s) }

// Load 读取热改值；ok=false 表示没设置过，调用方回退静态字段。
func (h *hotString) Load() (string, bool) {
	p := h.v.Load()
	if p == nil {
		return "", false
	}
	return *p, true
}

// hotInt 可热改整数槽位（带 set 标志，理由同 hotString：0 是合法配置值）。
type hotInt struct {
	v   atomic.Int64
	set atomic.Bool
}

// Store 写入热改值。
func (h *hotInt) Store(n int) {
	h.v.Store(int64(n))
	h.set.Store(true)
}

// Load 读取热改值；ok=false 表示没设置过，调用方回退静态字段。
func (h *hotInt) Load() (int, bool) {
	if !h.set.Load() {
		return 0, false
	}
	return int(h.v.Load()), true
}

// stickyCountFn / modelFallbackFn 是「延迟取值」的回调槽位：
// 这两个配置项本身是函数（main 装配期注入，内部指向 upstream/scheduler 的活数据），
// 面板热改后需要换成新的函数对象——用 atomic.Pointer 指向一个小结构体，
// 请求路径读一次即可，既不复制活数据也不加锁。
type stickyCountFn struct{ fn func() int }
type modelFallbackFn struct{ fn func() map[string]string }

// ---- Setter（面板保存配置后调用；并发安全，下一条请求生效） ----

// SetAuthDir 热改账号目录。
//
// 为什么必须热改：重载/导入/登录/密钥四处都按这个目录找账号文件，账号目录搬家
// 后若还要重启才能生效，「保存配置」就是半生效，运维会以为配错了。
func (p *Panel) SetAuthDir(dir string) { p.authDirHot.Store(dir) }

// authDir 当前生效的账号目录（Setter > 静态字段）。
func (p *Panel) authDir() string {
	if v, ok := p.authDirHot.Load(); ok {
		return v
	}
	return p.cfg.AuthDir
}

// SetPollIntervalSec 热改总览里展示的账号状态轮询间隔（秒）。
func (p *Panel) SetPollIntervalSec(sec int) { p.pollIntervalHot.Store(sec) }

// pollIntervalSec 当前生效的轮询间隔（秒）。
func (p *Panel) pollIntervalSec() int {
	if v, ok := p.pollIntervalHot.Load(); ok {
		return v
	}
	return p.cfg.PollIntervalSec
}

// SetPackageDetailLimit 热改套餐「明细」拉取的条数上限（0 = 不限/未设置）。
//
// 面板侧目前不落库也不出报表，这个值只影响前端展示口径（前端从配置接口读），
// 这里给的是「保存后能立刻读回来」的那份权威值，避免前端还要等配置文件重读。
func (p *Panel) SetPackageDetailLimit(n int) { p.packageDetailLimitHot.Store(n) }

// PackageDetailLimit 当前生效的套餐明细条数上限；ok=false 表示没热改过。
func (p *Panel) PackageDetailLimit() (int, bool) { return p.packageDetailLimitHot.Load() }

// SetStickyCount 热改「进行中粘性会话数」的取值回调（nil = 回退静态字段）。
func (p *Panel) SetStickyCount(fn func() int) {
	if fn == nil {
		p.stickyCountHot.Store(nil)
		return
	}
	p.stickyCountHot.Store(&stickyCountFn{fn: fn})
}

// stickyCount 当前粘性会话数（热改回调 > 静态字段 > 0）。
func (p *Panel) stickyCount() int {
	if f := p.stickyCountHot.Load(); f != nil && f.fn != nil {
		return f.fn()
	}
	if p.cfg.StickyCount != nil {
		return p.cfg.StickyCount()
	}
	return 0
}

// SetModelFallback 热改模型兜底映射回调（nil = 回退静态字段）。
func (p *Panel) SetModelFallback(fn func() map[string]string) {
	if fn == nil {
		p.modelFallbackHot.Store(nil)
		return
	}
	p.modelFallbackHot.Store(&modelFallbackFn{fn: fn})
}

// modelFallback 当前兜底映射（热改回调 > 静态字段 > nil）。
func (p *Panel) modelFallback() map[string]string {
	if f := p.modelFallbackHot.Load(); f != nil && f.fn != nil {
		return f.fn()
	}
	if p.cfg.ModelFallback != nil {
		return p.cfg.ModelFallback()
	}
	return nil
}
