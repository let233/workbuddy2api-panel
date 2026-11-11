// logstore.go 面板「运行日志」的持久化、重启回填与按区间导出。
//
// 为什么需要（此前只有内存环 + 任务频道的一份快照）：
//
//	环形缓冲每频道 500 条，对话一多就把上午的行冲掉；进程一重启全部归零。
//	于是「昨天下午那波 429 是什么时候开始的」这类问题，只能靠当时有没有人截屏。
//	事故复盘要看的是**过去几天的原始行**，这是一个纯粹的可观测性缺口。
//
// 与 reqlog 的分工（不要合并）：
//
//	reqlog      逐请求的**结构化元数据**（模型/账号/时延/结果），用于统计与筛选
//	logstore    面板三频道（chat/task/sys）的**原始文本行**，用于照着日志读事故
//
// 存储：数据库表 panel_logs（旧实现是按天 JSONL 文件 panel-logs/panel-<day>.jsonl，
// 自动迁移时由 importLegacyPanelLogs 整目录导入）。按天切文件、超限轮转这些文件
// 概念随之上移为「一列 day + 一个总量软上限」。写路径仍然攒批（见 logStoreFlushBytes），
// 失败只记 s.err 不阻塞在处理的请求。
package panel

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

const (
	// logStoreDefaultDays / logStoreDefaultMaxMB 配置缺省（与 reqlog 的默认量级对齐）。
	logStoreDefaultDays  = 7
	logStoreDefaultMaxMB = 50
	// logStoreFlushBytes 攒到这么多字节才落库一次。
	// 不逐行 insert：日志是高频写入，每行一次写库会在长流式会话里明显吃掉 CPU。
	logStoreFlushBytes = 48 << 10
	// logStoreBackfillMax 回填上限（条）。环路只有 500/频道，回填更多也留不住。
	logStoreBackfillMax = 3000
	// logStoreMaxPending 写库失败时最多滞留的待重试行数（防内存无界增长）。
	logStoreMaxPending = 20000
)

// LogStore 面板运行日志的持久化存储（表 panel_logs）。
//
// db 为 nil 时调用方拿到 nil，退回纯内存行为；写库失败同样只记 s.err，
// 绝不阻塞主流程。
type LogStore struct {
	mu            sync.Mutex
	db            *store.Store
	retentionDays int
	maxBytes      int64 // 库内日志总量的软上限（超出后从最旧的自然日整批删）
	buf           []panelLogRow
	bufBytes      int64
	err           string // 最近一次写库失败原因（面板可提示）
	// disabled 运行期被关掉（logging.panel_log_enabled=false）：只停写不停读，
	// 已有的库内日志仍可导出/回填。真正「退回任务归档」由 Panel.ReconfigureLogs 把
	// 环上的 store 摘掉完成，这里只是最后一道防线（漏网的 Append 不会继续写库）。
	disabled bool
}

// NewLogStore 创建存储并做一次保留期清理。db 为 nil（纯内存模式）时返回 nil。
// retentionDays<=0 回退 7，maxMB<=0 回退 50。
func NewLogStore(db *store.Store, retentionDays, maxMB int) *LogStore {
	if db == nil {
		return nil
	}
	if retentionDays <= 0 {
		retentionDays = logStoreDefaultDays
	}
	if maxMB <= 0 {
		maxMB = logStoreDefaultMaxMB
	}
	s := &LogStore{db: db, retentionDays: retentionDays, maxBytes: int64(maxMB) << 20}
	s.cleanup()
	return s
}

// Dir 落库位置的人类可读描述（导出/启动日志里展示，测试也可断言）。
func (s *LogStore) Dir() string {
	if s == nil {
		return ""
	}
	return "数据库表 panel_logs"
}

// Error 最近一次写库失败原因（空 = 正常）。面板 logsHandler 透出。
func (s *LogStore) Error() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Append 追加一条日志；攒够 logStoreFlushBytes 就落库一次。
//
// 失败一律只记 s.err 不返回错误：日志系统不能反过来拖垮在处理的请求。
func (s *LogStore) Append(e LogEntry) {
	if s == nil || strings.TrimSpace(e.Text) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled { // 运行期被关掉：不再进库（环里仍有，页面照常能看）
		return
	}

	row := rowOfLog(e)
	s.buf = append(s.buf, row)
	s.bufBytes += int64(len(row.Text))
	if s.bufBytes >= logStoreFlushBytes {
		s.flushLocked()
	}
}

