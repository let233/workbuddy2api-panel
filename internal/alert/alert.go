// Package alert 账号池可用性阈值告警。Enabled=false 时 Run 直接返回。
package alert

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultInterval     = 30 * time.Second
	defaultTimeout      = 5 * time.Second
	defaultForTicks     = 2
	defaultClearTicks   = 2
	defaultStartupGrace = 30 * time.Second
	defaultRecover      = 1
	defaultServiceName  = "workbuddy2api"
)

type Health struct {
	Total, Healthy, Cooling, Disabled, Breaker, Degraded, InFlight int
}

type Source interface {
	Health(realm string) Health
	WAFActive() bool
}

type Config struct {
	Enabled          bool
	WebhookURL       string
	Secret           string
	Interval         time.Duration
	Timeout          time.Duration
	StartupGrace     time.Duration
	MinHealthyCN     int
	MinHealthyGlobal int
	RecoverHealthy   int
	BreakerThreshold int
	ForTicks         int
	ClearTicks       int
	SendResolve      bool
	ServiceName      string
}

func (c *Config) normalize() {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultTimeout
	}
	if c.StartupGrace <= 0 {
		c.StartupGrace = defaultStartupGrace
	}
	if c.ForTicks <= 0 {
		c.ForTicks = defaultForTicks
	}
	if c.ClearTicks <= 0 {
		c.ClearTicks = defaultClearTicks
	}
	if c.RecoverHealthy <= 0 {
		c.RecoverHealthy = defaultRecover
	}
	if c.ServiceName == "" {
		c.ServiceName = defaultServiceName
	}
}

type ruleState int

const (
	stateOK ruleState = iota
	statePending
	stateFiring
)

type rule struct {
	id, severity, realm string
	sample              func(Source) (value float64, bad, recovered bool, threshold float64, msg string)
	state               ruleState
	badTicks            int
	clearCount          int
	since               time.Time
}

type event int

const (
	eventNone event = iota
	eventAlert
	eventResolve
)

func (r *rule) step(now time.Time, src Source, forTicks, clearTicks int) (event, float64, float64, string) {
	v, bad, recovered, th, msg := r.sample(src)
	switch r.state {
	case stateOK:
		if !bad {
			return eventNone, 0, 0, ""
		}
		r.state = statePending
		r.badTicks = 0
		fallthrough
	case statePending:
		if !bad {
			r.state = stateOK
			r.badTicks = 0
			return eventNone, 0, 0, ""
		}
		r.badTicks++
		if r.badTicks >= forTicks {
			r.state = stateFiring
			r.since = now
			r.clearCount = 0
			return eventAlert, v, th, msg
		}
	case stateFiring:
		if recovered {
			r.clearCount++
			if r.clearCount >= clearTicks {
				r.state = stateOK
				r.badTicks = 0
				r.clearCount = 0
				return eventResolve, v, th, msg
			}
		} else {
			r.clearCount = 0
		}
	}
	return eventNone, 0, 0, ""
}

func newRules(cfg Config) []*rule {
	var rs []*rule
	for _, rc := range []struct {
		realm string
		min   int
	}{{"cn", cfg.MinHealthyCN}, {"global", cfg.MinHealthyGlobal}} {
		if rc.min <= 0 {
			continue
		}
		realm, min := rc.realm, rc.min
		rs = append(rs, &rule{
			id: "realm_healthy_low", severity: "critical", realm: realm,
			sample: func(src Source) (float64, bool, bool, float64, string) {
				v := float64(src.Health(realm).Healthy)
				bad := v < float64(min)
				return v, bad, v >= float64(cfg.RecoverHealthy), float64(min),
					fmt.Sprintf("%s realm healthy accounts (%d) below threshold (%d)", realm, int(v), min)
			},
		})
	}
	if cfg.BreakerThreshold > 0 {
		th := cfg.BreakerThreshold
		rs = append(rs, &rule{
			id: "breaker_high", severity: "warning",
			sample: func(src Source) (float64, bool, bool, float64, string) {
				v := float64(src.Health("cn").Breaker + src.Health("global").Breaker)
				return v, v >= float64(th), v < float64(th), float64(th),
					fmt.Sprintf("accounts in circuit-breaker (%d) reached threshold (%d)", int(v), th)
			},
		})
	}
	rs = append(rs, &rule{
		id: "waf_ip_block", severity: "warning",
		sample: func(src Source) (float64, bool, bool, float64, string) {
			active := src.WAFActive()
			v := 0.0
			if active {
				v = 1
			}
			return v, active, !active, 1, "IP-level WAF block is active"
		},
	})
	return rs
}

