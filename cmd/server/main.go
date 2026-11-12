// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // WB2API_PPROF 打开时提供 /debug/pprof（默认无监听）
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/alert"
	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/balance"
	"github.com/misakano7545/workbuddy2api-panel/internal/credithist"
	"github.com/misakano7545/workbuddy2api-panel/internal/gatewaykeys"
	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/panel"
	"github.com/misakano7545/workbuddy2api-panel/internal/panelauth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/proxypool"
	"github.com/misakano7545/workbuddy2api-panel/internal/redisstore"
	"github.com/misakano7545/workbuddy2api-panel/internal/reqlog"
	"github.com/misakano7545/workbuddy2api-panel/internal/scheduler"
	"github.com/misakano7545/workbuddy2api-panel/internal/server"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/updater"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

// appVersion 面板版本，透出到 /panel/api/overview。
// 本地构建保持 dev。发布构建由 CI 用 -X 注入上海时间（2026.9.26.711），不跟上游 1.x。
var appVersion = "dev"

// appRepository 面板「在线更新」查询 Release 用的仓库（owner/repo），可在构建时用
// -X 覆盖。与 go.mod 里的模块路径无关：模块路径是历史血缘，跟着上游走没意义，
// 这里只决定「去哪儿找新版本」。
var appRepository = "let233/workbuddy2api-panel"

// defaultCredWarning 首次部署的初始口令 + 非回环监听 = 把面板交给整个内网：
// 只要管理员还是「刚建出来、没改过密码」的状态就提醒一次（改密后不再打扰）。
// 只打印告警，不阻塞启动。
func defaultCredWarning(listen string, defaultAdmin bool) string {
	if !defaultAdmin {
		return ""
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen // 非 host:port 形态：按具名地址保守处理（继续告警）
	}
	switch {
	case host == "", host == "0.0.0.0", host == "::":
		// 空 host = 所有网卡，维持告警
	case host == "127.0.0.1", host == "::1", strings.EqualFold(host, "localhost"):
		return "" // 只听回环：暴露面有限，不打扰
	}
	return fmt.Sprintf("面板监听 %s 非回环地址，而管理员仍是初始口令 %s——请登录后立即修改密码", listen, panelauth.DefaultAdminPassword)
}

// dataDirOf 返回数据目录（旧版 state.json 所在目录）。
//
// 运行数据现在都进数据库，这个目录只剩两个用途：SQLite 缺省库文件的落点、旧版
// JSON 数据的一次性导入来源。规则与旧版一致——跟着 state_file 走，改配置时整份
// 数据一起搬，不必再引一个路径配置项。
func dataDirOf(stateFile string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return "."
	}
	return dir
}

