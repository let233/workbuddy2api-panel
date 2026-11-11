// setconfig_test.go 归档配置热改：开关 / 保留天数 / 容量上限。
//
// 关键契约：Enabled=false 停止写库，但内存指标照常（与 New 语义一致）——
// 归档是可选的旁路，关掉不影响 /status 观测。
package reqlog

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// countRows 直接数归档表行数（读路径在归档关闭时返回空，故测试绕开 Recorder）。
func countRows(t *testing.T, db *store.Store) int64 {
	t.Helper()
	var n int64
	if err := db.DB().Model(&reqLogRow{}).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// waitRows 等到归档表行数达到 want（批刷是异步的），超时返回当前行数。
func waitRows(t *testing.T, db *store.Store, want int64, within time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if n := countRows(t, db); n >= want {
			return n
		}
		if time.Now().After(deadline) {
			return countRows(t, db)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSetConfigDisablesArchive 热关归档：Archive.Enabled 立刻变 false、不再写库，
// 但内存指标（Completed/Recent）照常累计；重新打开后恢复写库。
func TestSetConfigDisablesArchive(t *testing.T) {
	db := storetest.Open(t)
	r := New(Config{DB: db, Enabled: true, RetentionDays: 7, MaxBytes: 1 << 20})

	// 灌满一批触发立即落库（reqLogFlushBatch 行攒够就写）。
	for i := 0; i < reqLogFlushBatch; i++ {
		r.Record(Event{Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	before := waitRows(t, db, int64(reqLogFlushBatch), 5*time.Second)
	if before == 0 {
		t.Fatal("前置：归档应已写库")
	}

	r.SetConfig(Config{DB: db, Enabled: false, RetentionDays: 7, MaxBytes: 1 << 20})
	if r.Snapshot().Archive.Enabled {
		t.Fatal("SetConfig(Enabled=false) 后 Archive.Enabled 应为 false")
	}

	completed := r.Snapshot().Completed
	r.Record(Event{Status: 500, OK: false, Outcome: OutcomeHTTPError})
	if got := r.Snapshot().Completed; got != completed+1 {
		t.Fatalf("内存指标必须照常累计：completed %d → %d", completed, got)
	}
	if n := len(r.Snapshot().Recent); n == 0 {
		t.Fatal("关掉归档后 Recent 仍应保留（内存指标与归档开关无关）")
	}

	time.Sleep(300 * time.Millisecond) // 给写循环（1s flush ticker）足够机会
	if got := countRows(t, db); got != before {
		t.Fatalf("关掉归档后不该再写库：%d → %d 行", before, got)
	}
	// 归档关闭时读路径返回空（面板不展示已停用的归档）。
	if rows, err := r.ReadArchive(10, Filter{}); err != nil || len(rows) != 0 {
		t.Fatalf("关闭时读归档应返回空，got %d 行 err=%v", len(rows), err)
	}

	// 重新打开：继续写库。
	r.SetConfig(Config{DB: db, Enabled: true, RetentionDays: 7, MaxBytes: 1 << 20})
	if !r.Snapshot().Archive.Enabled {
		t.Fatal("重新打开后 Archive.Enabled 应为 true")
	}
	for i := 0; i < reqLogFlushBatch; i++ {
		r.Record(Event{Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	if got := waitRows(t, db, before+int64(reqLogFlushBatch), 5*time.Second); got != before+int64(reqLogFlushBatch) {
		t.Fatalf("重新打开后应继续写库：%d → %d，want %d", before, got, before+int64(reqLogFlushBatch))
	}
	r.Close()
}

// TestSetConfigRetentionAndMaxBytesHot 保留天数/容量上限每轮重读：改完下一个清理
// 周期（这里直接调 prune）就按新口径淘汰，不必重启进程。
func TestSetConfigRetentionAndMaxBytesHot(t *testing.T) {
	db := storetest.Open(t)
	r := New(Config{DB: db, Enabled: true, RetentionDays: 30, MaxBytes: 1 << 20})

	// 直接落库（绕过异步写循环，断言才确定）：5 条 48 小时前的事件。
	old := time.Now().Add(-48 * time.Hour)
	rows := make([]reqLogRow, 0, 5)
	for i := 0; i < 5; i++ {
		row, err := rowOfEvent(Event{Time: old, Status: 200, OK: true, Outcome: OutcomeSuccess})
		if err != nil {
			t.Fatalf("rowOfEvent: %v", err)
		}
		rows = append(rows, row)
	}
	if err := store.Insert(db, rows...); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db); n != 5 {
		t.Fatalf("前置：应落库 5 行，got %d", n)
	}

	// 保留 30 天：48 小时前的行不该被删。
	r.archive.prune()
	if n := countRows(t, db); n != 5 {
		t.Fatalf("保留 30 天时不该删旧行，got %d 行", n)
	}

	// 热改保留 1 天 → 48 小时前的行立刻被淘汰（不必等 1 分钟的清理周期）。
	r.SetConfig(Config{DB: db, Enabled: true, RetentionDays: 1, MaxBytes: 1 << 20})
	r.archive.prune()
	if n := countRows(t, db); n != 0 {
		t.Fatalf("保留 1 天后旧行应被清理，got %d 行", n)
	}
	r.Close()
}
