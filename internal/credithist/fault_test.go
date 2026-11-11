package credithist

// T5 故障注入与边界测试（账本层）。只新增测试，不改业务代码。
//
// 存储从「整文件重写」换成数据库后，故障注入点也跟着变：文件不可写 → 表不存在
// （库连不上/权限不足的等价形态），损坏文件 → 损坏的旧格式文件导入。

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// ── 1. 落库失败 ───────────────────────────────────────────────────────────

// 表不存在（≈ 落库目标不可用）时，Observe 只能记日志：不得 panic、不得影响内存
// 账本；故障排除（AutoMigrate 建回表）后必须恢复落库。
func TestFaultUnwritableTargetKeepsMemoryAndRecovers(t *testing.T) {
	db := storetest.Open(t)
	l := New(db, 100)
	l.Observe("u1", 100) // 基线
	l.Observe("u1", 250) // +150
	if got := l.Read(0); len(got) != 1 || got[0].Delta != 150 || got[0].After != 250 {
		t.Fatalf("正常路径应记账，得到 %+v", got)
	}

	// 制造故障：把两张表删掉，后续写入必失败。
	for _, tbl := range []string{"credit_history", "credit_baselines"} {
		if err := db.DB().Exec("DROP TABLE " + tbl).Error; err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	l.Observe("u1", 400)                 // +150：落库失败
	l.Observe("u2", 10)                  // 新基线：落库失败
	if got := l.Read(0); len(got) != 2 { // 内存账本照常
		t.Fatalf("落库失败不应影响内存账本，得到 %+v", got)
	}
	if l.snapshot["u1"] != 400 || l.snapshot["u2"] != 10 {
		t.Fatalf("内存基线应推进，得到 %v", l.snapshot)
	}

	// 排除故障：重新建表，再观测必须恢复落库。
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	l.Observe("u1", 500) // +100
	if n, err := store.Count[entryRow](db); err != nil || n != 1 {
		t.Fatalf("恢复后应落库 1 条流水（表刚建回），得到 %d (err=%v)", n, err)
	}
	if n, err := store.Count[baselineRow](db); err != nil || n != 1 {
		t.Fatalf("恢复后应落库 1 个基线（u2 的写入丢了，u1 的应成功），得到 %d (err=%v)", n, err)
	}
}

// ── 2. 退化/损坏的旧文件 ──────────────────────────────────────────────────

// 损坏 JSON、null 快照/流水、空文件、类型不符、顶层数组——导入一律不 panic：
// 解析失败的文件跳过（留待下次重试），能解析的按空内容导入，账本照常可用。
func TestFaultDegenerateFilesStartEmpty(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		fails bool // true = 导入失败，旧文件应保留
	}{
		{name: "invalid_json", body: "{not json", fails: true},
		{name: "empty_file", body: "", fails: true},
		{name: "null_snapshot_and_entries", body: `{"version":1,"snapshot":null,"entries":null}`},
		{name: "entries_null", body: `{"version":1,"snapshot":{"u1":5},"entries":null}`},
		{name: "wrong_types", body: `{"version":1,"snapshot":"x","entries":42}`, fails: true},
		{name: "top_level_array", body: "[]", fails: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "credit-history.json")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}

			db := storetest.OpenDir(t, dir)
			store.ImportLegacy(db, dir) // 必须不 panic
			_, err := os.Stat(path)
			if c.fails && err != nil {
				t.Fatalf("导入失败时旧文件应保留: %v", err)
			}
			if !c.fails && err == nil {
				t.Fatalf("导入成功后旧文件应改名，仍存在 %s", path)
			}

			l := New(db, 100)
			base := len(l.Read(0))
			l.Observe("fresh", 100) // 基线
			l.Observe("fresh", 130) // +30
			got := l.Read(0)
			if len(got) != base+1 {
				t.Fatalf("退化输入后应能正常观测：base=%d 期望 %d 条，得到 %d（%+v）", base, base+1, len(got), got)
			}
			if got[0].UID != "fresh" || got[0].Delta != 30 || got[0].Before != 100 || got[0].After != 130 {
				t.Fatalf("新增流水字段不符: %+v", got[0])
			}
		})
	}
}

