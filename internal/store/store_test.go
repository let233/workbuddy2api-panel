package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testItem 本包测试专用表：验证建表 / 覆盖写 / 增量写等通用能力，
// 不依赖任何业务包（store 不反向 import 业务包）。
type testItem struct {
	ID   string `gorm:"column:id;primaryKey;size:64"`
	Name string `gorm:"column:name;size:64"`
	Qty  int    `gorm:"column:qty"`
}

func (testItem) TableName() string { return "test_items" }

// testPair 复合主键测试表（DeleteComposite 用）。
type testPair struct {
	A string `gorm:"column:a;primaryKey;size:32"`
	B string `gorm:"column:b;primaryKey;size:32"`
}

func (testPair) TableName() string { return "test_pairs" }

// 旧数据导入测试用的两个导入器：导入器是包级登记、进程内全局生效的，
// 所以回调里绝不碰 *testing.T（其它用例触发 ImportLegacy 时会再次跑到它们）。
var (
	legacyOKCalls   int
	legacyFailCalls int
)

func init() {
	RegisterModels(&testItem{}, &testPair{})
	RegisterLegacy(LegacyImport{
		Key:  "testlegacy-import",
		Name: "legacy-test.json",
		Fn:   legacyTestImportOK,
	})
	RegisterLegacy(LegacyImport{
		Key:  "testlegacy-broken",
		Name: "broken-test.json",
		Fn:   legacyTestImportBroken,
	})
}

func legacyTestImportOK(st *Store, dir string) error {
	raw, ok := LegacyFile(dir, "legacy-test.json")
	if !ok {
		return nil // 没有旧文件 = 全新部署
	}
	legacyOKCalls++
	if !strings.Contains(string(raw), "world") {
		return errors.New("旧文件内容不符")
	}
	return KVSet(st, "testlegacy:imported", "1")
}

func legacyTestImportBroken(*Store, string) error {
	legacyFailCalls++
	return errors.New("故意失败")
}

// openTest 开一个临时 SQLite 库（每个用例独立文件，互不影响）。
func openTest(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "store-test.db")
	s, err := Open(Config{Driver: DriverSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesSchemaAndFile(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "nested", "wb2api.db")
	if err := EnsureDir(dsn); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	s, err := Open(Config{DSN: dsn}) // Driver 留空 = 默认 sqlite
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if got := s.Driver(); got != DriverSQLite {
		t.Fatalf("driver = %q, want sqlite", got)
	}
	if err := s.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := os.Stat(dsn); err != nil {
		t.Fatalf("库文件未创建: %v", err)
	}
	if !s.DB().Migrator().HasTable(&testItem{}) || !s.DB().Migrator().HasTable(&KV{}) {
		t.Fatalf("自动迁移没建出表")
	}
	if desc := s.Describe(); !strings.HasPrefix(desc, "sqlite://") {
		t.Fatalf("Describe = %q", desc)
	}
}

func TestMigrateRecreatesDroppedTable(t *testing.T) {
	s := openTest(t)
	if err := s.DB().Migrator().DropTable(&testItem{}); err != nil {
		t.Fatalf("DropTable: %v", err)
	}
	if s.DB().Migrator().HasTable(&testItem{}) {
		t.Fatal("表应已删除")
	}
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !s.DB().Migrator().HasTable(&testItem{}) {
		t.Fatal("Migrate 未重建表")
	}
}

func TestNormalizeDriverAndDSN(t *testing.T) {
	cases := map[string]string{
		"":           DriverSQLite,
		"sqlite":     DriverSQLite,
		" SQLite3 ":  DriverSQLite,
		"mysql":      DriverMySQL,
		"MariaDB":    DriverMySQL,
		"postgres":   DriverPostgres,
		"PostgreSQL": DriverPostgres,
		"pgx":        DriverPostgres,
	}
	for in, want := range cases {
		if got := normalizeDriver(in); got != want {
			t.Errorf("normalizeDriver(%q) = %q, want %q", in, got, want)
		}
		if !ValidDriver(in) {
			t.Errorf("ValidDriver(%q) = false", in)
		}
	}
	if ValidDriver("oracle") {
		t.Error("oracle 不该被接受")
	}

	dsn, err := normalizeDSN(DriverSQLite, "")
	if err != nil || !strings.HasPrefix(dsn, DefaultSQLiteDSN) || !strings.Contains(dsn, "busy_timeout") {
		t.Fatalf("缺省 sqlite DSN = %q, err = %v", dsn, err)
	}
	if dsn, _ := normalizeDSN(DriverSQLite, ":memory:"); dsn != ":memory:" {
		t.Fatalf(":memory: 不该被追加参数，得到 %q", dsn)
	}
	if _, err := normalizeDSN(DriverMySQL, ""); err == nil {
		t.Fatal("mysql 缺 DSN 应报错")
	}
	if _, err := normalizeDSN(DriverPostgres, ""); err == nil {
		t.Fatal("postgres 缺 DSN 应报错")
	}
}

func TestEnsureMySQLParams(t *testing.T) {
	got := ensureMySQLParams("user:pass@tcp(127.0.0.1:3306)/wb2api")
	for _, want := range []string{"parseTime=true", "charset=utf8mb4", "loc=Local"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %s: %q", want, got)
		}
	}
	if !strings.Contains(got, "?parseTime=true") {
		t.Errorf("首个参数应接在 ? 后: %q", got)
	}
	withQuery := ensureMySQLParams("user:pass@tcp(h:3306)/db?timeout=5s")
	if !strings.Contains(withQuery, "&parseTime=true") {
		t.Errorf("已有查询串应接 & 而非 ?: %q", withQuery)
	}
	done := ensureMySQLParams("u:p@tcp(h:3306)/db?parseTime=true&charset=utf8mb4&loc=Local")
	if done != "u:p@tcp(h:3306)/db?parseTime=true&charset=utf8mb4&loc=Local" {
		t.Errorf("已完整配置不该再改写: %q", done)
	}
}