func main() {
	if handled, err := updater.HandleSelfUpdate(os.Args); handled {
		if err != nil {
			log.Printf("[updater] %v", err)
			os.Exit(1)
		}
		return
	}
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；只放端口与数据库，其余配置入库）")
	flag.Parse()
	// 实际生效的配置路径。必须在 flag.Parse() **之后**取值：flag.String 返回的指针
	// 在 Parse 时才写入命令行值，此前取到的是默认值拷贝，-config 自定义路径时横幅
	// 会报一份根本没读的文件。下面退回「默认+env」时清空，同理（issue #155）。
	loadedFrom := *cfgPath

	// 引导配置：配置文件只带「进程起来之前就必须知道」的三样东西——监听地址、
	// 数据目录、数据库连接。其余配置（上游 / 池 / 排程 / 提示词 / 告警 / 预算 /
	// api_key …）全部入库存 app_kv，面板保存即热生效，不再是「重启才生效」。
	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 落一份只含引导参数的配置再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if werr := WriteBootstrap(*cfgPath, Default()); werr == nil {
				log.Printf("config %s 不存在，已生成引导配置（只含端口与数据库；其余配置与 api_key 首次启动自动生成入库）", *cfgPath)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env，不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				loadedFrom = ""
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}
	// 文件原始内容：用来判断这份配置是不是「旧版全量配置」，需要在入库时迁移一次。
	rawConfigFile, _ := os.ReadFile(*cfgPath)

	// 数据存储：运行数据（池状态 / 用量桶 / 请求归档 / 余额趋势 / 作业 / 日志 /
	// 积分历史 / 模型上限缓存）全部进关系库，账号仍只读 auths/*.json（不进库）。
	// 缺省 SQLite（纯 Go 实现、无 CGO、零外部服务，开箱即用），可配 mysql / postgres。
	dataDir := dataDirOf(cfg.StateFile)
	dbCfg := cfg.DatabaseSettings()
	if dbCfg.Driver == store.DriverSQLite {
		// 首次部署时库文件父目录可能还不存在：先建目录，否则 sqlite 打不开。
		if err := store.EnsureDir(dbCfg.DSN); err != nil {
			log.Fatalf("database dir: %v", err)
		}
	}
	db, err := store.Open(dbCfg)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.LogSummary()
	// 旧版 JSON 数据一次性导入库（幂等：成功后写 app_kv 标记并把旧文件改名
	// *.imported）。必须早于各记录器装配，否则记录器先从空表起算再被导入覆盖。
	store.ImportLegacy(db, dataDir)

	// 运行期配置从库里读，覆盖在「文件 + 默认值」之上。首次从旧版全量配置文件
	// 升级时整份搬进库，并把文件改写为只含引导参数（否则用户升级一次就发现
	// 面板里的设置全变回默认值）。api_key 缺失（首次部署 / 老库里没存）时
	// 现场生成 sk- 前缀随机 key 并入库：配置文件不再是密钥的落点。
	settings := newSettingsHub(db, *cfgPath)
	if migrated, serr := settings.Load(cfg, hasLegacySettings(rawConfigFile)); serr != nil {
		log.Fatalf("load settings: %v", serr)
	} else if migrated {
		log.Printf("[config] 旧版配置文件中的设置已整体迁移入库，配置文件已改写为只含端口与数据库")
	} else if hasLegacySettings(rawConfigFile) {
		// 库里已经有配置，文件里却还写着运行期键：这些键**不再被读取**（配置以库为准）。
		// 不明说一句，"改了配置文件没反应" 就会变成下一个 issue——旧版里 dns_servers /
		// admin.* / budget.* 这类只在文件里改的旋钮，现在都得在面板或库里改。
		log.Printf("[config] 警告：%s 里的运行期配置已不再生效（配置以数据库为准）。该文件只读 listen / state_file / database；其余请在面板「配置」页改（保存即生效），或直接改库里 app_kv 的 %s",
			*cfgPath, settingsKVKey)
	}
	cfg = settings.Current()

	// 面板登录账号：首次部署播种 admin/123456（bcrypt 哈希入 panel_users 表）。
	// 密码不再是配置项——没有 account 就没有任何管理员密钥可泄漏。
	users := panelauth.NewManager(db)
	defer users.Close()
	defaultAdminPending := false
	if _, created, uerr := users.EnsureDefaultAdmin(); uerr != nil {
		log.Fatalf("panel auth: %v", uerr)
	} else if created {
		defaultAdminPending = true
		// 只在这一次打印明文：无头部署（容器 / 不开浏览器）得从这里抄走初始密码。
		log.Printf("[auth] 首次部署：面板管理员 %s / 初始密码 %s（登录后请立即修改；此行只打印一次）",
			panelauth.DefaultAdminUsername, panelauth.DefaultAdminPassword)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	// 外面再包一层 Switcher：面板改 upstash.url/token 保存后热换后端，不重启进程。
	// 全进程共用同一个 Switcher 指针（pool / session 都持有它），Swap 一次处处生效。
	rstore := redisstore.NewSwitcher(redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token))
	var store redisstore.Store = rstore

	p := pool.New(db)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetBalanceWindow(cfg.BalanceWindowDur)             // 窗口用量调度（issue #112，默认 1h；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetCreditFloorMaxRate(cfg.Pool.CreditFloorMaxRate) // 保底兜底的目录倍率上限（0 = 严格模式）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetReserveCredits(*cfg.Pool.ReserveCredits) // 保留积分：余额低于阈值停止接单（0 = 关闭，缺省 10）
	p.SetReserveCreditsByRealm(cfg.Pool.ReserveCreditsByRealm)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring, cfg.ExpiringSoonDur)
	p.SetAccountPriority(cfg.Pool.AccountPriority) // 账号优先级/占比（issue #62）：空表 = 不启用
	p.SetAccountShare(cfg.Pool.AccountShare)
	// 选号策略（pool.selection_strategy）：weighted 缺省 / round_robin / sequential。
	p.SetStrategy(cfg.Pool.SelectionStrategy)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := rstore.Current().(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 出站 ClientHello 指纹档位：默认 node（复刻官方 Node/OpenSSL 的 JA3/JA4）。
	// 配 go 才退回 crypto/tls 原生形态——唯一用途是指纹导致握手异常时紧急回滚。
	if err := up.SetTLSFingerprint(cfg.Upstream.TLSFingerprint); err != nil {
		log.Fatalf("CONFIG: %v", err)
	}
	if up.Runtime().TLSFingerprint == upstream.TLSFingerprintNode {
		log.Printf("[upstream] TLS 指纹复刻已启用（JA3 %s / JA4 %s）",
			upstream.OfficialJA3MD5, upstream.OfficialJA4)
	} else {
		log.Printf("[upstream] TLS 指纹为原生 Go 形态（JA3/JA4 与官方客户端不一致）")
	}

	// 出站 DNS（config dns_servers / WB2A_DNS）：显式指定解析器，绕开系统配置。
	// 典型场景是 Android（无 /etc/resolv.conf，Go 解析器退回 [::1]:53 全域名失败）
	// 与本地 DNS 被污染。未配置时保持系统解析器，行为零变化。
	if len(cfg.DNSServers) > 0 {
		up.SetDNSDialer(upstream.NewDNSDialer(cfg.DNSServers))
	}

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })
	// 出站参数（超时 / UA / 域名 / 设备 token / dump / TLS 指纹）整份装进
	// upstream.Runtime 快照：读侧每请求原子读、写侧走 SetRuntime/UpdateRuntime 并
	// 重建 Transport，故这些值全部可以在运行期热改（面板保存即生效）。
	up.SetRuntime(upstreamRuntime(cfg))
	// proxypool 克隆基底走 BaseTransport()：Transport 实例会随配置重建，克隆基底
	// 必须每次现取，否则换了 TLS 指纹/idle 超时之后走代理的请求还用旧实例。
	proxies, err := proxypool.New(db,
		up.BaseTransport(), upstream.NewDNSDialer(cfg.DNSServers))
	if err != nil {
		log.Fatalf("proxy pool: %v", err)
	}
	defer proxies.Close()
	// 基底 Transport 每次现取：TLS 指纹 / idle 超时 / dialer 热改后，经代理的流量
	// 也必须跟着换到新基底克隆（否则只有直连生效）。
	proxies.SetBaseProvider(up.BaseTransport)
	proxies.SetTLSOver(up.TLSOver) // 经代理的 HTTPS 与直连同一指纹、同一请求头顺序
	up.ProxyPool = proxies
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	up.SanitizeBrandScope.Store(cfg.Features.SanitizeBrandScope)
	up.KeepRecentRounds.Store(int64(cfg.Prompt.KeepRecentRounds))
	// 思考档位策略（thinking 节）：启动装载，面板保存配置时经 saveConfig 热更新。
	up.SetEffortPolicy(effortPolicyOf(cfg))
	if cfg.Thinking.Enabled {
		priority := "客户端优先（只在缺档时补默认档）"
		if !cfg.Thinking.ClientPriority {
			priority = "网关优先（改写客户端档位）"
			if cfg.Thinking.OverrideScope == "all" {
				priority = "网关优先（档位与关闭意图一起覆盖）"
			}
		}
		log.Printf("思考档位策略：默认档 %s；%s", cfg.Thinking.DefaultEffort, priority)
	} else {
		log.Printf("思考档位策略已关闭（不补档、不改写客户端档位）")
	}
	if cfg.Features.SanitizeBlacklistFingerprints {
		if cfg.Features.SanitizeBrandScope == "identity" {
			log.Printf("指纹脱敏已启用；品牌词规则仅作用于 system/developer（features.sanitize_brand_scope=identity）")
		} else {
			log.Printf("指纹脱敏已启用；品牌词规则作用于所有消息角色（features.sanitize_brand_scope=all）")
		}
	}
	// 出站 UA / 归属头 / 域名覆盖均已随 upstreamRuntime(cfg) 进快照（见上）。
	// global realm 路由（config global 段）：上游侧开关（第一道闸）在快照里；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// 模型上限缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 落在 model_catalog 表（与其它运行数据同库）。首次缺失/损坏自动回落仓库内嵌
	// 种子；models.dev 按需拉取成功后写回表里。
	upstream.SetModelCatalogDB(db)

	// 余额采样记录器：采样点落 balance_points 表（与其它运行数据同库）。
	// 采样动作挂在余额刷新之后（见 internal/scheduler），这里只管装配与落库生命周期。
	bal := balance.New(db)
	bal.Start()
	defer bal.Stop()
	log.Printf("[balance] 余额趋势采样已启用: %s 每 %v 一点（%s）", db.Describe(), balance.Interval(), bal.Describe())

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 余额采样记录器：挂在余额刷新之后（scheduler 内），nil = 不采样。
		Balance: bal,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow:  cfg.ExpiringSoonDur,
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		WebActivityDisabled: !cfg.Schedule.WebActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
		GrowthDisabled:      !cfg.Schedule.GrowthEnabled,
		// 周期保活只刷新临期凭证（默认 7 天窗口）；面板手动全量保活不受影响。
		KeepaliveRefreshWindow: cfg.KeepaliveRefreshWindowDur,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
		JitterMinutes:          cfg.Schedule.JitterMinutes,
		// 账号级摊开（issue #45）：默认 0/0 = 关闭，同一秒打全池。
		AccountSpreadMinutes:       cfg.Schedule.AccountSpreadMinutes,
		AccountSpreadMinGapSeconds: cfg.Schedule.AccountSpreadMinGapSeconds,
		// 启动补跑去重标记落 app_kv 表：重启多次只在当天补一轮。
		DB: db,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.GrowthEnabled:
		log.Printf("成长任务队列已禁用（schedule.growth_enabled=false）")
	default:
		log.Printf("成长任务队列已启用：%v 点（每日自动跑一遍任务中心待办，账号间并发 %d）",
			cfg.Schedule.GrowthHours, cfg.Schedule.GrowthConcurrency)
	}
	if cfg.Schedule.JitterMinutes > 0 {
		log.Printf("排程抖动：各任务在名义整点后 0-%d 分钟（schedule.jitter_minutes）", cfg.Schedule.JitterMinutes)
	}
	if cfg.Schedule.AccountSpreadMinutes > 0 {
		log.Printf("账号摊开：账号遍历型任务在 %d 分钟内按账号错开，最小间隔 %d 秒（schedule.account_spread_minutes）",
			cfg.Schedule.AccountSpreadMinutes, cfg.Schedule.AccountSpreadMinGapSeconds)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	switch {
	case !cfg.Schedule.TaskSweepEnabled:
		log.Printf("补领清扫已禁用（schedule.task_sweep_enabled=false）")
	case cfg.TaskSweepInterval > 0:
		log.Printf("补领清扫：每 %s（只 accept 待接受的、只 claim 已达标未领的）", cfg.TaskSweepInterval)
	}
	if !cfg.Schedule.WindowProbeEnabled {
		log.Printf("额度窗口探针已禁用（schedule.window_probe_enabled=false）")
	}
	if cfg.Schedule.BalanceRefreshJitterPermill > 0 {
		log.Printf("周期抖动（人味化）：余额刷新 ±%d‰（账号遍历同步摊开）、补领清扫 ±%d‰，不再精确固定间隔（schedule.*_jitter_permill）",
			cfg.Schedule.BalanceRefreshJitterPermill, cfg.Schedule.TaskSweepJitterPermill)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	// 模型白名单（issue #68）：启动时归一化一次，快照只读持有。
	live := livecfg.New(liveSnapshotOf(cfg))
	// 用量记录器：小时/日聚合桶落 usage_buckets 表（与其它运行数据同库）。
	rec := usage.New(db)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s", rec.Describe())

	// 请求指标始终启用；归档（req_logs 表）只记脱敏元数据，写库失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		DB:            db,
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；归档表 %s（保留 %d 天，上限 %d MiB）",
			rs.Target, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；归档已关闭")
	}

	var gw *server.Handler
	// configSaveMu 串行化面板配置保存（并发两次保存会在读-改-写文件与热应用之间互踩）。
	var configSaveMu sync.Mutex
	// pn 先声明再赋值：SaveConfig 闭包要在同一条语句里捕获它（:= 的作用域从语句结束才开始）。
	// 积分历史：把每次真实查到的余额与上一次比对，变动即留痕（新账号首次只建
	// 基线）。挂载在余额查询路径上（upstream 的余额观察者），签到 / 活跃上报 /
	// 旅行 / 保活 / 面板手动刷新全覆盖——上游只在少数渠道落金额日志，比对余额
	// 是唯一可靠的口径。落 credit_history / credit_baselines 表（与其它数据同库）。
	creditHist := credithist.New(db, 2000)
	up.SetCreditObserver(creditHist.Observe)
	log.Printf("[credithist] 积分历史已启用: %s（%d 条封顶）", db.Describe(), 2000)

	updateManager := updater.New(appVersion, appRepository, *cfgPath, updateSettingsFrom(cfg))
	// 热应用入口：启动末尾与面板每次保存都走同一份映射（见 apply.go）。
	// gw/pn/mon/logs/serve 稍后装配时再补进来；这里先给出此刻已就绪的组件。
	ap := &hotApplier{
		db: db, up: up, pool: p, sch: sch, live: live, sess: sessRouter,
		rl: requestLog, rdb: rstore, redisURL: cfg.Upstash.URL, redisTok: cfg.Upstash.Token,
	}
	// 客户端 API Key 管理器：多 Key（到期时间 / 可用模型 / 积分额度 / 提示词策略）
	// 与分组模板落库，重启后原样恢复；网关照它鉴权并按 Key 计费。
	gatewayKeys := gatewaykeys.NewManager(db)
	defer gatewayKeys.Close()
	// 首次部署播种一把最高级别 Key（不限积分/模型、提示词跟随全局）：否则新装实例
	// 一把客户端 Key 都没有，网关对全部请求 401，用户会以为服务坏了。
	if k, created, kerr := gatewayKeys.EnsureSystemKey(); kerr != nil {
		log.Printf("WARN: 首次部署系统 Key 创建失败（可在面板「API Key」页手建）: %v", kerr)
	} else if created {
		log.Printf("[gateway] 首次部署：已创建系统 Key「%s」secret=%s（不限积分/模型；面板「API Key」页可复制；此行只打印一次）", k.Name, k.Secret)
	}
	var pn *panel.Panel
	pn = panel.New(panel.Config{
		DB:         db,
		Pool:       p,
		ProxyPool:  proxies,
		Usage:      rec,
		RequestLog: requestLog,
		Upstream:   up,
		Scheduler:  sch,
		// 降级路由表：面板据此在限流提示里标注「已路由至 <等价模型>」。
		ModelFallback: func() map[string]string { return live.Load().ModelFallback },
		// 余额趋势采样记录器：nil 时该视图返回 501。
		Balance: bal,
		// 后台任务的自动触发开关与成长任务排程共用热配置（schedule.growth_enabled）。
		AutoTasksEnabled: sch.GrowthEnabled,
		AuthDir:          cfg.AuthDir,
		Users:            users,
		RedisMode:        redisMode,
		StickyCount:      sessCount,
		Version:          appVersion,
		Updater:          updateManager,
		PollIntervalSec:  cfg.Panel.PollIntervalSec,
		PublicDir:        cfg.Panel.PublicDir,
		Live:             live,
		ConfigPath:       *cfgPath,
		LoadConfig: func() (any, error) {
			// 配置在库里（app_kv 的 config:settings），不再读配置文件——那里现在只剩
			// 端口与数据库这些引导参数。
			return settings.Current(), nil
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			configSaveMu.Lock()
			defer configSaveMu.Unlock()
			fields, err := saveConfig(raw, settings, ap)
			if err != nil {
				return fields, err
			}
			// 更新镜像设置（更新源/自动更新）与保存同批：保存后不必再读文件。
			updateManager.Configure(updateSettingsFrom(settings.Current()))
			return fields, nil
		},
		// 积分历史账本（只读展示；写入由上面的余额观察者完成）。
		CreditHistory: creditHist,
		// 客户端 API Key 管理器（面板「API Key」页读写）。
		Keys: gatewayKeys,
	})
	// 面板运行日志全频道按天落盘（logging.panel_log_*）：重启后回填、支持按区间导出。
	// 装配顺序必须在 log.SetOutput 之前——回填完再把实时输出接上，否则启动期的
	// 那几行会先入环、再被回填的历史插到前面，时间序就乱了。
	var panelLogs *panel.LogStore
	if cfg.Logging.PanelLogEnabled {
		panelLogs = panel.NewLogStore(db,
			cfg.Logging.PanelLogRetentionDays, cfg.Logging.PanelLogMaxMB)
		if panelLogs == nil {
			log.Printf("WARN: 面板日志落库不可用（未连数据库），退回纯内存环")
		} else {
			if err := pn.Logs().SetLogStore(panelLogs); err != nil {
				log.Printf("WARN: 面板日志回填失败（不影响新日志）: %v", err)
			}
			defer panelLogs.Close()
			log.Printf("[panel] 运行日志落库 %s（保留 %d 天，上限 %d MiB）",
				panelLogs.Dir(), cfg.Logging.PanelLogRetentionDays, cfg.Logging.PanelLogMaxMB)
		}
	}
	// 兜底：落库关掉或不可用时，仍用任务频道那份表格快照回填（旧行为）。
	if panelLogs == nil {
		if err := pn.Logs().SetTaskArchive(db); err != nil {
			log.Printf("WARN: task log archive: %v", err)
		}
	}
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))
	// 成长任务队列排程的执行体在面板（队列逻辑唯一实现）：注入回调，避免 scheduler → panel 循环依赖。
	sch.SetGrowthRunner(pn.RunGrowthQueueNow)
	sch.SetSweepRunner(pn.RunTaskSweepNow)
	pn.SetGrowthConcurrency(cfg.Schedule.GrowthConcurrency)

	var auditLog *server.AuditLog
	if cfg.Admin.AuditEnabled {
		// 审计记录进 admin_audit 表（admin.audit_file 不再决定落点，仅旧配置兼容保留）。
		al, aerr := server.NewAuditLog(db)
		if aerr != nil {
			p.Flush() // Fatal 绕过 defer：至少把池状态落盘再退，否则丢整轮运行状态
			log.Fatalf("admin audit: %v", aerr)
		}
		auditLog = al
		log.Printf("admin audit: %s", al.Path())
	}
	// MCP serverInfo.version 与面板 /status 用同一个版本串（本地构建是 dev）。
	server.BuildVersion = appVersion
	gw = server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		Keys:         gatewayKeys,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		RequestLog:   requestLog,
		PromptMode:   cfg.Prompt.Mode,
		// 模型降级路由表：读 live 快照（支持面板热更，改完即生效、无需重启）。
		ModelFallback: func() map[string]string { return live.Load().ModelFallback },
		PromptText:    cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		RecordContext:    cfg.Logging.RequestArchiveContext,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		// 模型黑名单原始条目；运行期以 livecfg 快照为准（热生效），此处供
		// Live 为 nil 的裸用/测试路径。
		ModelsHidden: cfg.Models.Hidden,
		// 模型白名单原始条目（issue #68）；同上，运行期以 livecfg 快照为准。
		ModelsAllowed: cfg.Models.Allowed,
		// 模型映射原始条目（issue #30）；同上，运行期以 livecfg 快照为准。
		ModelsAliases:    cfg.Models.Aliases,
		AdminEnabled:     cfg.Admin.Enabled,
		MetricsEnabled:   cfg.Metrics.Enabled,
		Tasks:            sch,
		Audit:            auditLog,
		BudgetLimit:      cfg.Budget.DailyCreditLimit,
		BurstCreditLimit: cfg.Budget.BurstCreditLimit,
		CacheLowRate:     cfg.Budget.CacheLowRate,
		// 入站请求体 OOM 护栏 + 11140 连坐暂停（两处均由 hotApplier 热改，保存即生效）。
		MaxPayloadBytes: cfg.Server.MaxPayloadBytes,
		RestrictionHold: time.Duration(cfg.Upstream.RestrictionHoldSeconds) * time.Second,
		// 等上游首帧期间的 SSE 注释帧间隔（server.head_keepalive，缺省 1.5s）。
		HeadKeepalive: cfg.HeadKeepaliveDur,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	proxyCtx, stopProxies := context.WithCancel(ctx)
	proxyDone := make(chan struct{})
	go func() {
		defer close(proxyDone)
		proxies.Run(proxyCtx)
	}()
	defer func() { stopProxies(); <-proxyDone }()
	updateManager.SetRestart(stop)
	go updateManager.Run(ctx)
	// 账号目录热重载（issue #199）：外部同步进来的 auths/*.json 不必重启进程即可入池，
	// 删文件也会在连续 N 次扫描确认后出池。面板「刷新」按钮之外还提供
	// POST /panel/api/accounts/reload 给脚本用；间隔 0s 时只保留手动接口。
	pn.SetReloadConfig(cfg.ReloadIntervalDur, cfg.Accounts.ReloadDeleteConfirm)
	go pn.ReloadLoop(ctx.Done())
	// 账号后台任务（taskjobs）：状态落 task_jobs 表，重启后自动恢复未完成作业。
	if err := pn.StartTaskJobs(ctx); err != nil {
		p.Flush() // Fatal 绕过 defer：至少把池状态落盘再退
		log.Fatalf("task jobs: %v", err)
	}
	defer pn.StopTaskJobs()
	pn.ResumeTaskJobs()
	go sch.Run(ctx)
	// 余额刷新 / 补领清扫 / 额度窗口探针：三条周期循环全部**常驻**，开关与间隔由
	// hotApplier.Apply 下发（enabled=false 只是空转等待）。所以装配期无条件启动，
	// 面板随后打开/关闭/改间隔都立刻生效，不必重启进程。
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	sch.StartTaskSweep(ctx, cfg.TaskSweepInterval)
	// 额度窗口探针：上游按首次请求起算额度窗口，429 重置时刻到了没人发请求窗口就
	// 不开始（额度空转）。到点主动发一个最小请求把起点钉住（schedule.window_probe_enabled）。
	sch.StartModelWindowProbe(ctx)

	// 告警监控常驻（alerting.enabled=false 时空转等待）：面板改 webhook/间隔/阈值
	// 由 Apply → Reconfigure 下发，下一次 tick 即按新值判断。
	mon := alert.New(alertConfigOf(cfg), alertSource{pool: p, h: gw})
	go mon.Run(ctx)
	// 排程轮次结果汇总的推送出口（与告警 webhook 同一条管道；未启用时只落面板日志）。
	pn.SetNotifier(mon.Notify)

	// 上游目录改版监控：常驻循环，开关与间隔每轮从原子的 hot 槽位现读（SetWatchdog
	// 由 Apply 下发），故这里只启动一次。未启用告警时 mon.Notify 自身 no-op，变化仍
	// 写进日志（面板任务日志里能翻到）。
	go gw.WatchCatalog(ctx, time.Duration(cfg.Watchdog.IntervalSeconds)*time.Second, mon.Notify)

	// 首轮热应用：把启动装配里散落的运行期参数统一收敛成「配置说了算」——同一份
	// 映射既用于这里也用于面板保存，任何字段漏接都会在启动日志/行为里立刻暴露。
	ap.gw = gw
	ap.pn = pn
	ap.mon = mon
	ap.audit = auditLog
	if err := ap.Apply(cfg); err != nil {
		log.Printf("WARN: 启动热应用: %v", err)
	}
	if cfg.Alerting.Enabled {
		log.Printf("alerting: webhook on, every %ds", cfg.Alerting.IntervalSeconds)
	}
	if cfg.Watchdog.Enabled {
		log.Printf("watchdog: 上游目录监控开，每 %ds", cfg.Watchdog.IntervalSeconds)
	}

	go warmModelRates(ctx, up, p)

	// 诊断用：WB2API_PPROF=127.0.0.1:6060 时另起一个只跑 pprof 的监听。
	// 默认不开启（不设环境变量就完全没有这个监听），供「并发一高就降速」这类
	// 只能靠真实负载定位的 CPU 问题取 profile（取完即关容器，不常驻生产）。
	if addr := os.Getenv("WB2API_PPROF"); addr != "" {
		go func() {
			log.Printf("[pprof] 诊断监听开启 %s（CPU: /debug/pprof/profile?seconds=30）", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				log.Printf("WARN: [pprof] 监听失败 %v", err)
			}
		}()
	}

	// 监听交给 serverManager：改端口/改绑定网卡不必重启进程（先听新的再关旧的），
	// 读超时也不写 http.Server 字段（运行期写 = 数据竞争），改由连接层按原子值
	// 注入 deadline（见 serve.go）。
	//
	// server.read_timeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
	// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定 60s
	// 会掐掉大上下文/文件块经反代链的慢速上传）；"0" 显式关闭。现在**改完即生效**。
	// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积
	// （SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout，
	// 长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
	serveMgr := newServerManager(gw)
	serveMgr.SetReadTimeout(cfg.ServerReadTimeoutDur)
	ap.serve = serveMgr
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// 在途请求排干窗口可配（server.shutdown_timeout，缺省 30s）：长流式 SSE
		// 合法时长可达分钟级，固定 5s 必然把在途请求掐成连接重置；超时不再静默。
		shutdownTimeout := settings.Current().ShutdownTimeoutDur
		if err := serveMgr.Shutdown(shutdownTimeout); err != nil {
			log.Printf("WARN: 优雅关停未在 %s 内完成，在途请求被放弃: %v", shutdownTimeout, err)
		}
	}()

	if warn := defaultCredWarning(cfg.Listen, defaultAdminPending); warn != "" {
		log.Printf("WARN: %s", warn)
	}
	addr, err := serveMgr.Start(cfg.Listen)
	if err != nil {
		p.Flush() // 监听失败时信号路径不会跑：这里同样兜底落盘再退
		log.Fatalf("http: %v", err)
	}
	ap.listen = addr // 之后 Listen 变化时才需要热切（启动这次由 Start 完成）
	log.Print(listenBanner(addr, loadedFrom, defaultAdminPending))
	<-ctx.Done()
	<-shutdownDone
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled() && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// listenBanner 组装启动横幅。不再有任何密钥要打码：面板是账号密码登录，
// 客户端 Key 只在面板「API Key」页展示。path 传「实际加载的那份」配置文件，
// 退回默认时传空——启动横幅仍要说清配置从哪来（issue #155 的教训）。
func listenBanner(listen, path string, defaultAdmin bool) string {
	source := path
	if source == "" {
		source = "默认配置"
	}
	login := "账号密码登录"
	if defaultAdmin {
		login = "初始账号 " + panelauth.DefaultAdminUsername + "（密码见启动日志）"
	}
	return fmt.Sprintf("workbuddy2api listening on %s (配置 %s；面板 %s)，管理面板 http://127.0.0.1%s/panel/",
		listen, source, login, panelListenPath(listen))
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// effortPolicyOf 把 config thinking 节转成 upstream 运行时策略。nil 配置/
// 未启用时返回的策略由 Client.SetEffortPolicy 统一处理（Enabled=false 即不生效）。
func effortPolicyOf(cfg *Config) *upstream.EffortPolicy {
	if cfg == nil {
		return nil
	}
	return &upstream.EffortPolicy{
		Enabled:        cfg.Thinking.Enabled,
		DefaultEffort:  cfg.Thinking.DefaultEffort,
		ClientPriority: cfg.Thinking.ClientPriority,
		OverrideScope:  cfg.Thinking.OverrideScope,
		ModelDefaults:  cfg.Thinking.ModelDefaults,
	}
}

// updateSettingsFrom 把 config 的 update 段翻成 updater.Settings（issue #198）。
//
// 单独抽成函数是因为「启动装配」与「配置保存后热更新」两处必须用同一口 IO——
// 各写一份字面量时，新增字段必然只接一处（热更新丢了 proxy/mirror，面板看着
// 保存成功、实际还是旧通道）。
//
// proxy_from_env 在 config 里是 *bool：JSON 缺键 = 未表态 → 沿用环境变量（历史
// 行为，也是绝大多数部署的实际形态）；显式 false 才完全不读环境。
func updateSettingsFrom(u *Config) updater.Settings {
	fromEnv := true
	if u.Update.ProxyFromEnv != nil {
		fromEnv = *u.Update.ProxyFromEnv
	}
	return updater.Settings{
		AutoEnabled:  u.Update.AutoEnabled,
		AutoTime:     u.Update.AutoTime,
		Proxy:        u.Update.Proxy,
		ProxyFromEnv: fromEnv,
		Mirror:       u.Update.Mirror,
		InsecureTLS:  u.Update.InsecureTLS,
	}
}

// saveConfig 面板保存配置的唯一入口：合并 → 校验 → 入库 → 热应用。
//
// 合并基线是**内存里的当前配置**（含 env 覆盖与启动归一化），不是磁盘文件：配置
// 已整体入库，config.json 现在只剩 listen/state_file/database，拿它当基线会把其余
// 几百个键一并丢成默认值。
//
// 顺序刻意是「先入库、后应用」：
//   - 入库成功 = 用户看到保存成功；之后的失败只可能是监听切换这类局部问题，报错
//     即可，不回滚——回滚会留下「库里新值、运行旧值」这种更难查的持久错位。
//   - 单项热应用失败不致命（warnApply 留痕），不该为它让整次保存失败。
func saveConfig(raw []byte, hub *settingsHub, ap *hotApplier) ([]string, error) {
	baseline := hub.Current()
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	base, err := configMap(baseline)
	if err != nil {
		return nil, err
	}
	merged := mergeConfigMaps(base, incoming)

	// 与启动同一套 Default + normalize 校验：非法值在入库前就拒绝（错误信息带字段名）。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	// 环境变量覆盖与启动同优先级：保存路径同样应用，否则 env 托管的字段会被文件值
	// 抢班——面板显示 env 值、运行却按文件值。
	applyEnv(newCfg)
	if err := newCfg.normalize(); err != nil {
		return nil, err
	}

	if err := hub.Save(newCfg); err != nil {
		return nil, err
	}
	// 热应用：全部运行期参数在 Apply 一处收口（与启动装配同源）。
	if ap != nil {
		ap.syncAudit(newCfg)
		if aerr := ap.Apply(newCfg); aerr != nil {
			return restartRequiredFields(baseline, newCfg), aerr
		}
	}
	return restartRequiredFields(baseline, newCfg), nil
}

// configMap 把配置结构体翻回 map（按 json 标签口径，供深合并用）。
func configMap(c *Config) (map[string]any, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal current config: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("reparse current config: %w", err)
	}
	return m, nil
}