// 旧文件里 null entries 但快照有内容：快照必须导入，重启不会把旧余额当新增。
func TestFaultLegacySnapshotOnlyImport(t *testing.T) {
	dir := t.TempDir()
	raw := `{"version":1,"snapshot":{"u1":5},"entries":null}`
	if err := os.WriteFile(filepath.Join(dir, "credit-history.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	db := storetest.OpenDir(t, dir)
	store.ImportLegacy(db, dir)

	l := New(db, 100)
	l.Observe("u1", 5) // 与基线同值
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("导入的快照应作为基线，得到 %d 条流水", got)
	}
	l.Observe("u1", 15) // +10
	got := l.Read(0)
	if len(got) != 1 || got[0].Delta != 10 || got[0].Before != 5 {
		t.Fatalf("应按导入的快照续接: %+v", got)
	}
}

// ── 3. maxEntries 回落与截断 ──────────────────────────────────────────────

// maxEntries <= 0 回落默认上限，不产生无界增长；=1 只留最新一条。
func TestFaultMaxEntriesFallbackAndTrim(t *testing.T) {
	for _, m := range []int{0, -1, -1000} {
		l := New(nil, m)
		if l.max != defaultMaxEntries {
			t.Fatalf("maxEntries=%d 应回落默认 %d，得到 %d", m, defaultMaxEntries, l.max)
		}
	}

	// 不无界增长：写 defaultMaxEntries+300 次，内存与库都必须恰为默认上限。
	db := storetest.Open(t)
	l := New(db, 0)
	l.Observe("u1", 0)
	for i := int64(1); i <= int64(defaultMaxEntries)+300; i++ {
		l.Observe("u1", i)
	}
	got := l.Read(0)
	if len(got) != defaultMaxEntries {
		t.Fatalf("默认上限 %d 应截断，得到 %d", defaultMaxEntries, len(got))
	}
	if got[0].After != int64(defaultMaxEntries)+300 {
		t.Fatalf("截断后最新一条应保留，得到 %+v", got[0])
	}
	if n, err := store.Count[entryRow](db); err != nil {
		t.Fatalf("数流水行数: %v", err)
	} else if n < int64(defaultMaxEntries)-1000 || n > int64(defaultMaxEntries) {
		t.Fatalf("库里的流水应裁剪到上限附近，得到 %d（上限 %d）", n, defaultMaxEntries)
	}
	if l2 := New(db, 2000); len(l2.Read(0)) != defaultMaxEntries {
		t.Fatalf("重启后应读回 %d 条，得到 %d", defaultMaxEntries, len(l2.Read(0)))
	}

	// maxEntries=1：只留最新一条，且丢的是最旧。
	l1 := New(nil, 1)
	l1.Observe("u1", 0)
	l1.Observe("u1", 10)
	l1.Observe("u1", 20)
	one := l1.Read(0)
	if len(one) != 1 || one[0].After != 20 || one[0].Delta != 10 {
		t.Fatalf("上限 1 应只留最新一条，得到 %+v", one)
	}
}

// ── 4. Read 边界 ──────────────────────────────────────────────────────────

// limit<=0 = 全部；大于总数 = 全部；1 = 最新；顺序严格新的在前。
func TestFaultReadBoundaries(t *testing.T) {
	l := New(nil, 100)
	l.Observe("u1", 0)
	for i := int64(1); i <= 3; i++ {
		l.Observe("u1", i*10)
	}
	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"zero_is_all", 0, 3},
		{"negative_is_all", -7, 3},
		{"over_total_is_all", 99, 3},
		{"exact_total", 3, 3},
		{"two", 2, 2},
		{"one", 1, 1},
	}
	for _, c := range cases {
		if got := l.Read(c.limit); len(got) != c.want {
			t.Fatalf("%s: Read(%d) 得到 %d 条，期望 %d", c.name, c.limit, len(got), c.want)
		}
	}
	got := l.Read(0)
	if got[0].After != 30 || got[1].After != 20 || got[2].After != 10 {
		t.Fatalf("必须严格新的在前: %+v", got)
	}
	got = l.Read(1)
	if len(got) != 1 || got[0].After != 30 {
		t.Fatalf("Read(1) 应取最新一条: %+v", got)
	}
}