type Snapshot struct {
	CN        RealmSnapshot `json:"cn"`
	Global    RealmSnapshot `json:"global"`
	WAFActive bool          `json:"waf_active"`
}

type RealmSnapshot struct {
	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Cooling  int `json:"cooling"`
	Disabled int `json:"disabled"`
	Breaker  int `json:"breaker"`
	// Degraded 连败降权中的账号数。alert.Health 一直有这个观测，但快照转换时
	// 被丢弃——webhook 接收方永远看不到「有一批号在连败降权」，只能等它恶化成
	// healthy 不足才被 realm_healthy_low 告警。
	Degraded int `json:"degraded"`
	InFlight int `json:"in_flight"`
}

type Payload struct {
	Version   int       `json:"version"`
	Event     string    `json:"event"`
	Service   string    `json:"service"`
	Rule      string    `json:"rule"`
	Severity  string    `json:"severity"`
	Realm     string    `json:"realm,omitempty"`
	Value     float64   `json:"value"`
	Threshold float64   `json:"threshold"`
	Message   string    `json:"message"`
	Since     time.Time `json:"since"`
	FiredAt   time.Time `json:"fired_at"`
	Snapshot  Snapshot  `json:"snapshot"`
}

// Monitor 阈值告警监视器。
//
// 热改设计（面板保存配置后只需调 Reconfigure，进程不重启）：
//   - cfg / rules / startedAt 由 mu 保护：Tick 推进规则状态机，Reconfigure 重建规则表，
//     两者必须串行——否则「保存配置」与「一轮检查」并发时，规则状态机被撕裂
//     （pending 计数错乱 → 该报的不报、不该报的重复报）；
//   - 间隔热改用 timer + rearm 唤醒：新 interval 不必等旧 ticker 到期才生效；
//   - http.Client 整只换：Timeout 可能变，而 Client 的字段不支持并发原地改；
//   - enabled=false 时 Run 空转等待（monitor 常驻），Tick / Notify 直接 no-op。
type Monitor struct {
	src Source

	// client 当前 HTTP 客户端（Timeout 热改后重建）。
	client atomic.Pointer[http.Client]

	mu        sync.Mutex
	cfg       Config
	rules     []*rule
	startedAt time.Time

	stopMu   sync.Mutex
	stop     chan struct{}
	stopOnce sync.Once
	// rearm 间隔/启用热改唤醒：Run 收到后立刻回循环顶重读配置并重编下一轮等待。
	rearm chan struct{}
}

func New(cfg Config, src Source) *Monitor {
	cfg.normalize()
	m := &Monitor{
		src:       src,
		cfg:       cfg,
		rules:     newRules(cfg),
		startedAt: time.Now(),
		rearm:     make(chan struct{}, 1),
	}
	m.client.Store(newHTTPClient(cfg))
	return m
}

// newHTTPClient 按配置构造客户端（超时 + 重定向限制：最多一跳且不许跨主机，
// 防 webhook 被 302 到内网/他域）。
func newHTTPClient(cfg Config) *http.Client {
	return &http.Client{
		Timeout: cfg.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 2 {
				return fmt.Errorf("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("cross-host redirect refused")
			}
			return nil
		},
	}
}