// flushLocked 把缓冲行写库。调用方必须持 s.mu。
//
// 写失败时把这些行放回缓冲头部等待下次重试（超过 logStoreMaxPending 才丢最旧的），
// 否则一次 DB 抖动就会静默吃掉整段事故现场。
func (s *LogStore) flushLocked() {
	if len(s.buf) == 0 {
		return
	}
	rows := s.buf
	s.buf = nil
	s.bufBytes = 0
	if err := store.Insert(s.db, rows...); err != nil {
		s.err = err.Error()
		pending := append(rows, s.buf...)
		if len(pending) > logStoreMaxPending {
			pending = pending[len(pending)-logStoreMaxPending:]
		}
		s.buf = pending
		for _, row := range s.buf {
			s.bufBytes += int64(len(row.Text))
		}
		return
	}
	s.err = ""
}

// Configure 热改保留期、总量上限与总开关（面板保存 logging.panel_log_* 后调用）。
//
// 为什么放在 LogStore 上而不是每次都重建：重建意味着换一个 store 指针，环上的
// 回填/导出/写库句柄全部要跟着换（还要处理切换瞬间的在途写入）；参数级的改动
// 就地更新字段最省事，也不需要动 Ring。**开关**这一项光靠它不够（关掉要求退回
// 任务归档，那是 Ring 的装配关系），所以真正的热切换入口是 Panel.ReconfigureLogs，
// 它会在需要时摘掉/挂回 store，本方法负责「已挂着的 store 按新参数收敛」。
//
// retentionDays<=0 回 7 天，maxBytes<=0 回 50 MiB；enabled=false 只停写不停读。
func (s *LogStore) Configure(retentionDays int, maxBytes int64, enabled bool) {
	if s == nil {
		return
	}
	if retentionDays <= 0 {
		retentionDays = logStoreDefaultDays
	}
	if maxBytes <= 0 {
		maxBytes = int64(logStoreDefaultMaxMB) << 20
	}
	s.mu.Lock()
	s.retentionDays = retentionDays
	s.maxBytes = maxBytes
	s.disabled = !enabled
	if enabled {
		s.flushLocked() // 重新打开前先把停写期间滞留在缓冲里的尾巴落库
	}
	s.mu.Unlock()
	if enabled {
		// 新保留期/上限立即收敛一次：否则「把 7 天改 1 天」要等下次重启才真删。
		s.cleanupWith(retentionDays, maxBytes)
	}
}

// Disabled 是否处于运行期停写状态。
func (s *LogStore) Disabled() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disabled
}

// Flush 立即落库（回填/导出前调用，保证库里有内存中尚未写入的尾巴）。
func (s *LogStore) Flush() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

// Close 落库并释放（进程退出时调用）。缓冲写库后即无句柄可关。
func (s *LogStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	return nil
}

// Backfill 返回最近的 limit 条（时间升序，limit<=0 回退上限）。
//
// 取的是「最近的一批」而不是重建全部历史：回填的目的是重启后还能看到最近的日志。
func (s *LogStore) Backfill(limit int) []LogEntry {
	if s == nil {
		return nil
	}
	if limit <= 0 || limit > logStoreBackfillMax {
		limit = logStoreBackfillMax
	}
	s.mu.Lock()
	// 先把内存里的尾巴写下去，否则回填会漏掉重启前最后几行。
	s.flushLocked()
	db := s.db
	s.mu.Unlock()

	var rows []panelLogRow
	if err := db.DB().Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		s.mu.Lock()
		s.err = err.Error()
		s.mu.Unlock()
		return nil
	}
	out := make([]LogEntry, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		entry := rows[i].entry()
		if strings.TrimSpace(entry.Text) == "" {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Export 把 [from, to] 区间内的日志打成 ZIP 写到 w，返回写入的文件数（自然日数）。
//
// 一天一个 zip 条目（panel-<day>.jsonl），行内再按 TS 精确裁剪——这样跨天区间不会
// 带进区间外的行；导出包形态与旧的按天文件一致（既有解析脚本不用改），行内容仍是原始 JSON 行。
func (s *LogStore) Export(w *os.File, from, to time.Time) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("日志落库未启用")
	}
	s.mu.Lock()
	s.flushLocked()
	db := s.db
	s.mu.Unlock()

	var rows []panelLogRow
	if err := db.DB().Where("ts >= ? AND ts <= ?", from, to).Order("id ASC").Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	zw := zip.NewWriter(w)
	written := 0
	day := ""
	var dst io.Writer
	for _, row := range rows {
		entry := row.entry()
		rowDay := row.Day
		if rowDay == "" {
			rowDay = entry.TS.In(cst.Loc).Format(logStoreDayLayout)
		}
		if rowDay != day {
			// 一律写 panel-<day>.jsonl（与旧的按天文件名一致），跨天自动换条目。
			w2, err := zw.Create("panel-" + rowDay + ".jsonl")
			if err != nil {
				_ = zw.Close()
				return written, err
			}
			dst = w2
			day = rowDay
			written++
		}
		line, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		if _, err := dst.Write(append(line, '\n')); err != nil {
			_ = zw.Close()
			return written, err
		}
	}
	if err := zw.Close(); err != nil {
		return written, err
	}
	return written, nil
}

