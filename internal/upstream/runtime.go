// runtime.go 出站配置的运行期快照 —— 面板保存配置后「立即生效、不重启进程」的落点。
//
// 为什么需要这一层：出站配置（UA / 域名 / 设备 token / 超时 / TLS 指纹 / dump）此前是
// Client 上的明文导出字段，只在启动期写一次、运行期并发读。面板要热改就必须在跑着的
// 时候写这些字段，那是裸数据竞争。现在改成「一份不可变快照 + atomic.Pointer 换指针」：
//
//   - 读侧（chat 热路径）每请求 Load 一次快照，零锁、无竞争；
//   - 写侧（面板保存 / 启动期装配）经 SetRuntime / UpdateRuntime 串行化地读-改-写；
//   - 快照里任何影响 *http.Transport 构造的项（header/idle 超时、TLS 指纹、dialer）
//     改完都要重建一个 Transport 实例 —— Transport 的字段是构造期参数，运行期就地改
//     既会被在途请求并发读写（数据竞争），生效与否也取决于连接池状态（时灵时不灵）。
//     旧实例只 CloseIdleConnections()：在途请求继续跑完，空闲连接立即回收。
package upstream

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// 运行期默认值（New() 与零值 Client 的兜底共用，避免两处口径漂移）。
const (
	// defaultRuntimeTimeout 非流式请求（billing/report/refresh/embeddings…）的总时长
	// 上限：与旧 http.Client.Timeout=120s 同口径，覆盖「拨号 + 首字节 + 读完响应」。
	defaultRuntimeTimeout = 120 * time.Second
	// defaultRuntimeIdleTimeout 聊天 SSE 流中空闲上限（活跃吐数据续命不掐）。
	// 与 cmd/server 的 config 回落值 300s 一致；<=0 时 monitorBody 直接禁用空闲监控。
	defaultRuntimeIdleTimeout = 300 * time.Second
	// defaultChatBaseCN / defaultBillingBaseCN / defaultWebBaseCN CN 侧内置默认 base。
	// 取官方桌面端的 endpoint（product.json），不是备选入口 copilot.tencent.com ——
	// 后者是官方 NetworkFailover 里的备选，主入口才是 www.workbuddy.cn。生产实测
	// （同一账号）：/v3/config、billing 汇总、chat 三条路径两个域返回一致，
	// workbuddy.cn 对海外出口也不复位握手（见 cmd/server/config.go 的 cn.chat_base 注释）。
	//
	// BillingBaseCN 同为 www.workbuddy.cn（2026-10-28，5.9.3 真机）：客户端把 CN 侧
	// **全部** billing / growth / activity 打到 workbuddy.cn，没有一条打 codebuddy.cn。
	// 旧值 codebuddy.cn 是 CodeBuddy CLI 时代的旧口径；需要退回旧口径时用
	// config cn.billing_base 显式覆盖。
	defaultChatBaseCN    = "https://www.workbuddy.cn"
	defaultBillingBaseCN = "https://www.workbuddy.cn"
	defaultWebBaseCN     = "https://www.workbuddy.cn"
)

// Runtime 出站配置快照。**值类型、无指针/切片**：复制即不可变副本，读侧可随意持有。
//
// 零值语义刻意与旧的明文字段保持一致（这样 `&Client{}` 这种测试夹具行为不变）：
// GlobalEnabled=false、各 base 为空、TLSFingerprint 空（= node）。生产默认值只在
// New() 里一次性填好，见 Runtime 的核对清单与 client.go 的 New。
type Runtime struct {
	UserAgent     string
	ClientVersion string
	CliVersion    string
	ClientName    string

	DeviceToken     string
	DeviceTokenFile string

	PassthroughIP bool

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认（按 auth 的 domain 推，见 globalChatBase）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 时即便 auth 写了 realm=global 也**不**路由到 global base（双保险，与
	// auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool

	// Timeout 非流式请求总时长上限；<=0 不限。
	Timeout time.Duration
	// HeaderTimeout 聊天 SSE 首字节前（响应头）上限；<=0 不限。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲上限；<=0 禁用空闲监控（Transport 侧的
	// IdleConnTimeout 另按构造默认回收池里的空闲连接）。
	IdleTimeout time.Duration

	// DumpDir 非空时把每次 chat 请求的出站报文与上游原始响应落盘（诊断用，见 dump.go）。
	DumpDir string
	// DumpRequestMax 落盘文件里出站请求体片段的上限（字节）；0 = 默认 256 KB。
	DumpRequestMax int

	// TLSFingerprint 出站 ClientHello 指纹档位：node（默认，复刻官方 Node/OpenSSL
	// 形态）或 go（crypto/tls 原生，排障/回滚用）。空等价 node。改它只能经
	// SetRuntime/UpdateRuntime/SetTLSFingerprint —— 那会重建 Transport 的拨号闭包；
	// 直接改快照里的字符串不会生效。
	TLSFingerprint string
}