func TestDescribeRedactsPassword(t *testing.T) {
	s := New(nil, DriverPostgres)
	s.dsn = "postgres://wb2api:topsecret@db.example:5432/wb2api?sslmode=disable"
	desc := s.Describe()
	if strings.Contains(desc, "topsecret") {
		t.Fatalf("口令未隐去: %s", desc)
	}
	if !strings.Contains(desc, "****") || !strings.Contains(desc, "db.example") {
		t.Fatalf("Describe = %s", desc)
	}

	m := New(nil, DriverMySQL)
	m.dsn = "wb2api:topsecret@tcp(db.example:3306)/wb2api"
	if got := m.Describe(); strings.Contains(got, "topsecret") {
		t.Fatalf("mysql DSN 口令未隐去: %s", got)
	}
}

func TestEnsureDirSkipsSpecialDSN(t *testing.T) {
	for _, dsn := range []string{"", ":memory:", "file::memory:?cache=shared"} {
		if err := EnsureDir(dsn); err != nil {
			t.Errorf("EnsureDir(%q) = %v", dsn, err)
		}
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "a", "b", "x.db")
	if err := EnsureDir(target); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if st, err := os.Stat(filepath.Dir(target)); err != nil || !st.IsDir() {
		t.Fatalf("目录未创建: %v", err)
	}
}

func TestKVLifecycle(t *testing.T) {
	s := openTest(t)
	if _, ok, err := KVGet(s, "missing"); err != nil || ok {
		t.Fatalf("缺失键应返回 ok=false, err=%v", err)
	}
	if err := KVSet(s, "pool:saved_at", "t1"); err != nil {
		t.Fatalf("KVSet: %v", err)
	}
	if err := KVSet(s, "pool:saved_at", "t2"); err != nil {
		t.Fatalf("KVSet 覆盖: %v", err)
	}
	if v, ok, err := KVGet(s, "pool:saved_at"); err != nil || !ok || v != "t2" {
		t.Fatalf("KVGet = %q, %v, %v", v, ok, err)
	}
	if err := KVDel(s, "pool:saved_at"); err != nil {
		t.Fatalf("KVDel: %v", err)
	}
	if _, ok, _ := KVGet(s, "pool:saved_at"); ok {
		t.Fatal("KVDel 后仍能读到")
	}
}

