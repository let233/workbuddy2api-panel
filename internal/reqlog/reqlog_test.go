package reqlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

func TestRecorderMetricsAndRecent(t *testing.T) {
	r := New(Config{})
	r.Begin()
	r.Record(Event{RequestID: "ok", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 10})
	r.Begin()
	r.Record(Event{RequestID: "bad", Status: 429, OK: false, Outcome: OutcomeHTTPError, DurationMs: 30})

	s := r.Snapshot()
	if s.Completed != 2 || s.InFlight != 0 || s.Succeeded != 1 || s.Failed != 1 {
		t.Fatalf("counts = %+v", s)
	}
	if s.SuccessRate != 50 || s.HTTPSuccessRate != 50 || s.AvgDurationMs != 20 {
		t.Fatalf("rates = success:%v http:%v avg:%v", s.SuccessRate, s.HTTPSuccessRate, s.AvgDurationMs)
	}
	if len(s.Recent) != 2 || s.Recent[0].RequestID != "bad" || s.Recent[1].RequestID != "ok" {
		t.Fatalf("recent = %+v, want newest first", s.Recent)
	}
	if s.Archive.Enabled {
		t.Fatalf("没有 DB 时归档应为关闭: %+v", s.Archive)
	}
}

func TestArchiveReadAndFilter(t *testing.T) {
	db := storetest.Open(t)
	r := New(Config{DB: db, Enabled: true, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	for i := 0; i < 12; i++ {
		r.Record(Event{
			Time:       base.Add(time.Duration(i) * time.Second),
			RequestID:  "multi-" + string(rune('a'+i)),
			Model:      "glm-5.3",
			Account:    "账号(uid8)",
			Status:     200,
			OK:         true,
			Outcome:    OutcomeSuccess,
			DurationMs: int64(i + 1),
		})
	}
	r.Record(Event{
		Time:       base.Add(20 * time.Second),
		RequestID:  "other",
		Model:      "other-model",
		Status:     500,
		Outcome:    OutcomeHTTPError,
		DurationMs: 99,
	})
	r.Close()

	stats := r.Snapshot().Archive
	if !stats.Enabled || stats.Rows != 13 || stats.Bytes == 0 || stats.DroppedWrites != 0 || stats.Target != reqLogTableName {
		t.Fatalf("archive stats = %+v", stats)
	}
	rows, err := r.ReadArchive(5, Filter{Model: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0].RequestID != "multi-l" || rows[4].RequestID != "multi-h" {
		t.Fatalf("filtered rows = %+v", rows)
	}
}

// 来源字段随事件落库并可被 client_ip / user_agent 过滤（包含匹配、大小写不敏感）。
func TestArchiveFilterByClientInfo(t *testing.T) {
	db := storetest.Open(t)
	r := New(Config{DB: db, Enabled: true, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	rows := []Event{
		{RequestID: "a", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "203.0.113.7", UserAgent: "python-requests/2.31.0"},
		{RequestID: "b", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess,
			ClientIP: "198.51.100.4", UserAgent: "Mozilla/5.0 Chrome/120.0.0.0"},
		{RequestID: "c", Model: "glm-5.3", Status: 200, OK: true, Outcome: OutcomeSuccess},
	}
	for i, e := range rows {
		e.Time = base.Add(time.Duration(i) * time.Second)
		r.Record(e)
	}
	r.Close()

	got, err := r.ReadArchive(10, Filter{ClientIP: "203.0.113."})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "a" {
		t.Fatalf("client ip filter = %+v", got)
	}
	if got[0].UserAgent != "python-requests/2.31.0" {
		t.Fatalf("user agent not persisted: %+v", got[0])
	}

	// UA 片段过滤大小写不敏感（面板搜索框直接传用户输入）。
	got, err = r.ReadArchive(10, Filter{UserAgent: "chrome/120"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RequestID != "b" {
		t.Fatalf("ua filter = %+v", got)
	}

	// 来源为空的旧事件不该被非空来源条件误命中。
	got, err = r.ReadArchive(10, Filter{ClientIP: "10.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unmatched ip filter = %+v", got)
	}
}

// 时间区间过滤（「今天」/「自定义」）：闭区间，任一侧为零值即该侧不设界。
func TestArchiveFilterByTimeRange(t *testing.T) {
	db := storetest.Open(t)
	r := New(Config{DB: db, Enabled: true, MaxBytes: 1 << 20, RetentionDays: 30})
	base := time.Now().Add(-72 * time.Hour).Truncate(time.Minute)
	ids := []string{"d3", "d2", "d1", "now"}
	for i, id := range ids {
		r.Record(Event{
			Time: base.Add(time.Duration(i) * 24 * time.Hour), RequestID: id,
			Status: 200, OK: true, Outcome: OutcomeSuccess,
		})
	}
	r.Close()

	rows, err := r.ReadArchive(10, Filter{From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(rows))
	for _, e := range rows {
		got = append(got, e.RequestID)
	}
	// 倒序返回，闭区间命中 d2、d1。
	if len(got) != 2 || got[0] != "d1" || got[1] != "d2" {
		t.Fatalf("时间区间过滤 = %v, want [d1 d2]", got)
	}

	onlyFrom, err := r.ReadArchive(10, Filter{From: base.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyFrom) != 2 || onlyFrom[0].RequestID != "now" {
		t.Fatalf("仅 From 过滤 = %+v, want 2 条（d1、now）", onlyFrom)
	}

	onlyTo, err := r.ReadArchive(10, Filter{To: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyTo) != 1 || onlyTo[0].RequestID != "d3" {
		t.Fatalf("仅 To 过滤 = %+v, want 仅 d3", onlyTo)
	}

	// 时间区间与其它条件是与关系。
	combined, err := r.ReadArchive(10, Filter{
		From: base.Add(24 * time.Hour), To: base.Add(48 * time.Hour), Outcome: OutcomeHTTPError,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != 0 {
		t.Fatalf("区间+结果组合应为空（这几条都是成功）: %+v", combined)
	}
}

func TestArchiveQueueDropCounter(t *testing.T) {
	w := testArchive(nil, true, 0, 0)
	w.ch = make(chan Event, 1)
	w.ch <- Event{RequestID: "occupied"}
	w.enqueue(Event{RequestID: "dropped"})
	if got := w.dropped.Load(); got != 1 {
		t.Fatalf("dropped=%d want 1", got)
	}
}

// 容量上限按自然日整日淘汰，且最新一天永不淘汰（那是正在写的归档）。
func TestArchivePruneHonorsSize(t *testing.T) {
	db := storetest.Open(t)
	big := strings.Repeat("x", 200)
	for _, day := range []string{"2026-09-20", "2026-09-21", "2026-09-22"} {
		rows := []reqLogRow{
			{TS: time.Now(), Day: day, Outcome: OutcomeSuccess, Payload: big},
			{TS: time.Now(), Day: day, Outcome: OutcomeSuccess, Payload: big},
		}
		if err := store.Insert(db, rows...); err != nil {
			t.Fatal(err)
		}
	}
	w := testArchive(db, true, 30*24*time.Hour, 600)
	w.prune()

	left, err := store.LoadAll[reqLogRow](db)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("只剩最新一天的两行, got %d 行", len(left))
	}
	for _, row := range left {
		if row.Day != "2026-09-22" {
			t.Fatalf("残留了非最新一天的行: %+v", row.Day)
		}
	}
}

// 过期清理：超过保留天数的行全部删掉。
func TestArchivePruneDropsExpired(t *testing.T) {
	db := storetest.Open(t)
	old := time.Now().AddDate(0, 0, -30)
	if err := store.Insert(db,
		reqLogRow{TS: old, Day: old.Format(reqLogDayLayout), Payload: "{}"},
		reqLogRow{TS: time.Now(), Day: time.Now().Format(reqLogDayLayout), Payload: "{}"},
	); err != nil {
		t.Fatal(err)
	}
	w := testArchive(db, true, 7*24*time.Hour, 0)
	w.prune()
	if got, err := store.Count[reqLogRow](db); err != nil || got != 1 {
		t.Fatalf("过期行应被删掉, 剩 %d 行 (err=%v)", got, err)
	}
}

// importLegacyRequestLogs 把旧 JSONL 目录导进表：坏行只丢自己，好行照常入表，
// 导入完成后旧目录改名（.imported），不会再次导入。
func TestLegacyArchiveImported(t *testing.T) {
	old := eventLineMaxBytes
	eventLineMaxBytes = 4 << 10 // 测试用小上限，免得造 16 MB 文件
	defer func() { eventLineMaxBytes = old }()

	dir := t.TempDir()
	src := filepath.Join(dir, legacyReqLogDir)
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 3, 10, 0, 0, 0, time.Local)
	var b strings.Builder
	appendEvent := func(e Event) {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		b.Write(raw)
		b.WriteString(string(rune(10)))
	}
	appendEvent(Event{Time: base, RequestID: "before", Status: 200, OK: true, Outcome: OutcomeSuccess})
	// 模拟「开了上下文归档 + 请求带图」：Context 是 8 KB 的 base64 串，整行远超上限。
	appendEvent(Event{
		Time: base.Add(time.Second), RequestID: "huge", Status: 200, OK: true, Outcome: OutcomeSuccess,
		Context: json.RawMessage(`"` + strings.Repeat("A", 8<<10) + `"`),
	})
	appendEvent(Event{Time: base.Add(2 * time.Second), RequestID: "after", Status: 200, OK: true, Outcome: OutcomeSuccess})
	if err := os.WriteFile(filepath.Join(src, "requests-2026-11-03.jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	db := storetest.OpenDir(t, dir)
	store.ImportLegacy(db, dir)

	rows, err := store.LoadAll[reqLogRow](db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("超长行只丢自己, 应导入 2 行, got %d", len(rows))
	}
	if _, err := os.Stat(src + ".imported"); err != nil {
		t.Fatalf("旧目录应改名为 .imported: %v", err)
	}
}