// upstreamRuntime 把 config 里所有出站参数翻成一份 upstream.Runtime 快照。
//
// 抽成函数的原因：启动装配与面板保存后的热应用必须走同一口映射。各写一份字面量
// 时新增一个出站字段必然只接一处——保存看着成功、实际还是旧 UA / 旧域名。
// cn.* / global.* 为空即「保留内置默认」，与老配置（无这两个段）行为一致。
func upstreamRuntime(cfg *Config) upstream.Runtime {
	return upstream.Runtime{
		UserAgent:       cfg.Upstream.UserAgent,
		ClientVersion:   cfg.Upstream.ClientVersion,
		CliVersion:      cfg.Upstream.CliVersion,
		ClientName:      cfg.Upstream.ClientName,
		DeviceToken:     cfg.Upstream.DeviceToken,
		DeviceTokenFile: cfg.Upstream.DeviceTokenFile,
		PassthroughIP:   cfg.Upstream.PassthroughIP,
		ChatBaseCN:      cfg.CN.ChatBase,
		BillingBaseCN:   cfg.CN.BillingBase,
		// global realm 路由：上游侧开关（第一道闸）+ base 覆盖。
		ChatBaseGlobal:    cfg.Global.ChatBase,
		BillingBaseGlobal: cfg.Global.BillingBase,
		GlobalEnabled:     cfg.Global.Enabled,
		// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels）。
		Timeout: time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second,
		// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
		HeaderTimeout: time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second,
		// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
		IdleTimeout:    time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second,
		DumpDir:        cfg.Upstream.DumpDir,
		DumpRequestMax: cfg.Upstream.DumpMaxBytes,
		TLSFingerprint: cfg.Upstream.TLSFingerprint,
	}
}

// restartRequiredFields 只报「确实变化且无法热生效」的字段。
//
// 现在几乎全部热生效，剩下的是两处**引导参数**：换数据库要迁移数据、换 state_file
// 要重开数据目录，都发生在建连接之前，运行期改不了。其余字段改完即作用于下一条
// 请求，把它们列进待重启清单只会让用户以为还得重启一次。
//
// 无独立 pending 集合：改回原值即自动从清单里消失。
func restartRequiredFields(current, next *Config) []string {
	if current == nil || next == nil {
		return nil
	}
	var out []string
	changed := func(name string, before, after any) {
		if !reflect.DeepEqual(before, after) {
			out = append(out, name)
		}
	}
	changed("database", current.Database, next.Database)
	changed("state_file", current.StateFile, next.StateFile)
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
