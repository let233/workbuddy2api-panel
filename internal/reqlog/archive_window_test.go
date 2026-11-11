package reqlog

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// cst8 期望值都用固定 +08:00：夹具里的事件写的就是 +08:00，期望值若跟宿主
// 时区走，在 UTC 主机（CI runner）上会差 8 小时而假红——本机跑绿只是因为本机
// 恰好是 CST。
var cst8 = time.FixedZone("CST", 8*3600)

// newArchiveForTest 造一个只用于读路径的归档器（不做清理、不起写循环）。
func newArchiveForTest(t *testing.T) (*archiveStore, *store.Store) {
	t.Helper()
	db := storetest.Open(t)
	return testArchive(db, true, 30*24*time.Hour, 1<<20), db
}

// seedReqLogs 直接往归档表落事件（绕过写循环，读路径可确定性断言）。
func seedReqLogs(t *testing.T, db *store.Store, events ...Event) {
	t.Helper()
	rows := make([]reqLogRow, 0, len(events))
	for _, e := range events {
		row, err := rowOfEvent(e)
		if err != nil {
			t.Fatalf("rowOfEvent: %v", err)
		}
		rows = append(rows, row)
	}
	if err := store.Insert(db, rows...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func at(month time.Month, day, hour int) time.Time {
	return time.Date(2026, month, day, hour, 0, 0, 0, cst8)
}

// TestReadRespectsTimeWindow 带时间窗的查询只返回窗口内的事件（窗口条件下推到 SQL）。
func TestReadRespectsTimeWindow(t *testing.T) {
	w, db := newArchiveForTest(t)
	seedReqLogs(t, db,
		Event{Time: at(1, 1, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
		Event{Time: at(1, 2, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
		Event{Time: at(1, 3, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
	)
	rows, err := w.read(100, Filter{From: at(1, 2, 12)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("窗口内应恰有 1 条事件, got %d", len(rows))
	}
	if !rows[0].Time.Equal(at(1, 3, 10)) {
		t.Fatalf("应只返回窗口内事件, got %v", rows[0].Time)
	}
}

// TestReadStopsWhenLimitCollected 「最近 N 条」从最新往回收集，集满即停。
func TestReadStopsWhenLimitCollected(t *testing.T) {
	w, db := newArchiveForTest(t)
	seedReqLogs(t, db,
		Event{Time: at(1, 1, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
		Event{Time: at(1, 2, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
		Event{Time: at(1, 3, 8), Outcome: OutcomeSuccess, Status: 200, OK: true},
		Event{Time: at(1, 3, 9), Outcome: OutcomeSuccess, Status: 200, OK: true},
	)
	rows, err := w.read(2, Filter{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	// read 返回最新在前。
	if !rows[0].Time.Equal(at(1, 3, 9)) {
		t.Fatalf("第一条应为全局最新事件, got %v", rows[0].Time)
	}
	if !rows[1].Time.Equal(at(1, 3, 8)) {
		t.Fatalf("第二条应为次新事件, got %v", rows[1].Time)
	}
}

// TestReadFullScanUnchanged 无窗口、limit 大于总量时行为与全量扫描一致
// （乱序落库也能跨日排序、最新在前）。
func TestReadFullScanUnchanged(t *testing.T) {
	w, db := newArchiveForTest(t)
	seedReqLogs(t, db,
		Event{Time: at(1, 1, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
		Event{Time: at(1, 3, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
		Event{Time: at(1, 2, 10), Outcome: OutcomeSuccess, Status: 200, OK: true},
	)
	rows, err := w.read(100, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(rows))
	}
	want := []time.Time{at(1, 3, 10), at(1, 2, 10), at(1, 1, 10)}
	for i, wt := range want {
		if !rows[i].Time.Equal(wt) {
			t.Errorf("rows[%d]=%v want %v", i, rows[i].Time, wt)
		}
	}
}

// TestReadSkipsCorruptRow 单行坏数据不该让整页 500（旧实现里超长行会读废整份归档）。
func TestReadSkipsCorruptRow(t *testing.T) {
	w, db := newArchiveForTest(t)
	seedReqLogs(t, db, Event{Time: at(1, 1, 10), RequestID: "ok", Outcome: OutcomeSuccess, Status: 200, OK: true})
	if err := store.Insert(db, reqLogRow{TS: at(1, 1, 11), Day: "2026-01-01", Payload: "{not json"}); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	rows, err := w.read(10, Filter{})
	if err != nil {
		t.Fatalf("坏行不应让读取报错: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "ok" {
		t.Fatalf("应跳过坏行并返回好行, got %+v", rows)
	}
}
