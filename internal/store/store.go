// Package store 数据持久化层：gorm 之上的统一入口（SQLite / MySQL / PostgreSQL）。
//
// 面板此前把所有运行数据都摊在 JSON 文件里（state.json / usage.json /
// credit-history.json / *-logs.jsonl …），一次进程崩溃就可能留下半截文件，多实例
// 部署更是没法共享状态。本包把这些数据搬进关系库：
//
//   - 驱动三选一：SQLite（默认，纯 Go 实现、无需 CGO，开箱即用）/ MySQL / PostgreSQL；
//   - 建表由 gorm.AutoMigrate 完成（含索引），升级二进制即自动迁移，无需手写 SQL；
//   - 各业务包通过 RegisterModels 注册自己的表结构，store 不反向依赖任何业务包。
//
// 约定：*Store 为 nil 表示"纯内存模式"（测试与禁用持久化时使用），所有方法对 nil
// 接收者安全（读返回空、写静默成功），调用方无需到处判空。
package store

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 支持的驱动名（不区分大小写）。
const (
	DriverSQLite   = "sqlite"
	DriverMySQL    = "mysql"
	DriverPostgres = "postgres"
)

// DefaultSQLiteDSN 缺省 SQLite 库文件（相对工作目录）。
//
// 选文件而非 :memory:：面板重启后账号冷却、积分历史、用量统计都要接着算，内存库
// 一重启就全丢。文件落在工作目录，与旧版 state.json 同级，运维心智不变。
const DefaultSQLiteDSN = "wb2api.db"

// Config 数据库连接配置（来自 config.json 的 database 段）。
type Config struct {
	// Driver 驱动名：sqlite（默认）/ mysql / postgres。
	Driver string
	// DSN 连接串：
	//   sqlite   → 库文件路径（缺省 wb2api.db），也接受 :memory: 与 file: 前缀；
	//   mysql    → user:pass@tcp(host:port)/dbname?parseTime=true&charset=utf8mb4
	//   postgres → host=... user=... password=... dbname=... port=5432 sslmode=disable
	DSN string
}

// Store 数据库句柄包装。业务包只依赖本类型，不直接接触 gorm。
type Store struct {
	db     *gorm.DB
	driver string
	dsn    string
}

// models 待建表的结构列表（由各业务包 init 时 RegisterModels 注入）。
var models []any

// RegisterModels 注册需要自动迁移的表结构。业务包在自己的 init 里调用，
// 这样 store 包不必反向 import 各业务包（避免循环依赖）。
func RegisterModels(m ...any) {
	models = append(models, m...)
}

// Open 按配置打开数据库并执行自动迁移。driver 为空时默认 SQLite。
func Open(cfg Config) (*Store, error) {
	driver := normalizeDriver(cfg.Driver)
	dsn, err := normalizeDSN(driver, cfg.DSN)
	if err != nil {
		return nil, err
	}
	dial, err := dialector(driver, dsn)
	if err != nil {
		return nil, err
	}
	db, err := gorm.Open(dial, &gorm.Config{
		// 只打错误级日志：面板自己的日志已经够吵，gorm 的 SQL 回显只在排障时需要。
		// 只报真正的错误：KVGet 之类"查不到"的侦察读会走 ErrRecordNotFound，
		// 那是正常控制流，不该刷屏。
		Logger: logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Error,
			IgnoreRecordNotFoundError: true,
			Colorful:                  false,
		}),
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败（driver=%s）: %w", driver, err)
	}
	s := &Store{db: db, driver: driver, dsn: dsn}
	if err := s.tunePool(); err != nil {
		return nil, err
	}
	if err := s.Migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// New 用既有 gorm 句柄构造 Store（测试与外部注入用）。
func New(db *gorm.DB, driver string) *Store {
	return &Store{db: db, driver: normalizeDriver(driver)}
}

// Migrate 建表/加列（gorm AutoMigrate）。幂等：已存在的表只补缺失的列与索引。
func (s *Store) Migrate() error {
	if s == nil || s.db == nil {
		return nil
	}
	if len(models) > 0 {
		if err := s.db.AutoMigrate(models...); err != nil {
			return fmt.Errorf("自动迁移失败: %w", err)
		}
	}
	return nil
}

// DB 暴露底层 gorm 句柄（少数需要手写查询的地方用）。
func (s *Store) DB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// Driver 返回实际使用的驱动名。
func (s *Store) Driver() string {
	if s == nil {
		return ""
	}
	return s.driver
}

// Describe 返回不含口令的连接描述（打日志用）。
func (s *Store) Describe() string {
	if s == nil {
		return "内存（未启用持久化）"
	}
	if strings.Contains(s.dsn, "://") {
		// DSN 自带 scheme（postgres://…）：别再拼一次驱动名。
		return redactDSN(s.dsn)
	}
	return s.driver + "://" + redactDSN(s.dsn)
}

// Close 关闭底层连接池。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Ping 探活（启动自检用）。
func (s *Store) Ping() error {
	if s == nil || s.db == nil {
		return nil
	}
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}

func (s *Store) tunePool() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	if s.driver == DriverSQLite {
		// SQLite 单写者：多连接并发写会直接撞 SQLITE_BUSY。串行化到一条连接，
		// 写入由 busy_timeout 兜住等待——面板的写量（几秒一行状态）远低于瓶颈。
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetConnMaxLifetime(0)
		return nil
	}
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	return nil
}

