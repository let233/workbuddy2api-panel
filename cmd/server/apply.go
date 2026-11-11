// apply.go 把一份配置推给所有可热改组件 —— 「保存即生效」的唯一落点。
//
// 为什么集中在一处：启动装配与面板保存必须走**同一套映射**。历史教训是两处各写一份，
// 新增字段只接了一边，症状为「面板保存成功、跑起来还是旧值」，且只有重启才恢复，
// 排查成本极高。
//
// 约定：
//   - Apply 只「把值推给组件」，不读写库、不碰文件（保存事务由调用方管）。
//   - 组件内部用原子量/锁保护运行期读，故可以在服务中随时调用；Apply 之间由 mu
//     串行，避免两次保存交叉推字段（A 的 key 配 B 的 URL 这类错拼状态）。
//   - 少数需要重建资源的项（redis 连接、监听 socket）也在本函数收口。
package main

import (
	"log"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/alert"
	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/panel"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/redisstore"
	"github.com/misakano7545/workbuddy2api-panel/internal/reqlog"
	"github.com/misakano7545/workbuddy2api-panel/internal/scheduler"
	"github.com/misakano7545/workbuddy2api-panel/internal/server"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// hotApplier 持有全部运行期组件的引用；字段可为 nil（缺省装配/单测只测局部映射）。
type hotApplier struct {
	db    *store.Store
	up    *upstream.Client
	pool  *pool.Pool
	sch   *scheduler.Scheduler
	gw    *server.Handler
	pn    *panel.Panel
	live  *livecfg.Holder
	sess  *session.Router
	rl    *reqlog.Recorder
	mon   *alert.Monitor
	rdb   *redisstore.Switcher
	serve *serverManager
	audit *server.AuditLog

	mu       sync.Mutex
	redisURL string
	redisTok string
	listen   string
}

// Apply 把整份配置推给所有组件（不落库、不写文件）。
//
// 返回值只在「监听地址没热切成功」这类可报告的问题上非 nil：其余项目都是原子赋值，
// 不会失败。调用方（面板保存）要把它原样回给前端——用户以为改好了、实际端口还是
// 旧的，比直接报错难查得多。
func (a *hotApplier) Apply(c *Config) error {
	if a == nil || c == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	// —— 出站（upstream）：整份快照替换 + 按需重建 Transport ——
	if a.up != nil {
		rt := upstreamRuntime(c)
		a.up.SetRuntime(rt)
		a.up.SanitizeFingerprints.Store(c.Features.SanitizeBlacklistFingerprints)
		a.up.SanitizeBrandScope.Store(c.Features.SanitizeBrandScope)
		a.up.KeepRecentRounds.Store(int64(c.Prompt.KeepRecentRounds))
		// 思考档位策略：改默认档 / 客户端优先 / 作用域后，下一次 chat 即按新策略注入。
		a.up.SetEffortPolicy(effortPolicyOf(c))
	}
	// auth 侧 global 闸（第二道闸），与 upstream 快照里的 global.enabled 同源。
	auth.SetGlobalEnabled(c.Global.Enabled)

	// —— livecfg 快照（api_key / 软冷却 / 脱敏 / 白黑名单 / 别名 / 降级表 / 思考镜像）——
	if a.live != nil {
		a.live.Store(liveSnapshotOf(c))
	}

	// —— 池（选号 / 熔断 / 限流 / 保底 / 保留积分）——
	if a.pool != nil {
		p := a.pool
		p.SetBreaker(c.Pool.BreakerThreshold, c.BreakerCooldownDur, c.BreakerCooldownMaxD)
		p.SetMaxInFlight(c.Pool.MaxInFlight)
		p.SetMaxInFlightGlobal(c.Pool.MaxInFlightGlobal)
		p.SetDegrade(c.Pool.DegradeThreshold, c.DegradeCooldownDur, c.DegradeCooldownMaxD)
		p.SetSoftRateMax(c.SoftRateMaxDur)
		p.SetCostExploreInterval(c.CostExploreIntervalDur)
		p.SetBalanceWindow(c.BalanceWindowDur)
		p.SetCreditFloor(c.Pool.CreditFloor)
		p.SetCreditFloorMaxRate(c.Pool.CreditFloorMaxRate)
		p.SetWeights(c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
		p.SetPreferExpiring(c.Pool.PreferExpiring, c.ExpiringSoonDur)
		if c.Pool.ReserveCredits != nil {
			// 指针为 nil = 配置里没写这一项（沿用当前值），不能当 0 推下去。
			p.SetReserveCredits(*c.Pool.ReserveCredits)
		}
		p.SetReserveCreditsByRealm(c.Pool.ReserveCreditsByRealm)
		p.SetAccountPriority(c.Pool.AccountPriority)
		p.SetAccountShare(c.Pool.AccountShare)
		p.SetStrategy(c.Pool.SelectionStrategy)
	}

	// —— 会话粘性 ——
	if a.sess != nil {
		a.sess.SetEnabled(c.SessionSticky.Enabled)
		a.sess.Reconfigure(c.SessionTTL, c.SessionGCInterval)
	}

	// —— redis 镜像（upstash）：改地址/token 才换实现；没变就不白建一条连接 ——
	if a.rdb != nil && (c.Upstash.URL != a.redisURL || c.Upstash.Token != a.redisTok) {
		a.rdb.Swap(c.Upstash.URL, c.Upstash.Token)
		a.redisURL, a.redisTok = c.Upstash.URL, c.Upstash.Token
	}

	// —— 请求归档（只关归档，内存指标照旧）——
	if a.rl != nil {
		a.rl.SetConfig(reqlog.Config{
			DB:            a.db,
			Enabled:       c.Logging.RequestArchiveEnabled,
			RetentionDays: c.Logging.RequestRetentionDays,
			MaxBytes:      int64(c.Logging.RequestArchiveMaxMB) << 20,
		})
	}

	// —— 排程（小时制 + 周期循环全部走原子槽位）——
	if a.sch != nil {
		sch := a.sch
		sch.Reconfigure(scheduler.ScheduleParams{
			CheckinHours:   c.Schedule.CheckinHours,
			TravelHours:    c.Schedule.TravelHours,
			ActivityHours:  c.Schedule.ActivityHours,
			KeepaliveHours: c.Schedule.KeepaliveHours,
			GrowthHours:    c.Schedule.GrowthHours,

			CheckinDisabled:        !c.Schedule.CheckinEnabled,
			TravelDisabled:         !c.Schedule.TravelEnabled,
			ActivityDisabled:       !c.Schedule.ActivityEnabled,
			WebActivityDisabled:    !c.Schedule.WebActivityEnabled,
			KeepaliveDisabled:      !c.Schedule.KeepaliveEnabled,
			GrowthDisabled:         !c.Schedule.GrowthEnabled,
			IncludeDisabledInTasks: c.Schedule.IncludeDisabledInTasks,

			JitterMinutes:              c.Schedule.JitterMinutes,
			AccountSpreadMinutes:       c.Schedule.AccountSpreadMinutes,
			AccountSpreadMinGapSeconds: c.Schedule.AccountSpreadMinGapSeconds,
		})
		sch.SetExpiringSoonWindow(c.ExpiringSoonDur)
		sch.SetKeepaliveRefreshWindow(c.KeepaliveRefreshWindowDur)
		sch.SetJitter(c.Schedule.BalanceRefreshJitterPermill, c.Schedule.TaskSweepJitterPermill)
		sch.SetMakeupCardsPerRound(c.Schedule.MakeupCardsPerRound)
		// 三个周期循环常驻，开关与间隔每轮现读：关掉只是空转等待，打开立刻恢复。
		// 抖动统一由 SetJitter 按千分比控制（上面一行），故这里的 duration 抖动传 0，
		// 避免与 permill 口径叠加成双重抖动。
		sch.SetBalanceRefresh(c.Schedule.BalanceRefreshEnabled, c.BalanceRefreshInterval, 0)
		sch.SetTaskSweep(c.Schedule.TaskSweepEnabled, c.TaskSweepInterval, 0)
		sch.SetWindowProbe(c.Schedule.WindowProbeEnabled, 0)
	}

	// —— 告警（enabled=false 时 Run 空转等待，webhook 配置改了也能重新开起来）——
	if a.mon != nil {
		a.mon.Reconfigure(alertConfigOf(c))
	}

	// —— 网关（server.Handler）：请求侧全部热参数 ——
	if a.gw != nil {
		gw := a.gw
		gw.SetBudgetLimit(c.Budget.DailyCreditLimit)
		gw.SetBudgetGuards(c.Budget.BurstCreditLimit, c.Budget.CacheLowRate)
		gw.SetSoftCooldown(c.SoftRateDur)
		gw.SetMaxPayloadBytes(c.Server.MaxPayloadBytes)
		gw.SetHeadKeepalive(c.HeadKeepaliveDur)
		gw.SetRestrictionHold(time.Duration(c.Upstream.RestrictionHoldSeconds) * time.Second)
		// 提示词模式与文本成套切换：只改一半会出现「passthrough 却带着 custom 文本」。
		gw.SetPrompt(c.Prompt.Mode, c.PromptText)
		gw.SetGlobalEnabled(c.Global.Enabled)
		gw.SetAdminEnabled(c.Admin.Enabled)
		gw.SetMetricsEnabled(c.Metrics.Enabled)
		gw.SetWatchdog(c.Watchdog.Enabled, time.Duration(c.Watchdog.IntervalSeconds)*time.Second)
		// 审计：开关关闭时置空（下一条 /admin 请求即不落库），打开时由调用方先建好审计器。
		gw.SetAudit(a.audit)
	}

	// —— 面板自身参数 ——
	if a.pn != nil {
		a.pn.SetAPIKey(c.APIKey)
		a.pn.SetAuthDir(c.AuthDir)
		a.pn.SetPollIntervalSec(c.Panel.PollIntervalSec)
		a.pn.SetPackageDetailLimit(c.Panel.PackageDetailLimit)
		a.pn.SetGrowthConcurrency(c.Schedule.GrowthConcurrency)
		a.pn.SetReloadConfig(c.ReloadIntervalDur, c.Accounts.ReloadDeleteConfirm)
		// 面板日志落库：交给 panel 自己改参数（已挂着就只改参数、不重建内存环；
		// 关掉时先刷缓冲再摘、退回任务归档语义）。别在这里再 SetLogStore，会双写。
		if err := a.pn.ReconfigureLogs(c.Logging.PanelLogEnabled,
			c.Logging.PanelLogRetentionDays, c.Logging.PanelLogMaxMB); err != nil {
			warnApply("面板日志落库", err)
		}
	}

	// —— 监听地址与读超时（改端口不重启）：Start 之前由 Start 自己负责 ——
	if a.serve != nil {
		a.serve.SetReadTimeout(c.ServerReadTimeoutDur)
		if a.listen != "" && a.listen != c.Listen {
			if err := a.serve.Listen(c.Listen); err != nil {
				return err
			}
			a.listen = a.serve.Addr()
		}
	}
	return nil
}

// syncAudit 按配置启停审计器（Apply 之前调用：Apply 只负责把当前指针推给网关）。
//
// 审计开关在面板上原本属于「改完要重启」的一类：它决定启动时建不建审计器。现在
// 关→开时现场新建（只是持有 db 与 key 指纹的轻对象，不占连接），开→关时置空。
func (a *hotApplier) syncAudit(c *Config) {
	if a == nil || a.gw == nil {
		return
	}
	if !c.Admin.AuditEnabled {
		a.audit = nil
		return
	}
	if a.audit != nil {
		return
	}
	al, err := server.NewAuditLog(a.db, c.APIKey)
	if err != nil {
		warnApply("审计日志", err)
		return
	}
	a.audit = al
	log.Printf("admin audit: %s", al.Path())
}

// liveSnapshotOf 由整份配置算出 livecfg 快照（启动与热改同源）。
//
// 抽出来是为了避免「启动时归一化一次、保存时再手写一次」两份口径：模型白黑名单、
// 别名这类要归一的字段，漏掉一次就会出现「保存后名单失效直到重启」。
func liveSnapshotOf(c *Config) livecfg.Snapshot {
	allowed, allowlist := livecfg.NormalizeAllowedModels(c.Models.Allowed)
	return livecfg.Snapshot{
		APIKey:                 c.APIKey,
		SoftCooldown:           c.SoftRateDur,
		SanitizeFingerprints:   c.Features.SanitizeBlacklistFingerprints,
		EmbeddingsEnabled:      c.Features.EmbeddingsEnabled,
		WatermarkStripEnabled:  c.Features.WatermarkStripEnabled,
		RecordClientInfo:       c.Logging.RequestClientInfo,
		RecordContext:          c.Logging.RequestArchiveContext,
		HiddenModels:           livecfg.NormalizeHiddenModels(c.Models.Hidden),
		AllowedModels:          allowed,
		AllowlistEnabled:       allowlist,
		ModelAliases:           livecfg.NormalizeModelAliases(c.Models.Aliases),
		ModelFallback:          c.Pool.ModelFallback,
		ThinkingEnabled:        c.Thinking.Enabled,
		ThinkingDefaultEffort:  c.Thinking.DefaultEffort,
		ThinkingClientPriority: c.Thinking.ClientPriority,
		ThinkingOverrideScope:  c.Thinking.OverrideScope,
		ThinkingModelDefaults:  c.Thinking.ModelDefaults,
	}
}

// alertConfigOf config 的 alerting 段 → alert.Config（启动与热改同源）。
func alertConfigOf(c *Config) alert.Config {
	al := c.Alerting
	return alert.Config{
		Enabled:          al.Enabled,
		WebhookURL:       al.WebhookURL,
		Secret:           al.Secret,
		Interval:         time.Duration(al.IntervalSeconds) * time.Second,
		Timeout:          time.Duration(al.TimeoutSeconds) * time.Second,
		StartupGrace:     time.Duration(al.StartupGraceSeconds) * time.Second,
		MinHealthyCN:     al.MinHealthyCN,
		MinHealthyGlobal: al.MinHealthyGlobal,
		RecoverHealthy:   al.RecoverHealthy,
		BreakerThreshold: al.BreakerThreshold,
		ForTicks:         al.ForTicks,
		ClearTicks:       al.ClearTicks,
		SendResolve:      al.SendResolve,
	}
}

// warnApply 记录热应用失败（不致命：单项失败不该让整次保存回滚，
// 但必须留痕——静默失败等于「面板显示已保存、实际没生效」）。
func warnApply(name string, err error) {
	if err != nil {
		log.Printf("WARN: [config] 热应用 %s 失败：%v", name, err)
	}
}