// defaultsRuntime 零值 Client 的兜底快照：只填「与结构无关」的安全默认。
// GlobalEnabled 保持 false、base 保持空 —— 与旧版零值 Client 的行为一致
// （测试直构 &Client{} 时不会突然开始路由 global）。
func defaultsRuntime() Runtime {
	return Runtime{
		ChatBaseCN:     defaultChatBaseCN,
		BillingBaseCN:  defaultBillingBaseCN,
		Timeout:        defaultRuntimeTimeout,
		HeaderTimeout:  responseHeaderTimeout,
		IdleTimeout:    defaultRuntimeIdleTimeout,
		TLSFingerprint: TLSFingerprintNode,
	}
}

// Runtime 返回当前出站配置快照。每次调用都是一份独立副本（值语义），调用方随便改。
// chat 热路径每请求调一次；零锁（atomic.Pointer 读）。
func (c *Client) Runtime() Runtime {
	if c == nil {
		return defaultsRuntime()
	}
	if rt := c.runtime.Load(); rt != nil {
		return *rt
	}
	return defaultsRuntime()
}

// SetRuntime 整份替换出站配置快照，并重建 Transport 使连接层参数（响应头/idle
// 超时、拨号器、TLS 指纹）立即生效。
//
// TLSFingerprint 归一化：空 → node；非法值 → **保留旧值**并记日志（一次手滑的配置
// 不该把出站指纹变成未知档位，那会改掉全部请求的握手形态）。
func (c *Client) SetRuntime(rt Runtime) {
	if c == nil {
		return
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	c.setRuntimeLocked(rt)
}

// UpdateRuntime 读-改-写：在 mutex 内取快照、交给 fn 改、再整份发布 + 重建 Transport。
//
// 为什么不是「调用方取快照、改完再 SetRuntime」：两次调用之间会漏掉并发的另一次
// 更新（面板保存与启动期装配同时发生就会丢字段）。这里读改写同一把锁内完成。
func (c *Client) UpdateRuntime(fn func(*Runtime)) {
	if c == nil || fn == nil {
		return
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	rt := c.Runtime()
	fn(&rt)
	c.setRuntimeLocked(rt)
}

// setRuntimeLocked 归一化 → 换快照 → 重建 Transport。调用方必须持有 runtimeMu。
func (c *Client) setRuntimeLocked(rt Runtime) {
	if norm, err := validateTLSFingerprint(rt.TLSFingerprint); err == nil {
		rt.TLSFingerprint = norm
	} else {
		old := c.Runtime().TLSFingerprint
		log.Printf("WARN: [upstream] 忽略非法 tls_fingerprint：%v（保留旧值 %q）", err, old)
		rt.TLSFingerprint = old
	}
	// CN 三个 base 为空 = 保留内置默认：空的 base 会让出站 URL 变成相对路径，
	// 整条链路静默打空（老配置没有 cn 段时就会命中这条）。这里就地补默认值，
	// 读侧（chatBase/billingBase）因此永远拿到可用 base。
	if rt.ChatBaseCN == "" {
		rt.ChatBaseCN = defaultChatBaseCN
	}
	if rt.BillingBaseCN == "" {
		rt.BillingBaseCN = defaultBillingBaseCN
	}
	c.runtime.Store(&rt)
	c.rebuildTransportLocked()
}

// BaseTransport 返回当前出站 Transport，供 proxypool 当克隆基底（它在上面挂代理
// 隧道）。返回的是内部实例，调用方**不得**改它的字段 —— 要改配置请走
// SetRuntime/UpdateRuntime，那会换一个新实例。
func (c *Client) BaseTransport() *http.Transport {
	if c == nil {
		return nil
	}
	return c.transportNow()
}

// HeaderTimeout 聊天 SSE 首字节前的上限（<=0 不限）。
func (c *Client) HeaderTimeout() time.Duration { return c.Runtime().HeaderTimeout }

// IdleTimeout 聊天 SSE 流中空闲上限（<=0 禁用）。
func (c *Client) IdleTimeout() time.Duration { return c.Runtime().IdleTimeout }

// Timeout 非流式请求总时长上限（<=0 不限）。
func (c *Client) Timeout() time.Duration { return c.Runtime().Timeout }

// UserAgent 出站 User-Agent 显式覆盖（空 = 默认官方三段式）。
func (c *Client) UserAgent() string { return c.Runtime().UserAgent }

// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 WorkBuddy/<ver> + X-IDE-Version）。
func (c *Client) ClientVersion() string { return c.Runtime().ClientVersion }

// CliVersion 出站 UA 中 CLI/<ver> 段版本。
func (c *Client) CliVersion() string { return c.Runtime().CliVersion }

// ClientName 用量归属头取值（X-Product / X-IDE-*）。
func (c *Client) ClientName() string { return c.Runtime().ClientName }

// DeviceToken 设备风控 token 全局兜底来源（config upstream.device_token）。
func (c *Client) DeviceToken() string { return c.Runtime().DeviceToken }

// DeviceTokenFile 设备 token 文件路径兜底。
func (c *Client) DeviceTokenFile() string { return c.Runtime().DeviceTokenFile }

// PassthroughIP 是否透传客户端 IP 给上游。
func (c *Client) PassthroughIP() bool { return c.Runtime().PassthroughIP }

// ChatBaseCN CN chat base。
func (c *Client) ChatBaseCN() string { return c.Runtime().ChatBaseCN }

// BillingBaseCN CN billing base。
func (c *Client) BillingBaseCN() string { return c.Runtime().BillingBaseCN }

// WebBaseCN CN 官网域 base（空 = 回落 https://www.workbuddy.cn）。
func (c *Client) WebBaseCN() string { return c.Runtime().WebBaseCN }

// ChatBaseGlobal 显式配置的 global chat base（空 = 按 domain 推默认）。
func (c *Client) ChatBaseGlobal() string { return c.Runtime().ChatBaseGlobal }

// BillingBaseGlobal 显式配置的 global billing base。
func (c *Client) BillingBaseGlobal() string { return c.Runtime().BillingBaseGlobal }

// GlobalEnabled global realm 路由总闸（config global.enabled）。
func (c *Client) GlobalEnabled() bool { return c.Runtime().GlobalEnabled }

// TLSFingerprint 当前出站 ClientHello 指纹档位（node / go）。
// 改它请用 SetTLSFingerprint / UpdateRuntime —— 两者都会重建 Transport 的拨号闭包。
func (c *Client) TLSFingerprint() string { return c.Runtime().TLSFingerprint }

// DumpDir 报文落盘目录（空 = 不落盘）。
func (c *Client) DumpDir() string { return c.Runtime().DumpDir }

// DumpRequestMax 落盘请求体片段上限（0 = 默认 256 KB）。
func (c *Client) DumpRequestMax() int { return c.Runtime().DumpRequestMax }

// transportNow 取当前 Transport；还没建过（零值 Client / 测试直构）时按快照懒建一次。
//
// 懒建的意义：`&Client{...}` 这种测试夹具从不调 New()，但可能被要求给出基础
// Transport（proxypool 克隆、连接复用断言）；给一条按快照参数构造的实例，既不用
// 给每个夹具补初始化，也不会让它们撞上 nil Transport。
func (c *Client) transportNow() *http.Transport {
	if tr := c.transport.Load(); tr != nil {
		return tr
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	if tr := c.transport.Load(); tr != nil { // double check：等锁期间可能已被别人建好
		return tr
	}
	return c.rebuildTransportLocked()
}

// rebuildTransportLocked 按当前快照重建 Transport 并换指针，旧实例只清空闲连接。
//
// 为什么整份重建而不是改字段：ResponseHeaderTimeout / IdleConnTimeout / DialContext /
// DialTLSContext 都是 Transport 的**构造期**参数。运行期就地写这些字段，一是在途请求
// 会并发读同一个 struct（数据竞争，go test -race 直接报），二是连接池已缓存的连接
// 不会按新口径重建（改了等于没改，或者半生效）。整份换实例 + 旧实例 CloseIdleConnections
// 是唯一既能立即生效、又不打断在途流的做法。
func (c *Client) rebuildTransportLocked() *http.Transport {
	rt := c.Runtime()
	d := c.dialer
	if d == nil {
		// 记下这条拨号器：否则每次重建都新建一个 *net.Dialer，参数指纹永远不相等，
		// 「连接层没变就不重建」的判断会一直失效。
		d = newDialer()
		c.dialer = d
	}
	spec := transportSpec{
		headerTimeout: rt.HeaderTimeout,
		idleTimeout:   rt.IdleTimeout,
		fingerprint:   rt.TLSFingerprint,
		dialer:        d,
	}
	// 连接层参数没变就别动连接池：改 UA / 域名 / token / dump 目录这类配置时，
	// Transport 一个字段都不需要变，重建只会白白丢掉所有空闲连接。
	if cur := c.transport.Load(); cur != nil {
		if old := c.tspec.Load(); old != nil && *old == spec {
			return cur
		}
	}
	tr := newTransport() // 基线：真正禁 h2 / 握手与建连超时 / 池容量
	// ResponseHeaderTimeout：<=0 表示不限（0 = 不设响应头上限）。
	tr.ResponseHeaderTimeout = rt.HeaderTimeout
	// IdleConnTimeout：<=0 回落构造默认（池里的空闲连接总得有个回收期限）。
	tr.IdleConnTimeout = idleConnTimeout
	if rt.IdleTimeout > 0 {
		tr.IdleConnTimeout = rt.IdleTimeout
	}
	tr.DialContext = d.DialContext
	// DialTLSContext 接管整条 TLS 拨号（指纹复刻 + 请求头重排），见 newTLSDialer 注释。
	tr.DialTLSContext = newTLSDialer(d, nil, rt.TLSFingerprint)
	old := c.transport.Swap(tr)
	c.tspec.Store(&spec)
	if old != nil {
		old.CloseIdleConnections()
	}
	return tr
}

// transportSpec Transport 的构造参数指纹：任何一项变了都必须换实例（*http.Transport
// 的这些字段是构造期参数，运行期就地改既数据竞争、也对已有连接不生效）。
type transportSpec struct {
	headerTimeout time.Duration
	idleTimeout   time.Duration
	fingerprint   string
	dialer        *net.Dialer
}

// dynTransport 出站传输层门面：把「每请求现读配置快照」与「Transport 可热替换」
// 两件事隔离开，HTTP / ChatHTTP 两个 *http.Client 的 Transport 恒为它。
//
// 为什么总超时不放在 http.Client.Timeout 里：那个字段只能启动期设一次，热改就是
// 数据竞争；放进 RoundTrip 的 ctx 则天然每请求重新读快照。代价是要自己管 cancel
// —— 见 timeoutBody。
type dynTransport struct {
	c *Client
	// noTotalTimeout 聊天 SSE 流专用（ChatHTTP 用的那一份）：**不加**总时长 deadline。
	// 长回答可能跑几分钟，总时长上限会把它拦腰砍断；首字节由 Transport 的
	// ResponseHeaderTimeout 管、流中空闲由 IdleTimeout 监控（见 idle.go）。
	noTotalTimeout bool
}

// RoundTrip 实现 http.RoundTripper。
func (d dynTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return d.roundTrip(d.c.transportNow(), req)
}

// roundTrip 在指定底层传输上发请求，并按快照补总时长 deadline。
func (d dynTransport) roundTrip(rt http.RoundTripper, req *http.Request) (*http.Response, error) {
	if rt == nil {
		return nil, errors.New("upstream: 没有可用的 Transport")
	}
	if d.noTotalTimeout {
		return rt.RoundTrip(req)
	}
	total := d.c.Runtime().Timeout
	if total <= 0 {
		return rt.RoundTrip(req)
	}
	// 总时长从建连前开始计时，覆盖「拨号 + 首字节 + 读完整响应」，与旧
	// http.Client.Timeout 同口径。必须基于 req.Clone 生成新 req：调用方可能把同一个
	// *http.Request 交给多处使用，就地 WithContext 会改到别人的请求对象。
	ctx, cancel := context.WithTimeout(req.Context(), total)
	resp, err := rt.RoundTrip(req.Clone(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.Body == nil {
		cancel()
		return resp, nil
	}
	// cancel 不能此刻调用：会立刻取消 ctx，body 读到一半就断。所有权移交给 body 的
	// Close（net/http 自己的 Timeout 实现也是这个思路）。
	resp.Body = &timeoutBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// CloseIdleConnections 让 roundTripCloseIdle 在传输层失败后仍能清掉空闲池。
func (d dynTransport) CloseIdleConnections() {
	if tr := d.c.transportNow(); tr != nil {
		tr.CloseIdleConnections()
	}
}

// Wrap 供 proxypool 在「换掉传输层走代理」时把总时长补回来：它会把 Transport 换成
// 一个按节点建的 *http.Transport，若不外包一层，这条路径就会静默失去总上限（旧实现
// 靠 http.Client.Timeout，切代理时随 client 复制带走了）。
func (d dynTransport) Wrap(inner http.RoundTripper) http.RoundTripper {
	return wrappedTransport{outer: d, inner: inner}
}

// wrappedTransport 给任意底层传输套上 dynTransport 的「总时长 + 每请求读快照」语义。
type wrappedTransport struct {
	outer dynTransport
	inner http.RoundTripper
}

func (w wrappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return w.outer.roundTrip(w.inner, req)
}

// CloseIdleConnections 转发给底层（proxypool 的失败清池用得着）。
func (w wrappedTransport) CloseIdleConnections() {
	if ci, ok := w.inner.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}

// timeoutBody 在 body 关闭时释放总时长 ctx 的定时器：不这么做，每次请求都会留一个
// 到点才回收的 timer，长跑进程会攒出可观的内存与 goroutine 抖动。
type timeoutBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *timeoutBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}