// ── 5. Δ 边界 ────────────────────────────────────────────────────────────

// 恰好 ±maxDelta 记账；±(maxDelta+1) 只更新基线、不留痕，且基线必须推进到新值。
func TestFaultDeltaBoundaries(t *testing.T) {
	// +++ 恰好 +1000000 记账
	l := New(nil, 100)
	l.Observe("u1", 0)
	l.Observe("u1", maxDelta)
	if got := l.Read(0); len(got) != 1 || got[0].Delta != maxDelta || got[0].Before != 0 || got[0].After != maxDelta {
		t.Fatalf("恰好 +%d 应记账: %+v", maxDelta, got)
	}
	// --- 恰好 -1000000 记账
	l = New(nil, 100)
	l.Observe("u1", maxDelta)
	l.Observe("u1", 0)
	if got := l.Read(0); len(got) != 1 || got[0].Delta != -maxDelta {
		t.Fatalf("恰好 -%d 应记账: %+v", maxDelta, got)
	}
	// +++ 超限 +1000001 不留痕，但基线推进
	l = New(nil, 100)
	l.Observe("u1", 0)
	l.Observe("u1", maxDelta+1)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("超限 +%d 不应留痕，得到 %d 条", maxDelta+1, got)
	}
	if l.snapshot["u1"] != maxDelta+1 {
		t.Fatalf("超限后基线应推进到 %d，得到 %d", maxDelta+1, l.snapshot["u1"])
	}
	l.Observe("u1", 1) // Δ = 1-(maxDelta+1) = -maxDelta → 正常记账
	got := l.Read(0)
	if len(got) != 1 || got[0].Delta != -maxDelta || got[0].Before != maxDelta+1 || got[0].After != 1 {
		t.Fatalf("超限后应按新基线记账: %+v", got)
	}
	// --- 超限 -1000001 不留痕，基线推进
	l = New(nil, 100)
	l.Observe("u1", 0)
	l.Observe("u1", -(maxDelta + 1))
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("超限 -%d 不应留痕，得到 %d 条", maxDelta+1, got)
	}
	if l.snapshot["u1"] != -(maxDelta + 1) {
		t.Fatalf("超限后基线应推进到 %d，得到 %d", -(maxDelta + 1), l.snapshot["u1"])
	}
	l.Observe("u1", -1) // Δ = -1-(-(maxDelta+1)) = +maxDelta
	got = l.Read(0)
	if len(got) != 1 || got[0].Delta != maxDelta || got[0].Before != -(maxDelta+1) {
		t.Fatalf("超限后应按新基线记账: %+v", got)
	}
}

// ── 6. uid 边界 ───────────────────────────────────────────────────────────

// 空 uid（上游异常条目）不建基线、不记账。
func TestFaultEmptyUIDIgnored(t *testing.T) {
	l := New(nil, 100)
	l.Observe("", 100)
	l.Observe("", 200)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("空 uid 不应记账，得到 %d 条", got)
	}
	if len(l.snapshot) != 0 {
		t.Fatalf("空 uid 不应建基线，得到 %v", l.snapshot)
	}
}

