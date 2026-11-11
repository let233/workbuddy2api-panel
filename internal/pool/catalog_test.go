package pool

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// 这一组用例钉住「账号列表是数据库分页」这件事本身：分页必须由 SQL 的
// COUNT + ORDER BY + LIMIT/OFFSET 完成（Source == "db"），只有无库运行或库内
// 还没落过盘时才回落内存——否则面板就退化成「把整池拉回来前端切片」，
// 29 个号只是看着还行，几百个号就开始卡在序列化上。

func catalogPool(t *testing.T) *Pool {
	t.Helper()
	db := tempTestDB(t)
	p := New(db)
	// 7 个号：g01/g02 为国际版（domain 后缀判定），其余国内版；分组 A/B 用于过滤断言。
	for _, uid := range []string{"u01", "u02", "u03", "u04", "u05"} {
		p.Add(&auth.Auth{UID: uid, Nickname: "昵称-" + uid, AccessToken: "at-" + uid})
	}
	for _, uid := range []string{"g01", "g02"} {
		p.Add(&auth.Auth{UID: uid, Nickname: "昵称-" + uid, AccessToken: "at-" + uid, Domain: "api.workbuddy.ai"})
	}
	p.SetCredits("u01", 30, 100)
	p.SetCredits("u02", 5, -1)
	p.SetGroup("u01", "A")
	p.SetGroup("u02", "A")
	p.SetGroup("g01", "B")
	p.SyncPersist()
	return p
}

func pageUIDs(list []Status) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.UID)
	}
	return out
}

func TestPageAccountsPagingRunsInDatabase(t *testing.T) {
	p := catalogPool(t)

	// 全池 7 个号，uid 升序：g01 g02 u01 u02 u03 u04 u05。
	first := p.PageAccounts(1, 2, "", "")
	if first.Source != "db" {
		t.Fatalf("source=%q want db（分页必须落在数据库里，不是内存切片）", first.Source)
	}
	if first.Total != 7 || len(first.Accounts) != 2 {
		t.Fatalf("page1 = total %d / %d 行，want 7 / 2", first.Total, len(first.Accounts))
	}
	if got := pageUIDs(first.Accounts); got[0] != "g01" || got[1] != "g02" {
		t.Fatalf("page1 uid = %v，want [g01 g02]", got)
	}
	if first.Accounts[0].Realm != "global" {
		t.Fatalf("g01 realm=%q want global（库行里的 realm 由池写入）", first.Accounts[0].Realm)
	}

	second := p.PageAccounts(2, 2, "", "")
	if got := pageUIDs(second.Accounts); len(got) != 2 || got[0] != "u01" || got[1] != "u02" {
		t.Fatalf("page2 uid = %v，want [u01 u02]", got)
	}
	// 页内状态来自内存：u01 的积分是内存里刚设的 30，不是库行里的旧值。
	if second.Accounts[0].Credits != 30 || second.Accounts[1].Credits != 5 {
		t.Fatalf("page2 积分 = %d/%d，want 30/5", second.Accounts[0].Credits, second.Accounts[1].Credits)
	}

	last := p.PageAccounts(4, 2, "", "")
	if last.Total != 7 || len(last.Accounts) != 1 || last.Accounts[0].UID != "u05" {
		t.Fatalf("末页 = total %d / %v，want 7 / [u05]", last.Total, pageUIDs(last.Accounts))
	}
	beyond := p.PageAccounts(9, 2, "", "")
	if beyond.Total != 7 || len(beyond.Accounts) != 0 {
		t.Fatalf("越界页 = total %d / %d 行，want 7 / 0", beyond.Total, len(beyond.Accounts))
	}

	// 每页条数归一化：0 → 默认 50；超上限钳到 200（再大就退回整表）。
	if big := p.PageAccounts(1, 100000, "", ""); big.Size != maxAccountPageSize {
		t.Fatalf("size 未钳到上限：%d want %d", big.Size, maxAccountPageSize)
	}
	if def := p.PageAccounts(0, 0, "", ""); def.Page != 1 || def.Size != defaultAccountPageSize {
		t.Fatalf("缺省页 = %d/%d，want 1/%d", def.Page, def.Size, defaultAccountPageSize)
	}
}