// Reconfigure 热改告警配置（面板保存配置后调用）：webhook_url / secret / interval /
// timeout / startup_grace / 各阈值 / for、clear ticks / send_resolve 全部即时生效。
//
// 规则状态按 (id, realm) 继承：只改间隔或超时这类「与阈值无关」的字段时，正在
// pending/firing 的规则保持原状态与计数，不会因重建规则对象而重新数 badTicks
// （否则保存一次配置就等于把「连续 2 次不健康才报」重置一次，该报的告警被无限推迟）。
func (m *Monitor) Reconfigure(cfg Config) {
	if m == nil {
		return
	}
	cfg.normalize()
	m.mu.Lock()
	prevEnabled := m.cfg.Enabled
	old := make(map[string]*rule, len(m.rules))
	for _, r := range m.rules {
		old[r.key()] = r
	}
	m.cfg = cfg
	rules := newRules(cfg)
	for _, nr := range rules {
		if o, ok := old[nr.key()]; ok {
			nr.state, nr.badTicks, nr.clearCount, nr.since = o.state, o.badTicks, o.clearCount, o.since
		}
	}
	m.rules = rules
	if cfg.Enabled && !prevEnabled {
		// 由关到开：重新起算启动静默期。进程可能已经跑了很多天，若沿用旧的
		// startedAt（远早于现在），刚打开告警就会拿当前快照立刻开火——用户预期
		// 的是「先观察一会儿，别一开就报」。
		m.startedAt = time.Now()
	}
	m.mu.Unlock()
	m.client.Store(newHTTPClient(cfg))
	poke(m.rearm)
}

// key 规则身份（用于 Reconfigure 时继承状态）。
func (r *rule) key() string { return r.id + "|" + r.realm }

// cfgSnapshot 取当前配置快照（无锁读给调用方自己用）。
func (m *Monitor) cfgSnapshot() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// HTTPTimeout 当前生效的 webhook 超时（面板观测/测试用）。
func (m *Monitor) HTTPTimeout() time.Duration { return m.cfgSnapshot().Timeout }

// graceRemaining 启动静默期还剩多久（<=0 = 已过；enabled=false 时返回 false）。
//
// Run 用它把「静默期结束的那一刻」作为第一次真正检查的时点：静默期的语义是
// 「刚起来先别报」，不是「起来后干等一个完整 interval」。顺带消掉一个边角 bug——
// 毫秒级时钟粒度（Windows ~1ms）下 startedAt 与紧随其后的 time.Now() 可能相等，
// 于是首次 Tick 被 now.Sub(startedAt) < StartupGrace 判成「仍在静默期」而跳过，
// 下一次要等整个 interval（默认 30s 还好，配置长了就是长期不告警）。
func (m *Monitor) graceRemaining() (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.cfg.Enabled {
		return 0, false
	}
	return m.startedAt.Add(m.cfg.StartupGrace).Sub(time.Now()), true
}

// Run 常驻循环：按 interval 轮询检查，直到 ctx 取消或 Stop。
//
// 与旧实现的差别：不再因 enabled=false 提前返回。monitor 由 main 常驻启动，
// 启停由 Reconfigure 的 enabled 热控——关掉时这里只等 rearm/ctx，不做任何上游
// 采样，也不会 busy loop。
func (m *Monitor) Run(ctx context.Context) {
	if m.src == nil {
		return
	}
	m.stopMu.Lock()
	if m.stop != nil {
		m.stopMu.Unlock()
		return
	}
	stop := make(chan struct{})
	m.stop = stop
	m.stopMu.Unlock()

	for {
		cfg := m.cfgSnapshot()
		if !cfg.Enabled {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-m.rearm:
				continue
			}
		}
		// 静默期还没过：等它过（可被 rearm/ctx/stop 打断），过完立刻检查一次。
		if rem, ok := m.graceRemaining(); ok && rem > 0 {
			grace := time.NewTimer(rem)
			select {
			case <-ctx.Done():
				grace.Stop()
				return
			case <-stop:
				grace.Stop()
				return
			case <-m.rearm:
				grace.Stop() // 配置变了：回循环顶重读（静默期可能被改短/关掉告警）
				continue
			case <-grace.C:
			}
		}
		m.Tick(time.Now())
		timer := time.NewTimer(cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-stop:
			timer.Stop()
			return
		case <-m.rearm:
			// 配置已变：立刻回循环顶重读（间隔是新的、开关可能是刚打开的）。
			// 这里会紧接着再 Tick 一次——规则是边沿触发的，重复 Tick 不会重复报警，
			// 换来的是「保存配置后立即按新参数检查一次」的即时观感。
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		m.stopMu.Lock()
		defer m.stopMu.Unlock()
		if m.stop != nil {
			close(m.stop)
			m.stop = nil
		}
	})
}

