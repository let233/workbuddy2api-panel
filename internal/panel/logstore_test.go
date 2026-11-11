// logstore_test.go 运行日志落库 / 回填 / 导出的回归钉。
//
// 这些用例守的是「事故现场还在」这件事：内存环每频道 500 条、重启即归零，
// 落库（表 panel_logs）是唯一能让「昨天下午那波 429」还可查的东西。
package panel

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// dayTS 指定自然日的某个时刻（CST）。
func dayTS(day string, hour int) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15", day+" "+string(rune('0'+hour/10))+string(rune('0'+hour%10)), cst.Loc)
	if err != nil {
		panic(err)
	}
	return t
}

// logRows 读出表里的全部行（按 id 升序，即写入顺序）。
func logRows(t *testing.T, db *store.Store) []panelLogRow {
	t.Helper()
	rows, err := store.LoadAll[panelLogRow](db)
	if err != nil {
		t.Fatalf("读 panel_logs: %v", err)
	}
	return rows
}

// TestLogStorePersistsPerDayRows 跨天按 CST 自然日分列，且在库里可读。
func TestLogStorePersistsPerDayRows(t *testing.T) {
	db := storetest.Open(t)
	s := NewLogStore(db, 7, 0)
	if s == nil {
		t.Fatal("NewLogStore 返回 nil")
	}
	defer s.Close()
	s.Append(LogEntry{TS: dayTS("2026-11-08", 23), Ch: ChChat, Text: "row-a"})
	s.Append(LogEntry{TS: dayTS("2026-11-09", 1), Ch: ChSys, Text: "row-b"})
	s.Flush()

	rows := logRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("落库行数 = %d want 2（%+v）", len(rows), rows)
	}
	want := map[string]string{"row-a": "2026-11-08", "row-b": "2026-11-09"}
	for _, row := range rows {
		if day, ok := want[row.Text]; !ok || row.Day != day {
			t.Fatalf("行 %q 的 day = %q want %q（按自然日分列是保留期与导出的口径）", row.Text, row.Day, want[row.Text])
		}
	}
	if rows[1].Ch != string(ChSys) {
		t.Fatalf("频道未落库: %+v", rows[1])
	}
}

