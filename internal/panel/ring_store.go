package panel

import (
	"encoding/json"
	"log"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// taskLogRow 任务频道日志行（表 task_logs）。
//
// 旧实现把任务频道整体写 task-logs.json（JSON 数组，最多 500 条）；这里改为
// 一张表，读写语义不变：每次任务日志入环后整表覆写当前缓冲内的任务行。
type taskLogRow struct {
	ID   uint      `gorm:"column:id;primaryKey;autoIncrement"`
	TS   time.Time `gorm:"column:ts;index"`
	Seq  uint64    `gorm:"column:seq"`
	Ch   string    `gorm:"column:ch;size:16"`
	Text string    `gorm:"column:text_value;type:text"`
}

func (taskLogRow) TableName() string { return "task_logs" }

// rowOfEntry / entry 在内存日志与行之间转换。
func rowOfEntry(e LogEntry) taskLogRow {
	return taskLogRow{TS: e.TS, Seq: e.Seq, Ch: e.Ch, Text: e.Text}
}

func (row taskLogRow) entry() LogEntry {
	return LogEntry{TS: row.TS, Seq: row.Seq, Ch: row.Ch, Text: row.Text}
}

// legacyTaskLogsFile 旧的 JSON 落盘文件名（与 state 文件同目录）。
const legacyTaskLogsFile = "task-logs.json"

func init() {
	store.RegisterModels(&taskLogRow{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "tasklogs",
		Name: legacyTaskLogsFile,
		Fn:   importLegacyTaskLogs,
	})
}

// importLegacyTaskLogs 把旧的 task-logs.json（JSON 数组）导入 task_logs。
func importLegacyTaskLogs(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, legacyTaskLogsFile)
	if !ok {
		return nil
	}
	var entries []LogEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	rows := make([]taskLogRow, 0, len(entries))
	for _, entry := range entries {
		if entry.Ch != ChTask || entry.Text == "" {
			continue
		}
		rows = append(rows, rowOfEntry(entry))
	}
	if err := store.ReplaceAll(s, rows); err != nil {
		return err
	}
	log.Printf("[panel] 已从旧文件 %s 导入 %d 条任务日志", legacyTaskLogsFile, len(rows))
	return nil
}