// cleanup 清掉保留期外的日志，并执行总量上限（启动时跑一次）。
func (s *LogStore) cleanup() {
	if s == nil {
		return
	}
	s.mu.Lock()
	days, maxBytes := s.retentionDays, s.maxBytes
	s.mu.Unlock()
	s.cleanupWith(days, maxBytes)
}

// cleanupWith 按给定保留期/上限清理一次（参数现传，便于热改后立刻按新值收敛）。
func (s *LogStore) cleanupWith(retentionDays int, maxBytes int64) {
	if s == nil || s.db == nil {
		return
	}
	cutoff := cst.Now().AddDate(0, 0, -retentionDays).Format(logStoreDayLayout)
	s.db.DB().Where("day < ?", cutoff).Delete(&panelLogRow{})

	if maxBytes <= 0 {
		return
	}
	// 按自然日聚字节数，从最新往前累计；越界的日子整批删（保留期是硬线，这里是软线）。
	var days []struct {
		Day   string
		Bytes int64
	}
	if err := s.db.DB().Model(&panelLogRow{}).
		Select("day, SUM(LENGTH(text_value)) AS bytes").
		Group("day").Order("day ASC").Scan(&days).Error; err != nil {
		return
	}
	kept := int64(0)
	var drop []string
	for i := len(days) - 1; i >= 0; i-- {
		kept += days[i].Bytes
		if kept > maxBytes {
			drop = append(drop, days[i].Day)
		}
	}
	if len(drop) == 0 {
		return
	}
	s.db.DB().Where("day IN ?", drop).Delete(&panelLogRow{})
}

// ReconfigureLogs 运行期热切换面板「运行日志」落库（logging.panel_log_enabled /
// panel_log_retention_days / panel_log_max_mb 保存后立即生效，kill 不掉进程）。
//
// 为什么由面板自己做切换、而不是 main 里重建：切换要同时动两处装配关系——环上的
// store 指针，以及「关掉后由任务归档兜底」的那条路（SetTaskArchive）。散在 main
// 里做，热改路径与启动路径就会各写一遍，早晚走岔。
func (p *Panel) ReconfigureLogs(enabled bool, retentionDays, maxMB int) error {
	cur := p.logs.CurrentLogStore()
	if enabled {
		if cur != nil {
			// 已经挂着：只改参数，不重建——重建会丢掉缓冲里尚未落库的尾巴，还会
			// 把最近日志再回填一遍（环里出现重复行）。
			cur.Configure(retentionDays, int64(maxMB)<<20, true)
			return nil
		}
		next := NewLogStore(p.cfg.DB, retentionDays, maxMB)
		if next == nil {
			// 没连数据库：落库本来就无从谈起，保持任务归档/纯内存即可。
			return fmt.Errorf("日志落库未启用：未连接数据库")
		}
		return p.logs.SetLogStore(next)
	}
	if cur == nil {
		return nil
	}
	// 关掉：先把缓冲写下去再摘（最后几行不能静默消失），然后退回任务归档语义。
	_ = cur.Close()
	cur.Configure(retentionDays, int64(maxMB)<<20, false)
	if err := p.logs.SetLogStore(nil); err != nil {
		return err
	}
	return p.logs.SetTaskArchive(p.cfg.DB)
}
