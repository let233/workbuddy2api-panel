// Package reqlog 记录脱敏的请求级指标与可选的请求事件归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；归档落数据库表 req_logs，
// 只存请求元数据，不存响应正文、Authorization 或其它凭证。可选的调用来源
// （客户端 IP / User-Agent，见 Event.ClientIP/UserAgent）与会话上下文
// （见 Event.Context）都由 server 按配置开关决定是否填充——提示词默认不落库。
// 归档队列满时丢弃并计数，不允许归档写库阻塞模型请求。
package reqlog

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

const (
	recentLimit  = 100
	defaultQueue = 1024
	// defaultReadMax 一次读取归档的上限（与面板 limit 上限同口径）。
	defaultReadMax = 1000
	// reqLogScanMax 单次筛选最多回看的行数。
	//
	// 面板的筛选语义是「大小写不敏感的子串包含」（一段 IP 前缀、UA 片段），
	// SQL 里没法直接表达，只能按 id 倒序分页扫、在内存里过滤。归档表可能有几十万行，
	// 给个上限，避免一次筛选把整表读进内存；命中太少时结果会偏旧，但绝不 OOM。
	reqLogScanMax = 20000
	// reqLogPageSize 分页扫描的每页行数。
	reqLogPageSize = 500
	// reqLogFlushBatch 攒够这么多行就写一次库（写循环 1s 也会兜底刷）。
	reqLogFlushBatch = 200
	// reqLogMaxPending 写库失败后最多在内存里挂这么多行，超出丢弃并计数。
	reqLogMaxPending = 20000
	// reqLogPruneEvery 过期清理间隔：保留天数 + 容量上限。
	reqLogPruneEvery = time.Minute
	// reqLogDayLayout 归档按自然日分组用的布局（导出、按天容量淘汰）。
	reqLogDayLayout = "2006-01-02"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 或 DB=nil 时仍保留内存指标。
type Config struct {
	// DB 数据存储（gorm）。nil = 纯内存模式：只留内存指标，不落任何归档。
	DB *store.Store
	// Enabled 归档开关。
	Enabled bool
	// RetentionDays 归档保留天数，缺省 7；<=0 回落默认。
	RetentionDays int
	// MaxBytes 归档表文本总量的软上限（按自然日整日淘汰），缺省 100 MiB。
	MaxBytes int64
	// QueueSize 归档队列长度，缺省 1024；满了丢弃并计数。
	QueueSize int
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Path      string    `json:"path"`
	Account   string    `json:"account,omitempty"`
	Model     string    `json:"model,omitempty"`
	// ModelEff 模型降级（pool.model_fallback / model_unavailable_fallback）后**实际
	// 使用**的模型（2026-10-25 实案）：只记 Model 会让「global:deepseek-v4.1-flash」
	// 与实际在跑的「deepseek-v4.1-flash-sg」归为一类——两者计价/能力都不同，
	// 归档与面板请求记录必须分开可辨。空 = 未降级。
	ModelEff         string  `json:"model_eff,omitempty"`
	Status           int     `json:"status"`
	OK               bool    `json:"ok"`
	Outcome          string  `json:"outcome"`
	DurationMs       int64   `json:"duration_ms"`
	TTFBMs           int64   `json:"ttfb_ms,omitempty"`
	Attempts         int     `json:"attempts,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens,omitempty"`
	CompletionTokens int64   `json:"completion_tokens,omitempty"`
	TotalTokens      int64   `json:"total_tokens,omitempty"`
	Credit           float64 `json:"credit,omitempty"`
	HasCredit        bool    `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92 同步）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64  `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64  `json:"cache_miss_tokens,omitempty"`
	ClientIP        string `json:"client_ip,omitempty"`
	UserAgent       string `json:"user_agent,omitempty"`
	Mode            string `json:"mode,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ReasoningEffortRequested 客户端原始请求的思考档位（reasoning_effort / reasoningEffort
	// 原值；空字符串 = 客户端没携带）。与上一列（生效值）配对：生效值可能来自网关补默认档、
	// 策略改写或就近降级——只给生效值时，「客户端要了 max」与「客户端什么都没发、网关补了
	// max」在事件页/JSONL 里长得一模一样（issue #43 的读法歧义）。
	// **刻意不带 omitempty**：旧归档行没有这个键（「不知道客户端发了什么」），新行恒有
	// （空串「确认客户端没发」）——两种情况在事件页里要能分开。
	ReasoningEffortRequested string `json:"reasoning_effort_requested"`
	ReasoningTokens          int64  `json:"reasoning_tokens,omitempty"`
	ExecutorType             string `json:"executor_type,omitempty"`
	// ToolsDeclared / ToolCalls 本轮的「工具面」观测量（issue #44）：声明了几个工具、
	// 实际拿到几个工具调用。ToolsDeclared>0 且 ToolCalls==0 就是一次「零工具调用
	// 收尾」——上游把本该调工具的回合当成正常完成，客户端据此认为任务做完、工具一次
	// 没跑。只记数量，不记工具名与参数。
	ToolsDeclared int `json:"tools_declared,omitempty"`
	ToolCalls     int `json:"tool_calls,omitempty"`
	// Context 本轮请求的会话上下文（system + messages，或出图/embedding 的 prompt），
	// 由 logging.request_archive_context 决定是否填充（缺省关，空则省略）。
	// 这是用户数据：默认不开，开了等于把提示词落到 state 目录的归档文件里。
	Context json.RawMessage `json:"context,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	Model     string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled bool `json:"enabled"`
	// Target 归档落地位置（数据库表名）。
	Target string `json:"target,omitempty"`
	// Rows / Days 归档行数与覆盖的自然日数。
	Rows          int64  `json:"rows"`
	Days          int    `json:"days"`
	Bytes         int64  `json:"bytes"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	archive     *archiveStore
}

// New 创建记录器；DB 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveStore(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	r.recent = append([]Event{e}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    append([]Event(nil), r.recent...),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter)
}

// PurgeResult 清空归档的结果：删掉的行数与字节数。
// 行数给面板显示「清了多少」，字节数给测试断言（0 字节 ≠ 没删行）。
type PurgeResult struct {
	Rows  int64 `json:"rows"`
	Bytes int64 `json:"bytes"`
}

// Purge 清空归档表（面板「清空归档」）。返回删除量。
// 归档关闭时返回零值：没有可清的东西，不算失败；归档写循环已退出则返回错误，
// 绝不把调用方（面板 HTTP handler）挂在那里等一个没人处理的结果。
func (r *Recorder) Purge() (PurgeResult, error) {
	if r == nil || r.archive == nil {
		return PurgeResult{}, nil
	}
	return r.archive.purge()
}

// SetConfig 热改归档配置（面板保存配置后调用）：Enabled / RetentionDays / MaxBytes
// 立即生效，写库与清理循环下一轮就按新口径走，不必重启进程。
//
// 语义与 New 一致：Enabled=false 时**停止写库**（队列里残留的事件也丢弃），但内存
// 指标照常保留——Recorder 的计数/最近事件与归档开关无关，关掉归档不影响 /status
// 观测。DB 只在非 nil 时采纳（Config 零值不该把已有数据库清掉）。
func (r *Recorder) SetConfig(cfg Config) {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.apply(cfg)
	// 启动时 DB 为 nil（纯内存模式）而这次配置里带了 DB：把写循环补起来，
	// 否则「打开了归档却永远写不进去」。
	r.archive.startIfNeeded()
}

// Close 刷库并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}
func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
