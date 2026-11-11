// hotcfg.go Handler 运行期可热改参数：面板保存配置后立即生效，不重启进程。
//
// 为什么是原子槽位而不是直接改 Config 语义：NewHandler 仍接收一份初始 Config
// （裸用/单测路径照旧直接给字段），运行期改动只落在这里——请求路径读槽位、
// 永不加锁，也不会读到「改了一半」的中间态。生效延迟恒为「下一条请求」：这些
// 参数都没有需要重建的装配期资源（连接池/监听器之类），读点全在请求入口之后。
//
// 约定：int64 槽位里 0 表示「未热改过」的只有 softCooldown（它与 livecfg 快照
// 有优先级关系，见 softCooldown 方法），其余槽位在 NewHandler 里用 Config 播种。
package server

import (
	"net/http"
	"sync/atomic"
	"time"
)

// hotConfig Handler 的运行期可变参数（全部原子，并发安全）。
type hotConfig struct {
	// maxPayloadBytes 入站请求体安全上限（字节）。<=0 = 回 bodylimit.go 的内置缺省。
	maxPayloadBytes atomic.Int64
	// headKeepalive 等上游首帧期间的 SSE 注释帧间隔。0 = 关闭。
	headKeepalive atomic.Int64
	// restrictionHold 11140 风控连坐的全池暂停时长。0 = 只保留「命中即停手」。
	restrictionHold atomic.Int64
	// softCooldown 软冷却基数。0 = 未热改 → getter 回退 live 快照/静态字段/缺省。
	softCooldown atomic.Int64
	// maxRotate 单请求最多换号次数（<=0 一律按 1 处理，避免死循环）。
	maxRotate atomic.Int64
	// refreshSkew token 提前刷新窗口。
	refreshSkew atomic.Int64

	// promptMode/promptText 出站提示词策略与文本。用 atomic.Pointer[string] 才能
	// 区分「未设置」与「设置为空串」——空文本是合法取值（= 不注入任何提示词）。
	promptMode atomic.Pointer[string]
	promptText atomic.Pointer[string]

	// globalEnabled global realm 路由闸（handler 侧第三道闸）。
	globalEnabled atomic.Bool
	// adminEnabled/metricsEnabled 管理面路由门禁：路由**常注册**，运行期关掉时
	// 处理器开头回 404（与「路由没注册」同语义），见 gateAdmin/gateMetrics。
	adminEnabled   atomic.Bool
	metricsEnabled atomic.Bool

	// audit 审计器指针：nil = 关闭审计。
	audit atomic.Pointer[AuditLog]

	// watchdog 上游目录指纹监控：enabled=false 时不探测（只等 kick）；interval
	// 存纳秒；kick 用来让「改开关/改间隔」立刻打断当前等待，不必等满一个周期。
	watchdogEnabled  atomic.Bool
	watchdogInterval atomic.Int64
	watchdogSeeded   atomic.Bool
	watchdogKick     chan struct{}
}

// initHot 用 Config 的初始值播种热改槽位（Setter 之后以槽位为准）。
//
// 刻意写成「在 Handler 自身上原地播种」而不是返回一个 hotConfig 值：hotConfig
// 里全是 atomic，值返回/赋值等于复制锁（go vet 的 copylocks 会报，且复制原子量
// 本身也是并发雷）。
func (h *Handler) initHot(cfg Config) {
	h.hot.watchdogKick = make(chan struct{}, 1)
	h.hot.maxPayloadBytes.Store(cfg.MaxPayloadBytes)
	h.hot.headKeepalive.Store(int64(cfg.HeadKeepalive))
	h.hot.restrictionHold.Store(int64(cfg.RestrictionHold))
	h.hot.maxRotate.Store(int64(cfg.MaxRotate))
	h.hot.refreshSkew.Store(int64(cfg.RefreshSkew))
	h.hot.setPrompt(cfg.PromptMode, cfg.PromptText)
	h.hot.globalEnabled.Store(cfg.GlobalEnabled)
	h.hot.adminEnabled.Store(cfg.AdminEnabled)
	h.hot.metricsEnabled.Store(cfg.MetricsEnabled)
	h.hot.audit.Store(cfg.Audit)
}

