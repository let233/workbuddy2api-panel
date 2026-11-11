package reqlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// reqLogTableName 归档表名（面板 ArchiveStats.Target 也用它）。
const reqLogTableName = "req_logs"

// legacyReqLogDir 旧的 JSONL 归档目录名（相对 state 文件所在目录）。
const legacyReqLogDir = "request-logs"

// reqLogRow 一条请求事件。
//
// Payload 存完整 Event 的 JSON：事件字段多且常有增量扩展（ModelEff、工具面、
// 上下文……），把每个字段都摊成列只会让 AutoMigrate 每次都加列、读路径还要维护
// 一份字段映射；筛选真正用得到的少数字段（时间、结果、账号、模型、来源）摊成列建索引，
// 其余一律回落到 Payload。Filter 的包含匹配仍在内存里做，语义与旧归档完全一致。
type reqLogRow struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	TS         time.Time `gorm:"column:ts;index"`
	Day        string    `gorm:"column:day;size:16;index"`
	Outcome    string    `gorm:"column:outcome;size:32;index"`
	Account    string    `gorm:"column:account;size:191"`
	Model      string    `gorm:"column:model;size:191"`
	ModelEff   string    `gorm:"column:model_eff;size:191"`
	ClientIP   string    `gorm:"column:client_ip;size:64"`
	UserAgent  string    `gorm:"column:user_agent;size:255"`
	Status     int       `gorm:"column:status"`
	OK         bool      `gorm:"column:ok"`
	DurationMs int64     `gorm:"column:duration_ms"`
	Payload    string    `gorm:"column:payload;type:text"`
}

// TableName 归档表名。
func (reqLogRow) TableName() string { return reqLogTableName }

func init() {
	store.RegisterModels(&reqLogRow{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "reqlog",
		Name: legacyReqLogDir,
		Fn:   importLegacyRequestLogs,
	})
}

// rowOfEvent 把事件拍成一行；列宽按 MySQL 的索引限制截断，避免长 UA/模型名
// 在严格模式下整批插入失败（sqlite 不校验长度，但归档内容本身不依赖它们）。
func rowOfEvent(e Event) (reqLogRow, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return reqLogRow{}, err
	}
	ts := e.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	return reqLogRow{
		TS:         ts,
		Day:        ts.Format(reqLogDayLayout),
		Outcome:    clamp(e.Outcome, 32),
		Account:    clamp(e.Account, 191),
		Model:      clamp(e.Model, 191),
		ModelEff:   clamp(e.ModelEff, 191),
		ClientIP:   clamp(e.ClientIP, 64),
		UserAgent:  clamp(e.UserAgent, 255),
		Status:     e.Status,
		OK:         e.OK,
		DurationMs: e.DurationMs,
		Payload:    string(raw),
	}, nil
}

func (r reqLogRow) event() (Event, error) {
	var e Event
	if err := json.Unmarshal([]byte(r.Payload), &e); err != nil {
		return Event{}, err
	}
	if e.Time.IsZero() {
		e.Time = r.TS
	}
	return e, nil
}

// clamp 按字节截断并按 UTF-8 边界收回（截断点可能落在多字节字符中间）。
func clamp(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// eventLineMaxBytes 旧归档单行上限（超长行整行丢弃并计数）。
var eventLineMaxBytes = 16 << 20

// importLegacyRequestLogs 把旧 JSONL 归档目录导进 req_logs。
//
// 旧格式是每自然日一个 requests-<day>.jsonl（超 16MiB 再切 .1.jsonl…），
// 一行一个 Event。坏行跳过并计数：历史上就有「上下文归档 + 图片」把整行撑爆的情况。
func importLegacyRequestLogs(s *store.Store, dir string) error {
	if s == nil {
		return nil
	}
	src := filepath.Join(dir, legacyReqLogDir)
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	total := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		rows, skipped, err := readLegacyArchiveFile(filepath.Join(src, entry.Name()))
		if err != nil {
			return err
		}
		if skipped > 0 {
			log.Printf("[reqlog] 导入 %s 时跳过 %d 行无法解析的事件", entry.Name(), skipped)
		}
		if len(rows) == 0 {
			continue
		}
		if err := store.Insert(s, rows...); err != nil {
			return err
		}
		total += len(rows)
	}
	log.Printf("[reqlog] 已从旧目录 %s 导入 %d 条请求事件", legacyReqLogDir, total)
	return nil
}

func readLegacyArchiveFile(path string) ([]reqLogRow, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 64<<10)
	var (
		rows    []reqLogRow
		skipped int
	)
	for {
		line, tooLong, err := nextLine(reader, eventLineMaxBytes)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, skipped, err
		}
		if tooLong {
			skipped++
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			skipped++
			continue
		}
		row, err := rowOfEvent(e)
		if err != nil {
			skipped++
			continue
		}
		rows = append(rows, row)
	}
	return rows, skipped, nil
}

// nextLine 读一行（去掉结尾换行）；超过 max 字节的行整行丢弃并返回 tooLong。
//
// 用 ReadByte 而不是 Scanner：Scanner 遇超长行会整体报错（旧实现曾因此让面板
// 「请求事件日志」整页 500），这里只丢这一行。
func nextLine(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return bytes.TrimRight(buf, "\r"), false, nil
			}
			return nil, false, err
		}
		if b == 0x0a {
			return bytes.TrimRight(buf, "\r"), false, nil
		}
		buf = append(buf, b)
		if len(buf) > max {
			for {
				b, err := r.ReadByte()
				if err != nil {
					return nil, true, nil
				}
				if b == 0x0a {
					return nil, true, nil
				}
			}
		}
	}
}
