package panel

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// panelLogRow 面板运行日志的一行（表 panel_logs）。
//
// 旧实现是数据目录下按天一个 JSONL 文件（panel-<day>.jsonl，超限再 .2/.3 轮转）。
// 换成表后「按天」变成一列 day，「轮转」这层文件概念整体消失——单文件上限改成
// 库内日志总量的软上限（见 LogStore.cleanup）。
type panelLogRow struct {
	ID   uint      `gorm:"column:id;primaryKey;autoIncrement"`
	TS   time.Time `gorm:"column:ts;index"`
	Day  string    `gorm:"column:day;size:16;index"`
	Ch   string    `gorm:"column:ch;size:16"`
	Seq  uint64    `gorm:"column:seq"`
	Text string    `gorm:"column:text_value;type:text"`
}

func (panelLogRow) TableName() string { return "panel_logs" }

// rowOfLog / entry 行 ↔ LogEntry 转换（落库列与内存结构一一口径）。
func rowOfLog(e LogEntry) panelLogRow {
	return panelLogRow{
		TS:   e.TS,
		Day:  e.TS.In(cst.Loc).Format(logStoreDayLayout),
		Ch:   e.Ch,
		Seq:  e.Seq,
		Text: e.Text,
	}
}

func (row panelLogRow) entry() LogEntry {
	return LogEntry{TS: row.TS, Ch: row.Ch, Seq: row.Seq, Text: row.Text}
}

// logStoreDayLayout 自然日格式（CST 口径，与切天/保留期判定一致）。
const logStoreDayLayout = "2006-01-02"

// legacyPanelLogsDir 旧实现落盘的面板日志目录名（与 state 文件同目录）。
const legacyPanelLogsDir = "panel-logs"

func init() {
	store.RegisterModels(&panelLogRow{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "panellogs",
		Name: legacyPanelLogsDir,
		Fn:   importLegacyPanelLogs,
	})
}

// importLegacyPanelLogs 把旧的 panel-logs/*.jsonl 全量导入 panel_logs。
//
// 坏行跳过（一行解析不了不该让整天的日志都丢掉）；导入成功后 store.ImportLegacy
// 会把整个目录改名为 panel-logs.imported。
func importLegacyPanelLogs(s *store.Store, dir string) error {
	path, ok := store.LegacyPath(dir, legacyPanelLogsDir)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	var rows []panelLogRow
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "panel-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Join(path, name))
		if readErr != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var e LogEntry
			if json.Unmarshal([]byte(line), &e) != nil || strings.TrimSpace(e.Text) == "" {
				continue
			}
			rows = append(rows, rowOfLog(e))
		}
	}
	if len(rows) == 0 {
		return nil
	}
	if err := store.Insert(s, rows...); err != nil {
		return err
	}
	log.Printf("[panel] 已从旧目录 %s 导入 %d 条运行日志", legacyPanelLogsDir, len(rows))
	return nil
}
