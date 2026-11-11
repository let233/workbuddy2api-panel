package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// accountsEnv 起一个「真库」面板：账号列表的分页必须由 SQL 完成，用 pool.New(nil)
// 测不到这一点（那走的是内存回落），所以这里开临时 SQLite。
type accountsEnv struct {
	panel *Panel
	pool  *pool.Pool
}

func newAccountsEnv(t *testing.T) *accountsEnv {
	t.Helper()
	db, err := store.Open(store.Config{Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "wb2api.db")})
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p := pool.New(db)
	// 7 个号：u01..u05 国内版，g01/g02 国际版；分组 A 两人、B 一人。
	for _, uid := range []string{"u01", "u02", "u03", "u04", "u05"} {
		p.Add(&auth.Auth{UID: uid, Nickname: "昵称-" + uid, AccessToken: "at-" + uid})
	}
	for _, uid := range []string{"g01", "g02"} {
		p.Add(&auth.Auth{UID: uid, Nickname: "昵称-" + uid, AccessToken: "at-" + uid, Domain: "api.workbuddy.ai"})
	}
	p.SetCredits("u01", 30, 100)
	p.SetGroup("u01", "A")
	p.SetGroup("u02", "A")
	p.SetGroup("g01", "B")
	p.SyncPersist()
	return &accountsEnv{panel: New(Config{Version: "test", Pool: p}), pool: p}
}

type accountsResp struct {
	Accounts []struct {
		UID       string `json:"uid"`
		Nickname  string `json:"nickname"`
		Realm     string `json:"realm"`
		Group     string `json:"group"`
		Credits   int64  `json:"credits"`
		Paused    bool   `json:"paused"`
		Disabled  bool   `json:"disabled"`
		SuccessNs int64  `json:"success_count"`
	} `json:"accounts"`
	Total  int64            `json:"total"`
	Page   int              `json:"page"`
	Size   int              `json:"size"`
	Pages  int              `json:"pages"`
	Realm  string           `json:"realm"`
	Group  string           `json:"group"`
	Counts map[string]int   `json:"counts"`
	Groups []map[string]any `json:"groups"`
	Source string           `json:"source"`
}

func (e *accountsEnv) get(t *testing.T, url string) accountsResp {
	t.Helper()
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s code=%d body=%s", url, rec.Code, rec.Body)
	}
	var d accountsResp
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("%s 解析失败: %v body=%s", url, err, rec.Body)
	}
	return d
}

// TestAccountsEndpointPaginatesInDatabase 账号列表分页的接线：/panel/api/accounts
// 必须按 page/size 只回一页，且总数是**过滤后**的全池条数（不能是当页长度）。
//
// 为什么用真库：这条需求的原文是「要数据库分页不要前端分页」。前端切片也能让
// total/每页条数看起来正常，只有 source == "db" 才能证明分页真的落在 SQL 上。
func TestAccountsEndpointPaginatesInDatabase(t *testing.T) {
	e := newAccountsEnv(t)

	first := e.get(t, "/panel/api/accounts?page=1&size=2")
	if first.Source != "db" {
		t.Fatalf("source=%q want db（否则就是前端分页）", first.Source)
	}
	if first.Total != 7 || first.Pages != 4 || len(first.Accounts) != 2 {
		t.Fatalf("page1 = total %d / pages %d / %d 行，want 7 / 4 / 2", first.Total, first.Pages, len(first.Accounts))
	}
	if first.Accounts[0].UID != "g01" || first.Accounts[1].UID != "g02" {
		t.Fatalf("page1 顺序错：%s %s", first.Accounts[0].UID, first.Accounts[1].UID)
	}
	if first.Accounts[0].Realm != "global" {
		t.Fatalf("g01 realm=%q want global", first.Accounts[0].Realm)
	}
	if first.Counts["all"] != 7 || first.Counts["cn"] != 5 || first.Counts["global"] != 2 {
		t.Fatalf("counts=%v want all7/cn5/global2", first.Counts)
	}

	// 第 2 页 = u01,u02（uid 升序：g01 g02 u01 u02 u03 u04 u05）。
	page2 := e.get(t, "/panel/api/accounts?page=2&size=2")
	if page2.Page != 2 || len(page2.Accounts) != 2 || page2.Total != 7 {
		t.Fatalf("page2 = %d / %d 行 / total %d", page2.Page, len(page2.Accounts), page2.Total)
	}
	if page2.Accounts[0].UID != "u01" || page2.Accounts[0].Credits != 30 || page2.Accounts[0].Group != "A" {
		t.Fatalf("page2 首行 = %+v（积分/分组应来自内存实时值）", page2.Accounts[0])
	}
	last := e.get(t, "/panel/api/accounts?page=4&size=2")
	if len(last.Accounts) != 1 || last.Accounts[0].UID != "u05" {
		t.Fatalf("末页 = %d 行，首行 %s", len(last.Accounts), last.Accounts[0].UID)
	}
	// 越界页夹回末页：删号后停在高页码时不该看到空表却翻不回来。
	beyond := e.get(t, "/panel/api/accounts?page=9&size=2")
	if beyond.Page != 4 || beyond.Total != 7 || len(beyond.Accounts) != 1 || beyond.Pages != 4 {
		t.Fatalf("越界页 = page %d / total %d / %d 行 / pages %d，want 4/7/1/4",
			beyond.Page, beyond.Total, len(beyond.Accounts), beyond.Pages)
	}
}