func normalizeDriver(driver string) string {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "", DriverSQLite, "sqlite3":
		return DriverSQLite
	case DriverMySQL, "mariadb":
		return DriverMySQL
	case DriverPostgres, "postgresql", "psql", "pgx":
		return DriverPostgres
	default:
		return strings.ToLower(strings.TrimSpace(driver))
	}
}

// ValidDriver 报告驱动名是否受支持（配置校验用）。
func ValidDriver(driver string) bool {
	switch normalizeDriver(driver) {
	case DriverSQLite, DriverMySQL, DriverPostgres:
		return true
	}
	return false
}

func normalizeDSN(driver, dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	switch driver {
	case DriverSQLite:
		if dsn == "" {
			dsn = DefaultSQLiteDSN
		}
		return sqliteDSN(dsn), nil
	case DriverMySQL:
		if dsn == "" {
			return "", fmt.Errorf("mysql 需要配置 database.dsn")
		}
		return ensureMySQLParams(dsn), nil
	case DriverPostgres:
		if dsn == "" {
			return "", fmt.Errorf("postgres 需要配置 database.dsn")
		}
		return dsn, nil
	default:
		return "", fmt.Errorf("不支持的数据库驱动 %q（可选 sqlite / mysql / postgres）", driver)
	}
}

// sqliteDSN 给库文件补上必要的 pragma：WAL（读写不互斥）+ busy_timeout（等锁而不是
// 直接报错）+ 外键约束。已显式带 pragma / :memory: / file: 前缀时按原样使用。
func sqliteDSN(dsn string) string {
	if dsn == ":memory:" || strings.HasPrefix(dsn, "file:") || strings.Contains(dsn, "_pragma=") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

// ensureMySQLParams 补上 parseTime（gorm 的 time.Time 字段需要）与 utf8mb4。
func ensureMySQLParams(dsn string) string {
	need := []string{}
	lower := strings.ToLower(dsn)
	if !strings.Contains(lower, "parsetime=") {
		need = append(need, "parseTime=true")
	}
	if !strings.Contains(lower, "charset=") {
		need = append(need, "charset=utf8mb4")
	}
	if !strings.Contains(lower, "loc=") {
		need = append(need, "loc=Local")
	}
	if len(need) == 0 {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + strings.Join(need, "&")
}

func dialector(driver, dsn string) (gorm.Dialector, error) {
	switch driver {
	case DriverSQLite:
		return sqlite.Open(dsn), nil
	case DriverMySQL:
		return mysql.Open(dsn), nil
	case DriverPostgres:
		return postgres.Open(dsn), nil
	default:
		return nil, fmt.Errorf("不支持的数据库驱动 %q（可选 sqlite / mysql / postgres）", driver)
	}
}

// redactDSN 隐去 DSN 中的口令（日志里只留库地址）。
//
// 纯字符串切分而不走 url.Parse：url 会把 **** 编码成 %2A%2A%2A%2A，日志更难认，
// 而且 postgres URL 形态自带 scheme，Describe 再拼一次就成了 postgres://postgres://…
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn // 无口令（或 SQLite 文件路径）
	}
	start := 0
	if i := strings.Index(dsn, "://"); i >= 0 {
		start = i + 3
	}
	cred := dsn[start:at]
	colon := strings.Index(cred, ":")
	if colon < 0 {
		return dsn // 只有用户名，没有口令
	}
	return dsn[:start] + cred[:colon] + ":****" + dsn[at:]
}

// EnsureDir 为 SQLite 库文件创建父目录（:memory: / file::memory: / 远端 authority 不动）。
//
// file: 前缀的 DSN 也要处理：它一样是本地文件，SQLite 不会替你建目录——
// file:./data/wb2api.db 在 data/ 不存在时直接报 unable to open database file，
// 而报错信息里的 errno 是 "out of memory (14)"，看起来像内存问题，实际是路径不存在。
func EnsureDir(dsn string) error {
	path, ok := localDBPath(dsn)
	if !ok {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建数据库目录 %s: %w", dir, err)
	}
	return nil
}

// localDBPath 从 SQLite DSN 里取出库文件的本地路径（去掉 file: 前缀与 ?param 部分）。
//
// ok=false 表示这不是一个「需要且能够」建目录的本地文件：空 DSN、:memory:、
// file::memory:、以及 file://host/... 这种带远端 authority 的形态（那由 SQLite
// 自己当 URI 处理，别在这里替它决定路径）。
func localDBPath(dsn string) (string, bool) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" || dsn == ":memory:" {
		return "", false
	}
	p := dsn
	if strings.HasPrefix(p, "file:") {
		p = strings.TrimPrefix(p, "file:")
		// file:// 形态（file:///C:/x.db、file://localhost/x.db、file://host/…）不碰：
		// authority 与「绝对路径要不要去掉前导斜杠」都随平台变，判错了会在 Windows 上
		// 拼出 "\\C:\…" 这种路径。这类写法请自己先把目录建好，SQLite 认得它。
		if strings.HasPrefix(p, "//") {
			return "", false
		}
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if p == "" || p == ":memory:" {
		return "", false
	}
	return p, true
}

// LogSummary 启动时打一条连接摘要（口令已隐去）。
func (s *Store) LogSummary() {
	if s == nil {
		log.Printf("store: 未启用持久化（纯内存模式）")
		return
	}
	log.Printf("store: 已连接数据库 %s（自动迁移完成，共 %s 张表）", s.Describe(), strconv.Itoa(len(models)))
}