// TestLogStoreByteCapDropsOldestDays 总量超上限时从最旧的整天删，最新一天永不动。
//
// 反向代价：不设上限等于库只增不减；而删掉「正在写的那一天」比超量更难解释。
func TestLogStoreByteCapDropsOldestDays(t *testing.T) {
	db := storetest.Open(t)
	big := strings.Repeat("x", 200)
	if err := store.Insert(db,
		panelLogRow{TS: dayTS("2026-11-07", 12), Day: "2026-11-07", Ch: string(ChSys), Text: big},
		panelLogRow{TS: dayTS("2026-11-09", 12), Day: "2026-11-09", Ch: string(ChSys), Text: big},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// 上限 200 字节：两天各 ~200 字节，只留最新一天。
	s := &LogStore{db: db, retentionDays: 7, maxBytes: 200}
	s.cleanup()

	rows := logRows(t, db)
	if len(rows) != 1 || rows[0].Day != "2026-11-09" {
		t.Fatalf("超量清理后 = %+v want 只留 2026-11-09（丢日志=丢事故现场，但更旧的可回收）", rows)
	}
}

// TestLogStoreBackfillNewestFirst 回填按时间升序返回最近的 limit 条。
func TestLogStoreBackfillNewestFirst(t *testing.T) {
	db := storetest.Open(t)
	s := NewLogStore(db, 7, 0)
	for i := 0; i < 5; i++ {
		s.Append(LogEntry{TS: dayTS("2026-11-08", 10).Add(time.Duration(i) * time.Minute), Ch: ChSys, Text: "old-" + string(rune('0'+i))})
	}
	for i := 0; i < 3; i++ {
		s.Append(LogEntry{TS: dayTS("2026-11-09", 10).Add(time.Duration(i) * time.Minute), Ch: ChSys, Text: "new-" + string(rune('0'+i))})
	}
	s.Close()

	// 重新打开（模拟重启），回填只看库里已有的行。
	fresh := NewLogStore(db, 7, 0)
	got := fresh.Backfill(4)
	if len(got) != 4 {
		t.Fatalf("回填条数 = %d want 4", len(got))
	}
	if got[0].Text != "old-4" || got[3].Text != "new-2" {
		t.Fatalf("回填顺序/内容 = %v ~ %v want old-4 ~ new-2（升序取最近）", got[0].Text, got[3].Text)
	}
	// 升序必须是严格成立的：环里的顺序就是面板展示顺序。
	for i := 1; i < len(got); i++ {
		if got[i].TS.Before(got[i-1].TS) {
			t.Fatalf("回填未按时间升序: %v 在 %v 之前", got[i].TS, got[i-1].TS)
		}
	}
}

// TestLogStoreExportFiltersByRange 导出按区间逐行裁剪，且只含区间内的行。
func TestLogStoreExportFiltersByRange(t *testing.T) {
	db := storetest.Open(t)
	s := NewLogStore(db, 7, 0)
	defer s.Close()
	base := dayTS("2026-11-09", 8)
	for i := 0; i < 6; i++ {
		s.Append(LogEntry{TS: base.Add(time.Duration(i) * time.Hour), Ch: ChSys, Text: "line-" + string(rune('0'+i))})
	}
	s.Append(LogEntry{TS: dayTS("2026-11-07", 8), Ch: ChSys, Text: "too-old"})

	tmp, err := os.CreateTemp(t.TempDir(), "export-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	files, err := s.Export(tmp, base.Add(time.Hour), base.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if files != 1 {
		t.Fatalf("导出文件数 = %d want 1（只该含 11-09 那天的一包）", files)
	}
	entries, err := zip.OpenReader(tmp.Name())
	if err != nil {
		t.Fatalf("打开 zip: %v", err)
	}
	defer entries.Close()
	var lines []string
	for _, f := range entries.File {
		if f.Name != "panel-2026-11-09.jsonl" {
			t.Fatalf("zip 条目名 = %q want panel-2026-11-09.jsonl", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var rec struct{ Text string }
		dec := json.NewDecoder(rc)
		for dec.More() {
			if err := dec.Decode(&rec); err != nil {
				break
			}
			lines = append(lines, rec.Text)
		}
		rc.Close()
	}
	want := []string{"line-1", "line-2", "line-3"}
	if len(lines) != len(want) {
		t.Fatalf("区间内行数 = %d want %d（%v）", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("第 %d 行 = %q want %q", i, lines[i], want[i])
		}
	}
}

// TestLogStoreCleanupDropsExpired 保留期外的行必须清掉（否则库只增不减）。
func TestLogStoreCleanupDropsExpired(t *testing.T) {
	db := storetest.Open(t)
	old := cst.Now().AddDate(0, 0, -30)
	today := cst.Now()
	if err := store.Insert(db,
		panelLogRow{TS: old, Day: old.In(cst.Loc).Format(logStoreDayLayout), Ch: string(ChSys), Text: "expired"},
		panelLogRow{TS: today, Day: today.In(cst.Loc).Format(logStoreDayLayout), Ch: string(ChSys), Text: "keep"},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewLogStore(db, 7, 0)
	if s == nil {
		t.Fatal("NewLogStore 返回 nil")
	}
	defer s.Close()
	rows := logRows(t, db)
	if len(rows) != 1 || rows[0].Text != "keep" {
		t.Fatalf("保留期清理后 = %+v want 只剩今天的行", rows)
	}
}

// TestLogStoreLegacyDirImported 旧的 panel-logs/*.jsonl 目录必须整批导入。
func TestLogStoreLegacyDirImported(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, legacyPanelLogsDir)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"ts":"2026-11-09T10:00:00+08:00","ch":"sys","seq":1,"text":"legacy-1"}
{"ts":"2026-11-09T10:01:00+08:00","ch":"sys","seq":2,"text":"legacy-2"}
{"broken line
`
	if err := os.WriteFile(filepath.Join(logDir, "panel-2026-11-09.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	db := storetest.OpenDir(t, dir)
	store.ImportLegacy(db, dir)

	if rows := logRows(t, db); len(rows) != 2 {
		t.Fatalf("导入行数 = %d want 2（坏行跳过，好行一条不丢）", len(rows))
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Fatalf("导入后旧目录应改名为 %s.imported: %v", legacyPanelLogsDir, err)
	}
	if s := NewLogStore(db, 7, 0); len(s.Backfill(10)) != 2 {
		t.Fatal("导入的行必须能被回填读到")
	}
}

// TestRingBackfillsFromLogStore 挂上 store 后，重启的环必须能看到旧行。
//
// 这是「重启后还能查昨天」的端到端断言：只测 LogStore 自己的 Backfill 不够，
// 真正决定面板有没有内容的一环接不接得上。
func TestRingBackfillsFromLogStore(t *testing.T) {
	db := storetest.Open(t)
	store := NewLogStore(db, 7, 0)
	store.Append(LogEntry{TS: cst.Now().Add(-time.Hour), Ch: ChChat, Text: "| #1 | old row"})
	store.Close()

	// 模拟重启：新 store + 新环，只靠库里的行回填。
	store2 := NewLogStore(db, 7, 0)
	defer store2.Close()
	r := NewRing(10)
	if err := r.SetLogStore(store2); err != nil {
		t.Fatalf("SetLogStore: %v", err)
	}
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].Text != "| #1 | old row" {
		t.Fatalf("回填后的环 = %+v want 一条旧 chat 行", snap)
	}
	// 新写入必须照常入环 + 落库（回填不能把写路径挂掉）。
	r.Write([]byte("2026-11-09 10:00:00 fresh line\n"))
	store2.Flush()
	if got := r.Snapshot(); len(got) != 2 {
		t.Fatalf("新写入后环条数 = %d want 2", len(got))
	}
	found := false
	for _, row := range logRows(t, db) {
		// Ring.Write 只剥 log 包形态的时间戳（斜杠分隔），所以按包含判定。
		if strings.Contains(row.Text, "fresh line") {
			found = true
		}
	}
	if !found {
		t.Fatal("新写入未落库（回填不能把写路径挂掉）")
	}
}
