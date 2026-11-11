// Package credithist 积分历史：把「每次真实查到的余额」与上一次比对，变动即留痕。
//
// 为什么需要：签到 / 活跃上报 / 猫猫旅行 / 成长任务都会让余额增加，但上游只在少数
// 渠道留下可读金额；面板「用量」折算的是**消耗**，此前没有任何逐账号的余额时间线。
// 把余额当观测量逐次比对，就能覆盖全部获取渠道（做法与 workbuddy-manager 的
// 「积分变动流水」一致）。
//
// 与参考实现的有意差异：参考实现只记增加（减少属"消耗"，已有用量视图覆盖）。本
// 实现两个方向都留痕（Delta 带符号），因为本面板缺的正是"什么时候变成多少"这条
// 时间线；展示层用正负号区分获取与消耗。
//
// 首次见到某个 uid 只建立基线、不记流水——否则会把历史余额误报成"刚获得"。
//
// 存储：数据库两张表（gorm 自动迁移）——credit_history 流水（只追加，超出上限
// 删最旧）与 credit_baselines 余额基线（uid 主键，重启后不把旧余额当新增）。
package credithist

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// maxDelta 单次变动的留痕上限（对齐参考实现的 _MAX_DELTA）：超过它基本可断定是
// 上游脏数据或口径切换（如企业版"不限量"哨兵值），只更新基线、不留痕，避免一条
// 假流水污染整段历史。
const maxDelta = 1_000_000

// defaultMaxEntries 流水条数上限（超出丢最旧）：按每日数十条估计可留数月。
const defaultMaxEntries = 2000

// Entry 一条积分变动流水。Before/After 是变动前后余额，Delta = After - Before
// （正 = 获取，负 = 消耗）。
type Entry struct {
	Time   time.Time `json:"time"`
	UID    string    `json:"uid"`
	Delta  int64     `json:"delta"`
	Before int64     `json:"before"`
	After  int64     `json:"after"`
}

// fileVersion 旧 credit-history.json 的格式版本（仅用于一次性导入旧数据）。
const fileVersion = 1

// diskFile 旧落盘结构（仅用于一次性导入旧数据）。
type diskFile struct {
	Version  int              `json:"version"`
	Saved    string           `json:"saved,omitempty"`
	Snapshot map[string]int64 `json:"snapshot"`
	Entries  []Entry          `json:"entries"`
}

// entryRow 流水表的一行。ID 自增即写入顺序，Read 按 ID 倒序取最近若干条。
type entryRow struct {
	ID     uint      `gorm:"column:id;primaryKey;autoIncrement"`
	Time   time.Time `gorm:"column:ts;index"`
	UID    string    `gorm:"column:uid;size:191;index"`
	Delta  int64     `gorm:"column:delta"`
	Before int64     `gorm:"column:before"`
	After  int64     `gorm:"column:after"`
}

// TableName 固定表名。
func (entryRow) TableName() string { return "credit_history" }

