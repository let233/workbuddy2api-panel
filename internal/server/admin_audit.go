// admin_audit.go /admin 操作审计：每次管理操作落一行（数据库 admin_audit 表）。
// 缺省关闭。写失败不影响请求本身。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// auditRow /admin 操作审计表的一行（原先是 JSONL 里的一行）。
type auditRow struct {
	ID     uint      `gorm:"column:id;primaryKey;autoIncrement"`
	TS     time.Time `gorm:"column:ts;index"`
	Action string    `gorm:"column:action;size:64;index"`
	Target string    `gorm:"column:target;size:255"`
	Status int       `gorm:"column:status"`
	Remote string    `gorm:"column:remote;size:64"`
	Key    string    `gorm:"column:key_fp;size:32"`
	Reason string    `gorm:"column:reason;type:text"`
}

// TableName 固定表名。
func (auditRow) TableName() string { return "admin_audit" }

func init() {
	store.RegisterModels(&auditRow{})
	store.RegisterLegacy(store.LegacyImport{Key: "adminaudit", Name: "admin_audit.log", Fn: importLegacyAudit})
}

// importLegacyAudit 把旧 data/admin_audit.log（每行一条 JSON）搬进 admin_audit。
func importLegacyAudit(st *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, "admin_audit.log")
	if !ok {
		return nil
	}
	rows := make([]auditRow, 0, 64)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 半截行（进程被杀）跳过，不因为一行坏数据放弃整份历史
		}
		ts, err := time.Parse(time.RFC3339, e.TS)
		if err != nil {
			ts = time.Now()
		}
		rows = append(rows, auditRow{
			TS: ts, Action: e.Action, Target: e.Target,
			Status: e.Status, Remote: e.Remote, Key: e.Key, Reason: e.Reason,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	if err := store.Insert(st, rows...); err != nil {
		return err
	}
	log.Printf("[audit] 已从旧文件 admin_audit.log 导入 %d 条审计记录", len(rows))
	return nil
}

// AuditLog /admin 操作审计（落库）。db 为 nil 时 NewAuditLog 直接报错：
// 审计是安全设施，静默丢弃不如启动时就明确失败。
type AuditLog struct {
	db    *store.Store
	keyFP string
	mu    sync.Mutex
}

// NewAuditLog 创建审计器。enabled 来自配置（原先是文件路径，现在只当开关）；
// 关闭时调用方不传 AuditLog，不会走到这里。
func NewAuditLog(db *store.Store, apiKey string) (*AuditLog, error) {
	if db == nil {
		return nil, errors.New("审计日志需要数据库：当前为纯内存模式")
	}
	return &AuditLog{db: db, keyFP: keyFingerprint(apiKey)}, nil
}

// Path 兼容旧接口：审计已入库，没有文件路径。
func (a *AuditLog) Path() string {
	if a == nil {
		return ""
	}
	return "数据库表 admin_audit"
}

func keyFingerprint(apiKey string) string {
	if apiKey == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:4])
}

type auditEntry struct {
	TS     string `json:"ts"`
	Action string `json:"action"`
	Target string `json:"target"`
	Status int    `json:"status"`
	Remote string `json:"remote"`
	Key    string `json:"key"`
	Reason string `json:"reason,omitempty"`
}

func (a *AuditLog) record(e auditEntry) {
	if a == nil || a.db == nil {
		return
	}
	e.Key = a.keyFP
	ts, err := time.Parse(time.RFC3339, e.TS)
	if err != nil {
		ts = time.Now()
	}
	row := auditRow{
		TS: ts, Action: e.Action, Target: e.Target,
		Status: e.Status, Remote: e.Remote, Key: e.Key, Reason: e.Reason,
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := store.Insert(a.db, row); err != nil {
		log.Printf("[audit] 写库失败，本条未记录（操作已生效）：%v", err)
	}
}

// Recent 读最近 limit 条审计记录（新的在前）。运维排查与测试用。
func (a *AuditLog) Recent(limit int) ([]auditRow, error) {
	if a == nil || a.db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []auditRow
	err := a.db.DB().Order("id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

const auditBodyPeek = 4 << 10

// audit 挂在 withAuth 之内：匿名请求不落盘。
//
// 审计器按**每次请求**从热改槽位取（SetAudit）：面板关掉审计后下一条管理面请求就
// 不再落库。以前在注册期判 nil 直接返回 next，配置热改后那条路会一直沿用启动时的
// 结论，只能靠重启才能生效。
func (h *Handler) audit(action string, target func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		al := h.auditLog()
		if al == nil { // 关闭审计：直接放过，不留痕迹也不判 404（审计是记录不是门禁）
			next(w, r)
			return
		}
		reason := peekReason(r)
		rec := &auditResponseWriter{ResponseWriter: w}
		next(rec, r)
		al.record(auditEntry{
			TS:     time.Now().Format(time.RFC3339),
			Action: action,
			Target: target(r),
			Status: rec.statusCode(),
			Remote: r.RemoteAddr,
			Reason: reason,
		})
	}
}

func auditPathValue(field string) func(*http.Request) string {
	return func(r *http.Request) string { return r.PathValue(field) }
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func peekReason(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf, _ := io.ReadAll(io.LimitReader(r.Body, auditBodyPeek))
	r.Body = auditBody{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	var body struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(buf, &body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

type auditBody struct {
	io.Reader
	io.Closer
}