func TestKVListPrefixEscapesWildcards(t *testing.T) {
	s := openTest(t)
	// 前缀里带 _ 与 %：不转义时 LIKE 会把它们当通配符，读回别的键。
	for _, k := range []string{"task_run:a", "taskXrun:b", "task_run:c", "other:d"} {
		if err := KVSet(s, k, k); err != nil {
			t.Fatalf("KVSet(%q): %v", k, err)
		}
	}
	rows, err := KVList(s, "task_run:")
	if err != nil {
		t.Fatalf("KVList: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("前缀匹配 %d 行，want 2: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if !strings.HasPrefix(r.Key, "task_run:") {
			t.Fatalf("串味的键: %q", r.Key)
		}
	}
	if err := KVDelPrefix(s, "task_run:"); err != nil {
		t.Fatalf("KVDelPrefix: %v", err)
	}
	if rows, _ := KVList(s, ""); len(rows) != 2 {
		t.Fatalf("删前缀后剩 %d 行，want 2", len(rows))
	}
}

func TestRepoReplaceAllIsWholeTable(t *testing.T) {
	s := openTest(t)
	if err := ReplaceAll(s, []testItem{{ID: "a", Name: "x", Qty: 1}, {ID: "b", Name: "y", Qty: 2}}); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	if n, _ := Count[testItem](s); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	// 覆盖写：旧的 b 行必须消失（旧 JSON 文件"整份重写"语义）。
	if err := ReplaceAll(s, []testItem{{ID: "a", Name: "x2", Qty: 5}}); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	rows, err := LoadAll[testItem](s)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "a" || rows[0].Name != "x2" {
		t.Fatalf("rows = %+v", rows)
	}
	// 空切片 = 清空表。
	if err := ReplaceAll(s, []testItem{}); err != nil {
		t.Fatalf("ReplaceAll 空: %v", err)
	}
	if n, _ := Count[testItem](s); n != 0 {
		t.Fatalf("清空后 count = %d", n)
	}
}

func TestRepoInsertUpsertDelete(t *testing.T) {
	s := openTest(t)
	if err := Insert(s, testItem{ID: "a", Name: "x", Qty: 1}, testItem{ID: "b", Name: "y", Qty: 2}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if n, _ := Count[testItem](s); n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
	// Upsert 命中主键则更新指定列，否则插入。
	if err := Upsert(s, []testItem{{ID: "a", Name: "x3", Qty: 9}, {ID: "c", Name: "z", Qty: 3}}, []string{"id"}, []string{"name", "qty"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	rows, _ := LoadAll[testItem](s)
	byID := map[string]testItem{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if len(rows) != 3 || byID["a"].Name != "x3" || byID["a"].Qty != 9 || byID["c"].Name != "z" {
		t.Fatalf("Upsert 结果不符: %+v", rows)
	}
	if err := DeleteIn[testItem](s, "name", []string{"y", "z"}); err != nil {
		t.Fatalf("DeleteIn: %v", err)
	}
	if n, _ := Count[testItem](s); n != 1 {
		t.Fatalf("DeleteIn 后 count = %d", n)
	}
	if _, err := DeleteAll[testItem](s); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if n, _ := Count[testItem](s); n != 0 {
		t.Fatalf("DeleteAll 后 count = %d", n)
	}
}

func TestRepoDeleteComposite(t *testing.T) {
	s := openTest(t)
	if err := Insert(s, testPair{A: "u1", B: "m1"}, testPair{A: "u1", B: "m2"}, testPair{A: "u2", B: "m1"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := DeleteComposite[testPair](s, []string{"a", "b"}, [][]any{{"u1", "m1"}, {"u2", "m1"}}); err != nil {
		t.Fatalf("DeleteComposite: %v", err)
	}
	rows, _ := LoadAll[testPair](s)
	if len(rows) != 1 || rows[0].A != "u1" || rows[0].B != "m2" {
		t.Fatalf("rows = %+v", rows)
	}
	if err := DeleteComposite[testPair](s, nil, [][]any{{"u1", "m2"}}); err == nil {
		t.Fatal("空列清单应报错")
	}
	if err := DeleteComposite[testPair](s, []string{"a"}, [][]any{{"u1", "m2"}}); err == nil {
		t.Fatal("列数不匹配应报错")
	}
	// 空键列表是合法空操作。
	if err := DeleteComposite[testPair](s, []string{"a", "b"}, nil); err != nil {
		t.Fatalf("空键列表: %v", err)
	}
}

func TestLegacyImportRunsOnceAndRenames(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy-test.json")
	if err := os.WriteFile(legacy, []byte("{\"hello\":\"world\"}"), 0o600); err != nil {
		t.Fatalf("写入旧文件: %v", err)
	}
	st := openTest(t)
	before := legacyOKCalls
	ImportLegacy(st, dir)
	if legacyOKCalls != before+1 {
		t.Fatalf("导入回调调用 %d 次，want %d", legacyOKCalls, before+1)
	}
	if _, err := os.Stat(legacy + ".imported"); err != nil {
		t.Fatalf("旧文件未改名: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("旧文件仍在: %v", err)
	}
	if v, ok, _ := KVGet(st, legacyKVKey+"testlegacy-import"); !ok || v == "" {
		t.Fatalf("导入标记未写入: %q %v", v, ok)
	}
	if v, ok, _ := KVGet(st, "testlegacy:imported"); !ok || v != "1" {
		t.Fatalf("导入回调未真正落库: %q %v", v, ok)
	}
	// 再次启动：标记已存在，回调不该再跑。
	ImportLegacy(st, dir)
	if legacyOKCalls != before+1 {
		t.Fatalf("重复导入又跑了，共 %d 次", legacyOKCalls)
	}
}

func TestLegacyImportKeepsFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "broken-test.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatalf("写入旧文件: %v", err)
	}
	st := openTest(t)
	before := legacyFailCalls
	ImportLegacy(st, dir)
	if legacyFailCalls != before+1 {
		t.Fatalf("失败导入回调调用 %d 次", legacyFailCalls-before)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("失败时应保留旧文件以便重试: %v", err)
	}
	if _, ok, _ := KVGet(st, legacyKVKey+"testlegacy-broken"); ok {
		t.Fatal("失败不该写导入标记")
	}
	// 下次启动重试。
	ImportLegacy(st, dir)
	if legacyFailCalls != before+2 {
		t.Fatalf("失败后未重试，累计 %d 次", legacyFailCalls)
	}
}

func TestNilStoreIsMemoryOnly(t *testing.T) {
	var s *Store
	if err := s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if s.DB() != nil || s.Driver() != "" || s.Ping() != nil || s.Close() != nil {
		t.Fatal("nil Store 的元信息方法应安全返回零值")
	}
	if desc := s.Describe(); !strings.Contains(desc, "内存") {
		t.Fatalf("Describe = %q", desc)
	}
	if _, ok, err := KVGet(s, "k"); ok || err != nil {
		t.Fatalf("KVGet = %v, %v", ok, err)
	}
	if err := KVSet(s, "k", "v"); err != nil {
		t.Fatalf("KVSet: %v", err)
	}
	if rows, err := KVList(s, ""); err != nil || len(rows) != 0 {
		t.Fatalf("KVList = %+v, %v", rows, err)
	}
	if err := KVDel(s, "k"); err != nil {
		t.Fatalf("KVDel: %v", err)
	}
	if err := KVDelPrefix(s, "k"); err != nil {
		t.Fatalf("KVDelPrefix: %v", err)
	}
	if rows, err := LoadAll[testItem](s); err != nil || len(rows) != 0 {
		t.Fatalf("LoadAll = %+v, %v", rows, err)
	}
	if err := ReplaceAll(s, []testItem{{ID: "a"}}); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}
	if err := Insert(s, testItem{ID: "a"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := Upsert(s, []testItem{{ID: "a"}}, []string{"id"}, []string{"name"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if n, err := Count[testItem](s); n != 0 || err != nil {
		t.Fatalf("Count = %d, %v", n, err)
	}
	if err := DeleteIn[testItem](s, "id", []string{"a"}); err != nil {
		t.Fatalf("DeleteIn: %v", err)
	}
	if err := DeleteComposite[testPair](s, []string{"a", "b"}, [][]any{{"a", "b"}}); err != nil {
		t.Fatalf("DeleteComposite: %v", err)
	}
	if n, err := DeleteAll[testItem](s); n != 0 || err != nil {
		t.Fatalf("DeleteAll = %d, %v", n, err)
	}
	// 纯内存模式下的导入应当无声跳过（dir 里没有旧文件）。
	ImportLegacy(nil, t.TempDir())
}