// baselineRow 余额基线表的一行（每个 uid 一条）。
type baselineRow struct {
	UID       string    `gorm:"column:uid;primaryKey;size:191"`
	Credits   int64     `gorm:"column:credits"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName 固定表名。
func (baselineRow) TableName() string { return "credit_baselines" }

func init() {
	store.RegisterModels(&entryRow{}, &baselineRow{})
	store.RegisterLegacy(store.LegacyImport{Key: "credithist", Name: "credit-history.json", Fn: importLegacy})
}

// importLegacy 把旧 data/credit-history.json 搬进 credit_history / credit_baselines。
func importLegacy(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, "credit-history.json")
	if !ok {
		return nil
	}
	var f diskFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	if len(f.Snapshot) > 0 {
		rows := make([]baselineRow, 0, len(f.Snapshot))
		for uid, credits := range f.Snapshot {
			rows = append(rows, baselineRow{UID: uid, Credits: credits})
		}
		if err := store.ReplaceAll(s, rows); err != nil {
			return err
		}
	}
	if len(f.Entries) > 0 {
		rows := make([]entryRow, 0, len(f.Entries))
		for _, e := range f.Entries {
			rows = append(rows, entryRow{Time: e.Time, UID: e.UID, Delta: e.Delta, Before: e.Before, After: e.After})
		}
		if err := store.Insert(s, rows...); err != nil {
			return err
		}
	}
	log.Printf("[credithist] 已从旧文件 credit-history.json 导入 %d 条流水 / %d 个基线", len(f.Entries), len(f.Snapshot))
	return nil
}

// Ledger 并发安全的积分账本。db 为 nil = 纯内存（不落盘）。
type Ledger struct {
	mu       sync.Mutex
	db       *store.Store
	max      int
	snapshot map[string]int64
	entries  []Entry // 时间升序
}

// New 创建账本；db 非 nil 时从库里加载既有基线与流水。加载失败只记日志、
// 从空账本开始——丢历史好过让面板起不来。maxEntries <= 0 回落默认上限。
func New(db *store.Store, maxEntries int) *Ledger {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	l := &Ledger{db: db, max: maxEntries, snapshot: map[string]int64{}}
	l.load()
	return l
}

// load 读回基线与流水（空库 = 空账本，不是错误）。
func (l *Ledger) load() {
	if l.db == nil {
		return
	}
	baselines, err := store.LoadAll[baselineRow](l.db)
	if err != nil {
		log.Printf("[credithist] 读取余额基线失败（从空账本开始）: %v", err)
		return
	}
	for _, b := range baselines {
		l.snapshot[b.UID] = b.Credits
	}
	// 只取最近 max 条（升序），与旧实现的"文件里最多 max 条"等价。
	var rows []entryRow
	if err := l.db.DB().Order("id DESC").Limit(l.max).Find(&rows).Error; err != nil {
		log.Printf("[credithist] 读取积分流水失败（从空账本开始）: %v", err)
		return
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		l.entries = append(l.entries, Entry{Time: r.Time, UID: r.UID, Delta: r.Delta, Before: r.Before, After: r.After})
	}
	log.Printf("[credithist] 已恢复 %d 条积分流水 / %d 个余额基线（数据库）", len(l.entries), len(l.snapshot))
}

// Observe 记录一次真实查到的余额（uid + 余额绝对值）。
//
// 语义：
//   - 首次见到该 uid：只建基线，不记流水；
//   - 余额与基线相同：直接返回（不写库）；
//   - 余额变化且 |Delta| <= maxDelta：追加一条流水；
//   - 余额变化但 |Delta| > maxDelta：只更新基线（脏数据不留痕）。
//
// 同步写库：留痕频率是"每次余额刷新"级（分钟级），单行 upsert + 单行 insert，
// 无需防抖队列。
func (l *Ledger) Observe(uid string, credits int64) {
	// 纯空白 uid 与空 uid 一样无效：放进去只会留下一个面板永远筛不到的幽灵基线
	// （uid 过滤参数为空 = 不过滤，所以这种条目既无用也删不掉）。
	if l == nil || strings.TrimSpace(uid) == "" {
		return
	}
	l.mu.Lock()
	prev, seen := l.snapshot[uid]
	if seen && prev == credits {
		l.mu.Unlock()
		return
	}
	l.snapshot[uid] = credits
	l.persistBaseline(uid, credits)
	if seen {
		if delta := credits - prev; delta >= -maxDelta && delta <= maxDelta {
			e := Entry{Time: time.Now(), UID: uid, Delta: delta, Before: prev, After: credits}
			l.entries = append(l.entries, e)
			if len(l.entries) > l.max {
				// 丢最旧：copy 到新底层数组，避免 append 复用旧数组导致内存不释放。
				l.entries = append([]Entry(nil), l.entries[len(l.entries)-l.max:]...)
			}
			l.persistEntry(e)
		}
	}
	l.mu.Unlock()
}

// Read 返回最近 limit 条流水（新的在前）；limit <= 0 = 全部。
func (l *Ledger) Read(limit int) []Entry {
	if l == nil {
		return []Entry{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.entries)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Entry, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

// persistBaseline 写基线（失败只记日志：留痕是旁路观测，不能反过来影响余额刷新主流程）。
func (l *Ledger) persistBaseline(uid string, credits int64) {
	if l.db == nil {
		return
	}
	if err := store.Upsert(l.db, []baselineRow{{UID: uid, Credits: credits}}, []string{"uid"}, []string{"credits", "updated_at"}); err != nil {
		log.Printf("[credithist] 写余额基线失败: %v", err)
	}
}

// persistEntry 追加一条流水，并按上限裁剪最旧的（避免表无限增长）。
func (l *Ledger) persistEntry(e Entry) {
	if l.db == nil {
		return
	}
	row := entryRow{Time: e.Time, UID: e.UID, Delta: e.Delta, Before: e.Before, After: e.After}
	if err := store.Insert(l.db, row); err != nil {
		log.Printf("[credithist] 写积分流水失败: %v", err)
		return
	}
	if err := l.trim(); err != nil {
		log.Printf("[credithist] 裁剪旧流水失败: %v", err)
	}
}

// trim 删掉超出上限的最旧流水。
//
// 不用 DELETE ... WHERE id NOT IN (SELECT ... LIMIT)：MySQL 不允许在 DELETE 的子查询
// 里引用同一张表。改成先把要删的 id 查出来再按 id 删，三种驱动一致。
func (l *Ledger) trim() error {
	var ids []uint
	if err := l.db.DB().Model(&entryRow{}).Order("id DESC").Offset(l.max).Limit(1000).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return l.db.DB().Where("id IN ?", ids).Delete(&entryRow{}).Error
}