// ---- 读点（内部 getter） ----

// maxPayload 入站请求体上限（<=0 → bodylimit.go 的内置缺省）。
func (h *Handler) maxPayload() int64 {
	if n := h.hot.maxPayloadBytes.Load(); n > 0 {
		return n
	}
	return defaultMaxPayloadBytes
}

// headKeepalive 首帧注释帧间隔（0 = 关闭）。
func (h *Handler) headKeepalive() time.Duration {
	return time.Duration(h.hot.headKeepalive.Load())
}

// restrictionHold 11140 连坐全池暂停时长（0 = 只保留命中即停手）。
func (h *Handler) restrictionHold() time.Duration {
	return time.Duration(h.hot.restrictionHold.Load())
}

// hotSoftCooldown 热改过的软冷却基数；0 = 没热改过。
//
// 单独一个读点而不是并进 softCooldown：livecfg 快照（面板改 cooldown.soft_rate）
// 与 Setter 两条路都存在，优先级是「显式 Setter > live 快照 > 静态字段 > 缺省」，
// 这样无论面板经哪条路下发都能立刻生效，且不会把老配置覆盖成 0。
func (h *Handler) hotSoftCooldown() time.Duration {
	return time.Duration(h.hot.softCooldown.Load())
}

// maxRotate 单请求最多换号次数（<=0 → 1：一个名额都不留会让请求直接不发出）。
func (h *Handler) maxRotate() int {
	n := int(h.hot.maxRotate.Load())
	if n <= 0 {
		return 1
	}
	return n
}

// refreshSkew token 提前刷新窗口（<=0 → 10m 缺省）。
func (h *Handler) refreshSkew() time.Duration {
	if d := time.Duration(h.hot.refreshSkew.Load()); d > 0 {
		return d
	}
	return 10 * time.Minute
}

// prompt 当前的出站提示词策略与文本（一次取两个值，保证同请求内口径一致）。
func (h *Handler) prompt() (mode, text string) {
	if m := h.hot.promptMode.Load(); m != nil {
		mode = *m
	}
	if t := h.hot.promptText.Load(); t != nil {
		text = *t
	}
	if mode == "" {
		mode = "custom"
	}
	return mode, text
}

// globalEnabled 当前 global realm 路由闸。
func (h *Handler) globalEnabled() bool { return h.hot.globalEnabled.Load() }

// adminEnabled / metricsEnabled 管理面门禁读点。
func (h *Handler) adminEnabled() bool   { return h.hot.adminEnabled.Load() }
func (h *Handler) metricsEnabled() bool { return h.hot.metricsEnabled.Load() }

// auditLog 当前审计器（nil = 关闭）。
func (h *Handler) auditLog() *AuditLog { return h.hot.audit.Load() }

// watchdog 当前监控开关与间隔。
func (h *Handler) watchdog() (bool, time.Duration) {
	d := time.Duration(h.hot.watchdogInterval.Load())
	if d <= 0 {
		d = time.Hour
	}
	return h.hot.watchdogEnabled.Load(), d
}

// ---- Setter（面板保存配置后调用；并发安全，下一条请求生效） ----

// setPrompt 播种用（不加锁、不 kick，只在构造期调用）。mode 空 → custom，与现状一致。
func (h *hotConfig) setPrompt(mode, text string) {
	if mode == "" {
		mode = "custom"
	}
	m, t := mode, text
	h.promptMode.Store(&m)
	h.promptText.Store(&t)
}

// SetMaxPayloadBytes 热改入站请求体安全上限（字节）。<=0 回内置缺省（64 MiB）。
//
// 只影响之后的请求：已经读进来一半的请求体不会被这件事改变（护栏不是内容策略，
// 见 bodylimit.go 文件头）。
func (h *Handler) SetMaxPayloadBytes(n int64) {
	if h == nil {
		return
	}
	h.hot.maxPayloadBytes.Store(n)
}

// SetHeadKeepalive 热改等上游首帧期间的 SSE 注释帧间隔。0 = 关闭（旧行为）。
func (h *Handler) SetHeadKeepalive(d time.Duration) {
	if h == nil {
		return
	}
	h.hot.headKeepalive.Store(int64(d))
}