func TestPageAccountsFiltersInDatabase(t *testing.T) {
	p := catalogPool(t)

	global := p.PageAccounts(1, 50, "global", "")
	if global.Total != 2 {
		t.Fatalf("global total=%d want 2（过滤条件必须进 SQL，否则总数会算成全池）", global.Total)
	}
	if got := pageUIDs(global.Accounts); len(got) != 2 || got[0] != "g01" || got[1] != "g02" {
		t.Fatalf("global uid = %v", got)
	}

	cn := p.PageAccounts(1, 50, "cn", "")
	if cn.Total != 5 {
		t.Fatalf("cn total=%d want 5", cn.Total)
	}
	for _, s := range cn.Accounts {
		if s.Realm == "global" {
			t.Fatalf("cn 页里出现国际版账号 %s", s.UID)
		}
	}

	groupA := p.PageAccounts(1, 50, "", "A")
	if groupA.Total != 2 || len(groupA.Accounts) != 2 {
		t.Fatalf("分组 A = total %d / %d 行，want 2 / 2", groupA.Total, len(groupA.Accounts))
	}
	if got := pageUIDs(groupA.Accounts); got[0] != "u01" || got[1] != "u02" {
		t.Fatalf("分组 A uid = %v", got)
	}
	// 叠加过滤：国际版 + 分组 A 为空（g01 在 B 组）。
	if mixed := p.PageAccounts(1, 50, "global", "A"); mixed.Total != 0 {
		t.Fatalf("global+A total=%d want 0", mixed.Total)
	}
}

// TestPageAccountsLegacyRowsCountAsCN 升级前的库行没有 realm 列值，SQL 侧必须按国内版算
// （否则老部署升级后「国内版」标签页一片空白，用户看到的是「账号全丢了」）；同时
// PageAccounts 发现库行数与内存不符时要按内存把 realm 补回库里，而不是长期靠 SQL 兜。
func TestPageAccountsLegacyRowsCountAsCN(t *testing.T) {
	p := catalogPool(t)
	// 把库行改写成升级前的形态（realm 为空），内存里的号不变。
	rows, err := store.LoadAll[stateAccount](p.db)
	if err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	for i := range rows {
		rows[i].Realm = ""
	}
	if err := store.ReplaceAll(p.db, rows); err != nil {
		t.Fatalf("写库失败: %v", err)
	}

	// SQL 口径：空 realm 的老行算国内版，不算国际版。
	if _, total, err := p.pageRows(1, 50, "cn", ""); err != nil || total != 7 {
		t.Fatalf("老行未按国内版计入 SQL：total=%d err=%v want 7", total, err)
	}
	if _, total, _ := p.pageRows(1, 50, "global", ""); total != 0 {
		t.Fatalf("老行不该算作国际版：global total=%d want 0", total)
	}

	// 走完整分页路径：内存是 cn 5 / global 2，库行被对齐回正确域。
	cn := p.PageAccounts(1, 50, "cn", "")
	if cn.Total != 5 {
		t.Fatalf("cn total=%d want 5", cn.Total)
	}
	global := p.PageAccounts(1, 50, "global", "")
	if global.Total != 2 || global.Source != "db" {
		t.Fatalf("对齐后 global = total %d source %q，want 2 / db", global.Total, global.Source)
	}
	rows2, _ := store.LoadAll[stateAccount](p.db)
	for _, r := range rows2 {
		if r.Realm == "" {
			t.Fatalf("对齐后库里仍有空 realm 行：%s", r.UID)
		}
	}
}

// TestPageAccountsFallsBackToMemoryWithoutDB 无库运行（测试/纯内存部署）时语义不变。
func TestPageAccountsFallsBackToMemoryWithoutDB(t *testing.T) {
	p := New(nil)
	for _, uid := range []string{"a1", "a2", "a3"} {
		p.Add(&auth.Auth{UID: uid, Nickname: uid, AccessToken: "at-" + uid})
	}
	p.SetCredits("a2", 7, 9)

	pg := p.PageAccounts(2, 2, "", "")
	if pg.Source != "memory" {
		t.Fatalf("source=%q want memory", pg.Source)
	}
	if pg.Total != 3 || len(pg.Accounts) != 1 || pg.Accounts[0].UID != "a3" {
		t.Fatalf("第 2 页 = total %d / %v，want 3 / [a3]", pg.Total, pageUIDs(pg.Accounts))
	}
	if pg.Accounts[0].Credits != 0 {
		t.Fatalf("a3 积分=%d want 0", pg.Accounts[0].Credits)
	}
	if cn := p.PageAccounts(1, 10, "cn", ""); cn.Total != 3 {
		t.Fatalf("内存分页的域过滤失效：%d want 3", cn.Total)
	}
}