// Tick 跑一轮检查。enabled=false 或仍在启动静默期内时直接返回。
//
// 规则求值在 mu 内（状态机不能被 Reconfigure 撕裂），但 HTTP 发送放到锁外：
// 一次 webhook 最长可等 Timeout（默认 5s），不能让它把面板保存配置堵住。
func (m *Monitor) Tick(now time.Time) {
	if m == nil || m.src == nil {
		return
	}
	type fired struct {
		ev        event
		rule      *rule
		value     float64
		threshold float64
		msg       string
	}
	m.mu.Lock()
	cfg := m.cfg
	if !cfg.Enabled || now.Sub(m.startedAt) < cfg.StartupGrace {
		m.mu.Unlock()
		return
	}
	var out []fired
	for _, r := range m.rules {
		ev, value, threshold, msg := r.step(now, m.src, cfg.ForTicks, cfg.ClearTicks)
		switch ev {
		case eventAlert:
			out = append(out, fired{ev, r, value, threshold, msg})
		case eventResolve:
			if cfg.SendResolve {
				out = append(out, fired{ev, r, value, threshold, msg})
			}
		}
	}
	m.mu.Unlock()
	for _, f := range out {
		name := "alert"
		if f.ev == eventResolve {
			name = "resolve"
		}
		m.send(cfg, name, f.rule, now, f.value, f.threshold, f.msg)
	}
}

func (m *Monitor) snapshot() Snapshot {
	to := func(h Health) RealmSnapshot {
		return RealmSnapshot{Total: h.Total, Healthy: h.Healthy, Cooling: h.Cooling, Disabled: h.Disabled, Breaker: h.Breaker, Degraded: h.Degraded, InFlight: h.InFlight}
	}
	return Snapshot{CN: to(m.src.Health("cn")), Global: to(m.src.Health("global")), WAFActive: m.src.WAFActive()}
}

func (m *Monitor) send(cfg Config, eventName string, r *rule, now time.Time, value, threshold float64, msg string) {
	m.post(cfg, Payload{
		Version: 1, Event: eventName, Service: cfg.ServiceName, Rule: r.id, Severity: r.severity,
		Realm: r.realm, Value: value, Threshold: threshold, Message: msg, Since: r.since, FiredAt: now,
		Snapshot: m.snapshot(),
	}, r.id)
}

// Notify 推送一条通知（无阈值语义的事件型消息，如排程轮次结果汇总）。
// 未启用或无 webhook 时静默——通知是旁路，不该影响调用方主流程。
// enabled 也在这里生效：面板关掉告警后，排程汇总不该继续外发。
func (m *Monitor) Notify(message string) {
	if m == nil {
		return
	}
	cfg := m.cfgSnapshot()
	if !cfg.Enabled || cfg.WebhookURL == "" {
		return
	}
	m.post(cfg, Payload{
		Version: 1, Event: "notify", Service: cfg.ServiceName, Severity: "info",
		Message: message, FiredAt: time.Now(), Snapshot: m.snapshot(),
	}, "notify")
}

// post 发 payload 到 webhook（HMAC 签名同规则事件；失败只记日志，不重试）。
func (m *Monitor) post(cfg Config, p Payload, tag string) {
	body, err := json.Marshal(p)
	if err != nil {
		log.Printf("WARN: [alert] marshal payload rule=%s: %v", tag, err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("WARN: [alert] build request rule=%s: %v", tag, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "workbuddy2api-alert/1")
	if cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(cfg.Secret))
		mac.Write(body)
		req.Header.Set("X-WB2A-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	client := m.client.Load()
	if client == nil {
		client = newHTTPClient(cfg)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("WARN: [alert] webhook post failed rule=%s: %v", tag, err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("WARN: [alert] webhook returned %d rule=%s", resp.StatusCode, tag)
	}
}

// poke 非阻塞唤醒（已有待处理信号则忽略）。
func poke(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}