// SetRestrictionHold 热改 11140 连坐的全池暂停时长。0 = 只保留「命中即停手」。
func (h *Handler) SetRestrictionHold(d time.Duration) {
	if h == nil {
		return
	}
	h.hot.restrictionHold.Store(int64(d))
}

// SetSoftCooldown 热改软冷却基数。<=0 = 撤销热改值（回退 live 快照/静态字段）。
func (h *Handler) SetSoftCooldown(d time.Duration) {
	if h == nil {
		return
	}
	h.hot.softCooldown.Store(int64(d))
}

// SetMaxRotate 热改单请求最多换号次数（<=0 按 1 处理）。
func (h *Handler) SetMaxRotate(n int) {
	if h == nil {
		return
	}
	h.hot.maxRotate.Store(int64(n))
}

// SetRefreshSkew 热改 token 提前刷新窗口（<=0 回 10m 缺省）。
func (h *Handler) SetRefreshSkew(d time.Duration) {
	if h == nil {
		return
	}
	h.hot.refreshSkew.Store(int64(d))
}

// SetPrompt 热改出站提示词策略与文本。mode 空 → custom（与启动时归一化口径一致）。
//
// 为什么两个参数一起给：它们必须成套切换——只改 mode 不改 text（或反之）会让
// 请求中途出现「passthrough 却带着 custom 文本」的错拼状态。
func (h *Handler) SetPrompt(mode, text string) {
	if h == nil {
		return
	}
	h.hot.setPrompt(mode, text)
}

// SetGlobalEnabled 热改 global realm 路由闸（第三道闸）。
func (h *Handler) SetGlobalEnabled(on bool) {
	if h == nil {
		return
	}
	h.hot.globalEnabled.Store(on)
}

// SetAdminEnabled 热改 /admin/tasks/... 门禁。
//
// 路由始终注册，关掉时处理器回 404：这样「开启」不需要重建 mux（重建会与在途
// 请求抢 ServeMux 的读写、且旧连接可能仍指着旧 mux），而对外语义与「路由不在」
// 一致——管理面不该因为开关状态而泄露存在性。
func (h *Handler) SetAdminEnabled(on bool) {
	if h == nil {
		return
	}
	h.hot.adminEnabled.Store(on)
}

// SetMetricsEnabled 热改 GET /metrics。同 SetAdminEnabled：常注册 + 运行期 404。
func (h *Handler) SetMetricsEnabled(on bool) {
	if h == nil {
		return
	}
	h.hot.metricsEnabled.Store(on)
}

// SetAudit 热改审计器（nil = 关闭审计）。面板关掉审计后下一条 /admin 请求即不落库。
func (h *Handler) SetAudit(a *AuditLog) {
	if h == nil {
		return
	}
	h.hot.audit.Store(a)
}

// SetWatchdog 热改上游目录监控开关与间隔。
//
// WatchCatalog 是常驻循环：关掉只是「不探测」（不消耗上游请求、不刷日志），
// 打开或改间隔会立刻打断当前等待，不必等满一个旧周期。
func (h *Handler) SetWatchdog(enabled bool, interval time.Duration) {
	if h == nil {
		return
	}
	h.hot.watchdogSeeded.Store(true) // 显式设定过 → WatchCatalog 不再用入参打底
	if interval <= 0 {
		interval = time.Hour
	}
	h.hot.watchdogInterval.Store(int64(interval))
	h.hot.watchdogEnabled.Store(enabled)
	h.hot.kickWatchdog()
}

// kickWatchdog 唤醒等待中的监控循环（容量 1，重复 kick 自动合并）。
func (h *hotConfig) kickWatchdog() {
	if h.watchdogKick == nil {
		return
	}
	select {
	case h.watchdogKick <- struct{}{}:
	default:
	}
}

// gateAdmin /admin 路由的运行期门禁：关掉时按「路由不存在」返回 404。
func (h *Handler) gateAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.adminEnabled() {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// gateMetrics GET /metrics 的运行期门禁（同上）。
func (h *Handler) gateMetrics(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.metricsEnabled() {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

// SetChatLogOutput / 其它装配期 API 不在本文件；这里只收敛「面板能改的运行期参数」。