// TestAccountsEndpointFiltersAndGroupList 域/分组过滤进 SQL，且分组下拉清单由服务端给
// （分页后页内可能没有某个分组，前端从页内反推会缺项）。
func TestAccountsEndpointFiltersAndGroupList(t *testing.T) {
	e := newAccountsEnv(t)

	global := e.get(t, "/panel/api/accounts?realm=global&size=50")
	if global.Realm != "global" || global.Total != 2 || len(global.Accounts) != 2 {
		t.Fatalf("realm=global → realm %q total %d / %d 行", global.Realm, global.Total, len(global.Accounts))
	}
	cn := e.get(t, "/panel/api/accounts?realm=cn&size=50")
	if cn.Total != 5 {
		t.Fatalf("realm=cn total=%d want 5", cn.Total)
	}
	all := e.get(t, "/panel/api/accounts?realm=all&size=50")
	if all.Realm != "" || all.Total != 7 {
		t.Fatalf("realm=all 应等价于不传：realm=%q total=%d", all.Realm, all.Total)
	}

	groupA := e.get(t, "/panel/api/accounts?group=A&size=50")
	if groupA.Group != "A" || groupA.Total != 2 {
		t.Fatalf("group=A → group %q total %d want 2", groupA.Group, groupA.Total)
	}
	// 分组清单是全池口径（分组下拉不能随页缩水），且带每组的数量。
	names := map[string]float64{}
	for _, g := range groupA.Groups {
		name, _ := g["name"].(string)
		cnt, _ := g["count"].(float64)
		names[name] = cnt
	}
	if names["A"] != 2 || names["B"] != 1 {
		t.Fatalf("分组清单 = %v want A:2 B:1", names)
	}
	// 切到国际版后，分组清单只列该域下的组（B 组是国际版）。
	globalGroups := e.get(t, "/panel/api/accounts?realm=global&size=50")
	if len(globalGroups.Groups) != 1 {
		t.Fatalf("国际版分组清单 = %v want 只有 B", globalGroups.Groups)
	}
	if name, _ := globalGroups.Groups[0]["name"].(string); name != "B" {
		t.Fatalf("国际版分组 = %v want B", globalGroups.Groups)
	}
}

// TestAccountsRosterEndpoint 名录端点保持轻量全量：备注/分组编辑器、组内批量、
// 「余额趋势行标禁用」都要按 uid 找全池，但它们只要身份与分组维度。
func TestAccountsRosterEndpoint(t *testing.T) {
	e := newAccountsEnv(t)
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts/roster", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("roster code=%d body=%s", rec.Code, rec.Body)
	}
	var d struct {
		Total    int `json:"total"`
		Accounts []struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
			Realm    string `json:"realm"`
			Group    string `json:"group"`
			Paused   bool   `json:"paused"`
			Disabled bool   `json:"disabled"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Total != 7 || len(d.Accounts) != 7 {
		t.Fatalf("roster total=%d 行=%d want 7", d.Total, len(d.Accounts))
	}
	if d.Accounts[0].UID != "g01" || d.Accounts[0].Realm != "global" {
		t.Fatalf("roster 首行 = %+v", d.Accounts[0])
	}
	// 名录不该带积分/用量这类大字段（那是分页端点的事）。
	if bytes := rec.Body.String(); len(bytes) > 1600 {
		t.Fatalf("roster 载荷过大（%d 字节），名录应保持轻量", len(bytes))
	}
}
