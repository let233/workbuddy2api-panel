package panel

// 账号列表分页与到期提醒的独立端点。
//
// 为什么分开：/panel/api/overview 是 5 秒级轮询的总览，此前顺带把**全池每个账号**
// 的完整状态（积分/冷却/熔断/用量/模型成本）一坨塞进去——账号多了以后每次轮询
// 都要序列化几百份大对象，而面板一屏只看得见二十行。现在总览只给汇总数，账号
// 列表按页向数据库取（SQL COUNT + LIMIT/OFFSET），到期提醒也另开端点。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// accountsPage GET /panel/api/accounts?page=&size=&realm=&group=
//
// 分页与过滤都在数据库里完成（见 pool.PageAccounts），响应带总数/页数，前端只
// 渲染当页。counts/groups 是页签与分组下拉的计数，按「当前域」实时算，与页无关。
func (p *Panel) accountsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := atoiOr(q.Get("page"), 1)
	size := atoiOr(q.Get("size"), 0)
	realm := q.Get("realm")
	if realm == "all" {
		realm = ""
	}
	group := q.Get("group")

	res := p.cfg.Pool.PageAccounts(page, size, realm, group)
	pages := (res.Total + int64(res.Size) - 1) / int64(res.Size)
	if pages < 1 {
		pages = 1
	}
	// 越界页夹回末页：删号后停在高页码、或手抖点了 page=99 时，用户不该看到
	// 一张空表却翻不回来。前端以响应里的 page 为准（见 loadAccounts）。
	if res.Page > int(pages) {
		res = p.cfg.Pool.PageAccounts(int(pages), res.Size, realm, group)
		res.Page = int(pages)
	}
	counts, groups := p.realmCounts(realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": res.Accounts,
		"total":    res.Total,
		"page":     res.Page,
		"size":     res.Size,
		"pages":    pages,
		"realm":    realm,
		"group":    group,
		"counts":   counts,
		"groups":   groups,
		// source=db 表示这一页由 SQL 取出；memory 是回落（无库/尚未落盘）。
		"source": res.Source,
	})
}

// accountsRoster GET /panel/api/accounts/roster 全池名录（uid/昵称/域/分组/暂停/禁用）。
//
// 备注编辑器、分组编辑器、组内批量、"余额趋势行标禁用"这类需求要的是**全量 uid**，
// 但不是全量状态：名录保持轻量，账号列表页才走分页取状态。
func (p *Panel) accountsRoster(w http.ResponseWriter, r *http.Request) {
	items := p.cfg.Pool.Roster()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": items,
		"total":    len(items),
	})
}

// accountsExport GET /panel/api/accounts/export?uid=…&uid=…&realm=&group=
//
// 导出账号凭据，形状见 pool.ExportAccount（与 cockpit tools 的账号详情导出同一套
// 字段名，导出的文件能原样被 /panel/api/import/cockpit 吃回去）。
//
// 两种口径：带 uid（可重复，也接受 uids=a,b 逗号串）只导这几号——多选框选中的
// 账号可能跨页，前端只能逐个报 uid；不带 uid 则按 realm/group 过滤整池——「导出
// 当前分组」不该只导出看得见的那一页。
//
// 响应体是裸数组而不是 {accounts:[…]}：外层套信封会把文件变成本面板的私有格式，
// 而这份文件的用途恰恰是搬运到别处。
func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	uids := append([]string{}, q["uid"]...)
	for _, v := range q["uids"] {
		for _, part := range strings.Split(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				uids = append(uids, s)
			}
		}
	}
	realm := q.Get("realm")
	if realm == "all" {
		realm = ""
	}
	items := p.cfg.Pool.ExportAccounts(uids, realm, q.Get("group"))
	if len(items) == 0 {
		// 空数组写出去只会得到一个 0 字节的「导出成功」，用户拿到文件才发现是空的。
		writeErr(w, http.StatusNotFound, "没有匹配的账号可导出")
		return
	}
	name := fmt.Sprintf("workbuddy-accounts-%d-%s.json", len(items), time.Now().Format("20060102-150405"))
	// 凭据不能进任何缓存：CDN/代理/浏览器都可能把响应体留在磁盘上。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(items)
}

// expiry GET /panel/api/expiry 积分到期提醒快照（独立页）。
//
// 只带到期相关字段（按日批次 + 最早到期兜底 + 昵称/域），顺带透出后台余额刷新的
// 节奏与年龄——面板要如实标注「每 N 分钟后台刷新 · M 分钟前更新」。
func (p *Panel) expiry(w http.ResponseWriter, r *http.Request) {
	rows := p.cfg.Pool.ExpiryRows()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":             rows,
		"count":                len(rows),
		"balance_refresh_sec":  p.balanceRefreshSec(),
		"balance_refreshed_at": p.balanceRefreshedAt(),
	})
}

// realmCounts 域计数与分组计数（供页签与分组下拉）。realm 为空 = 全部。
//
// 分组清单必须由服务端给：分页之后「当前页」未必含有某个分组，前端若从页内数据
// 反推分组，下拉项会随翻页缺斤少两。
func (p *Panel) realmCounts(realm string) (map[string]int, []map[string]any) {
	counts := map[string]int{"all": 0, "cn": 0, "global": 0}
	byGroup := map[string]int{}
	items := p.cfg.Pool.Roster()
	for _, it := range items {
		counts["all"]++
		if it.Realm == "global" {
			counts["global"]++
		} else {
			counts["cn"]++
		}
		if realm != "" && !realmMatchesForPanel(it.Realm, realm) {
			continue
		}
		if it.Group != "" {
			byGroup[it.Group]++
		}
	}
	names := make([]string, 0, len(byGroup))
	for name := range byGroup {
		names = append(names, name)
	}
	sort.Strings(names)
	groups := make([]map[string]any, 0, len(names))
	for _, name := range names {
		groups = append(groups, map[string]any{"name": name, "count": byGroup[name]})
	}
	return counts, groups
}

// realmMatchesForPanel 域过滤口径，与 pool 内一致：cn = 非国际版。
func realmMatchesForPanel(realm, want string) bool {
	switch want {
	case "", "all":
		return true
	case "cn":
		return realm != "global"
	default:
		return realm == want
	}
}

// atoiOr 解析查询参数里的整数，失败用默认值。
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
