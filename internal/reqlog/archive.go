package reqlog

import (
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// errArchiveClosed 归档写循环已退出：清空不能再排队等一个没人处理的结果。
var errArchiveClosed = errors.New("reqlog: 归档已停止")

// archiveStore 请求事件归档的落库器。
//
// 语义与原 JSONL 归档一致：请求侧只进队列（满则丢弃并计数），后台写循环攒批
// 写库、按保留天数与容量上限清理；读路径按 id 倒序分页扫、在内存里套 Filter，
// 保持「大小写不敏感子串包含」的筛选口径不变。
type archiveStore struct {
	// db / enabled / retention / maxBytes 运行期可热改（SetConfig）：写循环每轮读
	// 当前值，改配置后下一轮即按新口径开关与清理。用 atomic 而非互斥锁，是因为
	// enabled 在**请求路径**上每条事件都要读一次（enqueue），锁会把它变成热点。
	db        atomic.Pointer[store.Store]
	enabled   atomic.Bool
	retention atomic.Int64 // 保留时长（纳秒）
	maxBytes  atomic.Int64 // 文本总量软上限（字节）

	ch        chan Event
	purgeCh   chan chan PurgeResult
	stop      chan struct{}
	done      chan struct{}
	closed    atomic.Bool
	closeOnce sync.Once
	// startMu/started 写循环的懒启动：启动时给了 DB 就立刻起；启动时没给（面板/
	// 测试后来才 SetConfig 带 DB）则在其后补起——否则「后来配了 DB 却永远写不进去」。
	startMu   sync.Mutex
	started   atomic.Bool
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	// buf 待写库的行（写循环独占，不需要加锁）。
	buf []reqLogRow
}

// newArchiveStore 构建落库器。管道与关停 channel 恒建（很小），写循环只在
// DB 存在时才起——这样「启动时 Enabled=false、保存配置后打开」也能直接开始写。
func newArchiveStore(cfg Config) *archiveStore {
	queue := cfg.QueueSize
	if queue <= 0 {
		queue = defaultQueue
	}
	w := &archiveStore{
		ch:      make(chan Event, queue),
		purgeCh: make(chan chan PurgeResult, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	w.apply(cfg)
	w.startIfNeeded()
	return w
}

// apply 把一份 Config 应用为当前生效值（New 与 SetConfig 共用）。
//
// 语义：DB 只在**非 nil** 时采纳（面板热改不会传 DB=nil 来「清掉数据库」，
// 而 Config 零值会让调用方不小心清掉）；RetentionDays/MaxBytes <=0 回落默认，
// 与 New 口径一致。Enabled 还要求真有 DB——没有 DB 时「开启归档」无从落库。
//
// QueueSize 不在此列：队列长度在 New 时就固化成 channel 容量，运行期改不了
// （改它需要重建 channel，代价与收益不成比例）。
func (w *archiveStore) apply(cfg Config) {
	if cfg.DB != nil {
		w.db.Store(cfg.DB)
	}
	days := cfg.RetentionDays
	if days <= 0 {
		days = 7
	}
	w.retention.Store(int64(time.Duration(days) * 24 * time.Hour))
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	w.maxBytes.Store(maxBytes)
	w.enabled.Store(cfg.Enabled && w.db.Load() != nil)
}

// startIfNeeded 有 DB 且尚未启动时起写循环（幂等）。
func (w *archiveStore) startIfNeeded() {
	if w == nil || w.db.Load() == nil || w.closed.Load() {
		return
	}
	w.startMu.Lock()
	defer w.startMu.Unlock()
	if w.started.Load() {
		return
	}
	w.started.Store(true)
	go w.run()
}

// dbRef 当前数据存储（可为 nil = 纯内存模式）。
func (w *archiveStore) dbRef() *store.Store { return w.db.Load() }

func (w *archiveStore) enqueue(e Event) {
	// enabled 读一次原子量：关掉归档时请求路径上零成本（不排队、不落库），
	// 但内存指标仍由 Recorder 照常维护——与 New 时 Enabled=false 的语义一致。
	if w == nil || !w.enabled.Load() || w.closed.Load() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveStore) run() {
	defer close(w.done)
	flushTicker := time.NewTicker(time.Second)
	defer flushTicker.Stop()
	pruneTicker := time.NewTicker(reqLogPruneEvery)
	defer pruneTicker.Stop()
	for {
		select {
		case e := <-w.ch:
			// 关掉归档时队列里残留的事件直接丢弃，不写库（「Enabled=false 即停止
			// 写库」的语义必须盖住已经入队的那几条）。
			if !w.enabled.Load() {
				continue
			}
			w.appendEvent(e)
		case out := <-w.purgeCh:
			out <- w.purgeNow()
		case <-flushTicker.C:
			w.flush()
		case <-pruneTicker.C:
			w.flush()
			w.prune()
		case <-w.stop:
			// 停止前把队列里剩下的事件写掉，再刷一次库。
			for {
				select {
				case e := <-w.ch:
					w.appendEvent(e)
				default:
					w.flush()
					return
				}
			}
		}
	}
}

func (w *archiveStore) appendEvent(e Event) {
	row, err := rowOfEvent(e)
	if err != nil {
		w.setErr(err)
		return
	}
	w.buf = append(w.buf, row)
	if len(w.buf) >= reqLogFlushBatch {
		w.flush()
	}
}

// flush 把攒下的行写进归档表；失败时行留在内存里下次重试（有上限，超出丢弃并计数）。
func (w *archiveStore) flush() {
	if len(w.buf) == 0 {
		return
	}
	db := w.dbRef()
	if db == nil || !w.enabled.Load() {
		// 关掉归档时把攒下的行丢掉：停用后不该再有新行落库（下次开启重新攒）。
		w.buf = nil
		return
	}
	rows := w.buf
	w.buf = nil
	if err := store.Insert(db, rows...); err != nil {
		w.setErr(err)
		pending := append(rows, w.buf...)
		if len(pending) > reqLogMaxPending {
			drop := len(pending) - reqLogMaxPending
			w.dropped.Add(uint64(drop))
			pending = pending[drop:]
		}
		w.buf = pending
		return
	}
	w.clearErr()
}

func (w *archiveStore) prune() {
	db := w.dbRef()
	if db == nil || !w.enabled.Load() {
		return
	}
	// 保留天数/容量上限每轮重读：改配置后下一个清理周期（1 分钟）就按新口径淘汰，
	// 不必重启进程。
	if retention := time.Duration(w.retention.Load()); retention > 0 {
		cutoff := time.Now().Add(-retention)
		if err := db.DB().Where("ts < ?", cutoff).Delete(&reqLogRow{}).Error; err != nil {
			w.setErr(err)
			return
		}
	}
	w.pruneSize()
}

// pruneSize 按自然日整日淘汰，直到文本总量落回上限之内。
// 最新一天永不淘汰：它是正在写的归档，删掉等于把当前事故现场清空。
func (w *archiveStore) pruneSize() {
	db := w.dbRef()
	maxBytes := w.maxBytes.Load()
	if db == nil || maxBytes <= 0 {
		return
	}
	var days []struct {
		Day   string
		Bytes int64
	}
	err := db.DB().Model(&reqLogRow{}).
		Select("day AS day, COALESCE(SUM(LENGTH(payload)), 0) AS bytes").
		Group("day").Order("day ASC").Scan(&days).Error
	if err != nil {
		w.setErr(err)
		return
	}
	var total int64
	for _, d := range days {
		total += d.Bytes
	}
	for i, d := range days {
		if total <= maxBytes || i == len(days)-1 {
			return
		}
		if err := db.DB().Where("day = ?", d.Day).Delete(&reqLogRow{}).Error; err != nil {
			w.setErr(err)
			return
		}
		total -= d.Bytes
	}
}

// read 按 id 倒序分页扫，套 Filter 后返回最近的 limit 条（时间倒序）。
func (w *archiveStore) read(limit int, filter Filter) ([]Event, error) {
	db := (*store.Store)(nil)
	if w != nil {
		db = w.dbRef()
	}
	if w == nil || !w.enabled.Load() || db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	var (
		out     []Event
		lastID  uint
		scanned int
	)
	for len(out) < limit && scanned < reqLogScanMax {
		q := db.DB().Model(&reqLogRow{})
		if !filter.From.IsZero() {
			q = q.Where("ts >= ?", filter.From)
		}
		if !filter.To.IsZero() {
			q = q.Where("ts <= ?", filter.To)
		}
		if filter.Outcome != "" {
			q = q.Where("outcome = ?", filter.Outcome)
		}
		if lastID > 0 {
			q = q.Where("id < ?", lastID)
		}
		var rows []reqLogRow
		if err := q.Order("id DESC").Limit(reqLogPageSize).Find(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		scanned += len(rows)
		lastID = rows[len(rows)-1].ID
		for _, row := range rows {
			e, err := row.event()
			if err != nil {
				// 单行坏数据不该让整页 500：跳过它，其余照常返回。
				continue
			}
			if !filter.match(e) {
				continue
			}
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
		if len(rows) < reqLogPageSize {
			break
		}
	}
	// 分页按 id 取到的是「最近的一批命中」，这里再按时间升序取最后 limit 条并倒序，
	// 与原文件归档「读到的范围内最新的 limit 条」语义一致。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (w *archiveStore) purge() (PurgeResult, error) {
	if w == nil || !w.enabled.Load() {
		return PurgeResult{}, nil
	}
	// 写循环没起（没有 DB）或已关停：没有可清的东西，直接回零值——
	// 绝不把面板 handler 挂在那里等一个没人处理的结果。
	if !w.started.Load() || w.closed.Load() {
		return PurgeResult{}, errArchiveClosed
	}
	out := make(chan PurgeResult, 1)
	select {
	case w.purgeCh <- out:
	case <-w.done:
		return PurgeResult{}, errArchiveClosed
	}
	select {
	case res := <-out:
		return res, nil
	case <-w.done:
		return PurgeResult{}, errArchiveClosed
	}
}

// drainPending 把队列里还没处理的事件搬到缓冲区（不阻塞），
// 让「清空」覆盖调用瞬间之前 Record 过的全部事件。
func (w *archiveStore) drainPending() {
	for {
		select {
		case e := <-w.ch:
			w.appendEvent(e)
		default:
			return
		}
	}
}

func (w *archiveStore) purgeNow() PurgeResult {
	w.drainPending()
	w.flush()
	res := PurgeResult{}
	db := w.dbRef()
	if db == nil {
		return res
	}
	var agg struct {
		N     int64
		Bytes int64
	}
	if err := db.DB().Model(&reqLogRow{}).
		Select("COUNT(*) AS n, COALESCE(SUM(LENGTH(payload)), 0) AS bytes").
		Scan(&agg).Error; err != nil {
		w.setErr(err)
		return PurgeResult{}
	}
	if _, err := store.DeleteAll[reqLogRow](db); err != nil {
		w.setErr(err)
		return PurgeResult{}
	}
	res.Rows = agg.N
	res.Bytes = agg.Bytes
	return res
}

func (w *archiveStore) stats() ArchiveStats {
	if w == nil {
		return ArchiveStats{Target: reqLogTableName}
	}
	s := ArchiveStats{
		Enabled:       w.enabled.Load(),
		Target:        reqLogTableName,
		DroppedWrites: w.dropped.Load(),
		LastError:     w.errString(),
	}
	db := w.dbRef()
	if !s.Enabled || db == nil {
		return s
	}
	var agg struct {
		N     int64
		Days  int64
		Bytes int64
	}
	err := db.DB().Model(&reqLogRow{}).
		Select("COUNT(*) AS n, COUNT(DISTINCT day) AS days, COALESCE(SUM(LENGTH(payload)), 0) AS bytes").
		Scan(&agg).Error
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	s.Rows = agg.N
	s.Days = int(agg.Days)
	s.Bytes = agg.Bytes
	return s
}

func (w *archiveStore) close() {
	if w == nil {
		return
	}
	// 写循环从未启动（启动时没有 DB）：没有可停的东西，只标记关停，避免
	// 在没人处理的 done 上永久阻塞。注意这里不看 enabled——关闭归档后仍需
	// 能把写循环停掉（否则 goroutine 泄漏）。
	if !w.started.Load() {
		w.closed.Store(true)
		return
	}
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		close(w.stop)
		<-w.done
	})
}

func (w *archiveStore) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveStore) clearErr() {
	w.lastErrMu.Lock()
	if w.lastErr != "" {
		log.Printf("[reqlog] 归档落库恢复（此前错误: %s）", w.lastErr)
		w.lastErr = ""
	}
	w.lastErrMu.Unlock()
}

func (w *archiveStore) errString() string {
	if w == nil {
		return ""
	}
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}

// match 判断事件是否满足筛选条件（与存储实现无关，读路径复用）。
func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.Model, f.Model) && !containsFold(e.ModelEff, f.Model) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