// 纯空白 uid（上游脏数据）同样不建基线（实现里判的是 TrimSpace）。
func TestFaultWhitespaceUIDNotBaselined(t *testing.T) {
	l := New(nil, 100)
	l.Observe("   ", 100)
	l.Observe("\t\n", 100)
	if got := len(l.Read(0)); got != 0 {
		t.Fatalf("纯空白 uid 不应记账，得到 %d 条", got)
	}
	if len(l.snapshot) != 0 {
		t.Fatalf("纯空白 uid 不应建基线，但快照里有 %d 个键: %v", len(l.snapshot), l.snapshot)
	}
}

// ── 7. 并发 ──────────────────────────────────────────────────────────────

// 多 goroutine（不同 uid 定量 + 同一 uid 混跑）不 panic、不同 uid 条数不丢、
// 库里与内存逐条一致；同一 uid 并发后再顺序写一个唯一哨兵，必须是最终基线。
func TestFaultConcurrentMixedUIDs(t *testing.T) {
	db := storetest.Open(t)
	l := New(db, 4000)
	const accounts = 6
	const rounds = 30
	for i := 0; i < accounts; i++ {
		l.Observe(fmt.Sprintf("acct-%d", i), 0)
	}
	l.Observe("shared", 0)

	var wg sync.WaitGroup
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("acct-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(1); n <= rounds; n++ {
				l.Observe(uid, n*10)
			}
		}()
	}
	for w := 0; w < 8; w++ {
		base := int64(w) * 1000
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(1); n <= 20; n++ {
				l.Observe("shared", base+n)
			}
		}()
	}
	wg.Wait()

	// 顺序写入唯一哨兵 = 「最后一次写入」，并发结束后必须生效。
	l.Observe("shared", 777777)
	if l.snapshot["shared"] != 777777 {
		t.Fatalf("最后写入应成为基线，得到 %d", l.snapshot["shared"])
	}

	mem := l.Read(0)
	per := map[string]int{}
	for _, e := range mem {
		per[e.UID]++
	}
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("acct-%d", i)
		if per[uid] != rounds {
			t.Fatalf("%s 应有 %d 条流水，得到 %d", uid, rounds, per[uid])
		}
	}

	// 库里的流水必须与内存逐条一致（丢写会让库少于内存/快照回退）。
	l2 := New(db, 4000)
	disk := l2.Read(0)
	if len(disk) != len(mem) {
		t.Fatalf("重启后条数不一致：内存 %d / 库 %d", len(mem), len(disk))
	}
	for i := range mem {
		m, d := mem[i], disk[i]
		if m.UID != d.UID || m.Delta != d.Delta || m.Before != d.Before || m.After != d.After || !m.Time.Equal(d.Time) {
			t.Fatalf("第 %d 条内存/库不一致: 内存 %+v / 库 %+v", i, m, d)
		}
	}
	if l2.snapshot["shared"] != 777777 {
		t.Fatalf("重启后 shared 基线应为 777777，得到 %d", l2.snapshot["shared"])
	}
	for i := 0; i < accounts; i++ {
		uid := fmt.Sprintf("acct-%d", i)
		if l2.snapshot[uid] != rounds*10 {
			t.Fatalf("重启后 %s 基线应为 %d，得到 %d", uid, rounds*10, l2.snapshot[uid])
		}
	}
}

// ── 8. 不再产生数据文件 ────────────────────────────────────────────────────

// 账本落库后不再往数据目录写 credit-history.json（文件模式已彻底退役）。
func TestFaultNoLedgerFileWritten(t *testing.T) {
	dir := t.TempDir()
	db := storetest.OpenDir(t, dir)
	l := New(db, 100)
	l.Observe("u1", 1)
	l.Observe("u1", 2)
	l.Observe("u1", 3)
	left, err := filepath.Glob(filepath.Join(dir, "credit-history*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("不应再产生账本文件，发现 %v", left)
	}
	if n, err := store.Count[entryRow](db); err != nil || n != 2 {
		t.Fatalf("应落库 2 条流水，得到 %d (err=%v)", n, err)
	}
	_ = time.Now
}