// TestPageAccountsSyncsWhenDBBehindMemory 库还停在旧状态（落在 5 秒落盘窗口里）时，
// 首次查询必须先把内存对齐进库再查，不能让面板显示上一轮的行数/分组。
func TestPageAccountsSyncsWhenDBBehindMemory(t *testing.T) {
	p := catalogPool(t)
	before := p.PageAccounts(1, 50, "", "")
	if before.Total != 7 {
		t.Fatalf("基线 total=%d want 7", before.Total)
	}
	// 新加一个号，先不落盘。
	p.Add(&auth.Auth{UID: "u06", Nickname: "新号", AccessToken: "at-u06"})
	after := p.PageAccounts(1, 50, "", "")
	if after.Total != 8 {
		t.Fatalf("库与内存不一致时应先对齐再查：total=%d want 8", after.Total)
	}
	found := false
	for _, s := range after.Accounts {
		if s.UID == "u06" && s.Nickname == "新号" {
			found = true
		}
	}
	if !found {
		t.Fatalf("新号未出现在列表里：%v", pageUIDs(after.Accounts))
	}
}

func TestRosterAndCreditsSum(t *testing.T) {
	p := catalogPool(t)
	p.Add(&auth.Auth{UID: "e01", Nickname: "企业号", AccessToken: "at-e01", EnterpriseID: "ent-1"})

	roster := p.Roster()
	if len(roster) != 8 {
		t.Fatalf("名录 %d 行 want 8", len(roster))
	}
	if roster[0].UID != "e01" {
		t.Fatalf("名录未按 uid 排序：首行 %q", roster[0].UID)
	}
	byUID := map[string]RosterItem{}
	for _, r := range roster {
		byUID[r.UID] = r
	}
	if !byUID["e01"].Enterprise {
		t.Fatalf("企业号 enterprise 标志丢了：%+v", byUID["e01"])
	}
	if byUID["g01"].Realm != "global" || byUID["u01"].Realm != "cn" {
		t.Fatalf("名录 realm 错：g01=%q u01=%q", byUID["g01"].Realm, byUID["u01"].Realm)
	}
	if byUID["u01"].Group != "A" || byUID["u01"].Nickname != "昵称-u01" {
		t.Fatalf("名录分组/昵称错：%+v", byUID["u01"])
	}

	remain, total := p.CreditsSum()
	if remain != 35 {
		t.Fatalf("积分合计 %d want 35", remain)
	}
	// 不限额度（total = -1）的账号不计入总额。
	if total != 100 {
		t.Fatalf("总额合计 %d want 100", total)
	}
}

// TestExpiryRowsOnlyUnsettled 到期看板只看「还有未到期批次」的号：已过期/无批次的
// 账号在服务端就丢掉（整页要全量账号，少传一大截 JSON）。
func TestExpiryRowsOnlyUnsettled(t *testing.T) {
	p := New(nil)
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid, Nickname: "名-" + uid, AccessToken: "at-" + uid})
	}
	soon := time.Now().Add(48 * time.Hour)
	p.SetCreditsSnapshot("u1", CreditsSnapshot{
		Remain: 100, Total: 100,
		ExpiryBuckets: []ExpiryBucket{{Date: soon.Format("2006-01-02"), ExpireAt: soon, Remain: 40}},
	})
	p.SetCreditsSnapshot("u2", CreditsSnapshot{Remain: 10, Total: 10}) // 没有到期批次
	p.SetCreditsDetailed("u3", 50, 50, 0, soon.Add(24*time.Hour), 12)  // 只有最早批次快照

	rows := p.ExpiryRows()
	if len(rows) != 2 {
		t.Fatalf("到期行 %d 条 want 2（u2 无批次应被丢掉）", len(rows))
	}
	if rows[0].UID != "u1" || len(rows[0].CreditsExpiryBuckets) != 1 || rows[0].CreditsExpiryBuckets[0].Remain != 40 {
		t.Fatalf("u1 行 = %+v", rows[0])
	}
	if rows[0].Nickname != "名-u1" || rows[0].Realm != "cn" || rows[0].Credits != 100 {
		t.Fatalf("u1 行缺昵称/域/积分：%+v", rows[0])
	}
	if rows[1].UID != "u3" || rows[1].CreditsEarliestRemaining != 12 || rows[1].CreditsEarliestExpiry == "" {
		t.Fatalf("u3 兜底行 = %+v", rows[1])
	}
}
