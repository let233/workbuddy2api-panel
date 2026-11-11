package pool

// 账号名录与库内分页：面板账号列表不再把「全池 Status」一坨塞进 /overview，
// 而是按页向数据库取 uid（SQL 的 COUNT + ORDER BY + LIMIT/OFFSET），再用内存
// 里的状态补齐这一页的值。库里的行只是索引（uid + 几个可用于过滤的列），
// 内存才是真值来源——两者分工写在 PageAccounts 的注释里。

import (
	"log"
	"sort"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

const (
	// defaultAccountPageSize 面板没带 size 时的每页条数。
	defaultAccountPageSize = 50
	// maxAccountPageSize 每页条数上限：再大就退回「整表拉一遍」，分页失去意义。
	maxAccountPageSize = 200
)

// RosterItem 名录的一行：只有身份与分组维度（备注/分组编辑器、批量动作选号用），
// 不含积分/冷却/用量等大字段——名录要全量，状态只要一页。
type RosterItem struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	Realm      string `json:"realm"`
	Group      string `json:"group"`
	Paused     bool   `json:"paused"`
	Disabled   bool   `json:"disabled"`
	Enterprise bool   `json:"enterprise"`
	APIKey     bool   `json:"api_key"`
}

// Roster 全池名录（按 uid 排序，与 List 同序）。
func (p *Pool) Roster() []RosterItem {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]RosterItem, 0, len(uids))
	for _, uid := range uids {
		e := p.byUID[uid]
		out = append(out, RosterItem{
			UID:        uid,
			Nickname:   e.a.Nickname,
			Realm:      e.a.Realm(),
			Group:      e.group,
			Paused:     e.paused,
			Disabled:   e.disabled,
			Enterprise: e.a.IsEnterprise(),
			APIKey:     e.a.IsAPIKey(),
		})
	}
	return out
}

// StatusesFor 按给定 uid 顺序取状态（不在池里的 uid 跳过）。分页取页后用它补值。
func (p *Pool) StatusesFor(uids []string) []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		e, ok := p.byUID[uid]
		if !ok {
			continue
		}
		out = append(out, p.statusOf(uid, e))
	}
	return out
}

// CreditsSum 全池积分合计（剩余, 总额）。总额只累加有上限的账号（-1 = 不限不计）。
func (p *Pool) CreditsSum() (remain, total int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, e := range p.byUID {
		remain += e.credits
		if e.creditsTotal > 0 {
			total += e.creditsTotal
		}
	}
	return remain, total
}

// realmMatches 域过滤：cn 的含义是「非国际版」（空 realm 的老数据也算国内版）。
func realmMatches(realm, want string) bool {
	switch want {
	case "", "all":
		return true
	case "cn":
		return realm != "global"
	default:
		return realm == want
	}
}

// normalizePage 归一化页码/页长。
func normalizePage(page, size int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = defaultAccountPageSize
	}
	if size > maxAccountPageSize {
		size = maxAccountPageSize
	}
	return page, size
}

// AccountPage 一页账号 + 分页元信息。
type AccountPage struct {
	Accounts []Status
	// Total 过滤后的总条数（不受本页 Limit 影响，来自 SQL COUNT 或内存计数）。
	Total int64
	Page  int
	Size  int
	// Source 数据来源："db" = 分页在数据库里完成；"memory" = 回落内存分页
	// （无库运行，或库里还没落过盘）。面板与测试据此确认没有退化成前端切片。
	Source string
}

// PageAccounts 取账号列表的一页（按 uid 升序）。realm/group 为空表示不过滤。
//
// 分页在数据库里完成：COUNT(*) 给总数，ORDER BY uid LIMIT/OFFSET 给当页 uid。
// 只有当页 uid 会拿回内存取完整状态（积分、冷却、在途、用量都在内存里实时变动，
// 落盘最多滞后 5 秒，不适合直接从库行展示）。
//
// 回落：p.db == nil（测试/无库运行）或库里还没有行（首次启动尚未落盘）时退到
// 内存分页——语义与库内分页一致，只是数据源不同，避免「刚启动面板空表」。
func (p *Pool) PageAccounts(page, size int, realm, group string) AccountPage {
	page, size = normalizePage(page, size)
	out := AccountPage{Page: page, Size: size}
	if p.db == nil {
		out.Accounts, out.Total = p.memPage(page, size, realm, group)
		out.Source = "memory"
		return out
	}
	// 库只是索引，内存才是真值：两边行数对不上（首次启动还没落过盘、刚加/删号
	// 落在 5 秒落盘窗口里、建号后立刻开面板）就以内存为准先对齐一次再查。
	// 计数是纯整数遍历，不构造 Status，代价远小于「拿库行直接展示」带来的错页。
	want := p.memCount(realm, group)
	rows, total, err := p.pageRows(page, size, realm, group)
	if err != nil {
		log.Printf("pool: 账号分页查询失败（%v），回落内存分页", err)
		out.Accounts, out.Total = p.memPage(page, size, realm, group)
		out.Source = "memory"
		return out
	}
	if total != want {
		p.SyncPersist()
		if r2, t2, err2 := p.pageRows(page, size, realm, group); err2 == nil {
			rows, total = r2, t2
		}
		if total != p.memCount(realm, group) {
			// 对齐后仍不一致：写库没成（只读库/磁盘满）。退回内存分页，页面照样对。
			log.Printf("pool: 库内账号行数与内存不一致（库 %d / 内存 %d），本次回落内存分页", total, want)
			out.Accounts, out.Total = p.memPage(page, size, realm, group)
			out.Source = "memory"
			return out
		}
	}
	uids := make([]string, 0, len(rows))
	for _, row := range rows {
		uids = append(uids, row.UID)
	}
	stamps := p.StatusesFor(uids)
	// 内存里已经删掉、库里还留着的行：按行兜底合成最小状态，页面不出现空洞。
	if len(stamps) < len(rows) {
		have := make(map[string]struct{}, len(stamps))
		for _, s := range stamps {
			have[s.UID] = struct{}{}
		}
		for _, row := range rows {
			if _, ok := have[row.UID]; ok {
				continue
			}
			stamps = append(stamps, statusFromRow(row))
		}
	}
	out.Accounts, out.Total, out.Source = stamps, total, "db"
	return out
}

// memCount 内存里符合过滤条件的账号数（只数不取值）。
func (p *Pool) memCount(realm, group string) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var n int64
	for _, e := range p.byUID {
		if !realmMatches(e.a.Realm(), realm) {
			continue
		}
		if group != "" && e.group != group {
			continue
		}
		n++
	}
	return n
}

// pageRows 一页库内行（uid 升序）。
func (p *Pool) pageRows(page, size int, realm, group string) ([]stateAccount, int64, error) {
	opts := store.PageOpts{
		Order:  "uid ASC",
		Limit:  size,
		Offset: (page - 1) * size,
	}
	filter := map[string]any{}
	if group != "" {
		filter["group"] = group
	}
	switch realm {
	case "global":
		filter["realm"] = "global"
	case "cn":
		// 空 realm = 升级前落盘的老行，按国内版算（与 realmMatches 同口径）。
		opts.Where = "realm IS NULL OR realm = '' OR realm = 'cn'"
	}
	if len(filter) > 0 {
		opts.Filter = filter
	}
	return store.Page[stateAccount](p.db, opts)
}

// memPage 内存分页（库不可用时的回落；语义与 SQL 版一致）。
func (p *Pool) memPage(page, size int, realm, group string) ([]Status, int64) {
	all := p.List()
	kept := make([]Status, 0, len(all))
	for _, s := range all {
		if !realmMatches(s.Realm, realm) {
			continue
		}
		if group != "" && s.Group != group {
			continue
		}
		kept = append(kept, s)
	}
	total := int64(len(kept))
	from := (page - 1) * size
	if from >= len(kept) {
		return []Status{}, total
	}
	to := from + size
	if to > len(kept) {
		to = len(kept)
	}
	return kept[from:to], total
}

// statusFromRow 用库里的一行给内存里已不存在的账号兜底合成状态。
func statusFromRow(row stateAccount) Status {
	return Status{
		UID:                      row.UID,
		Credits:                  row.Credits,
		CreditsTotal:             row.CreditsTotal,
		Realm:                    row.Realm,
		Group:                    row.Group,
		Disabled:                 row.Disabled,
		Paused:                   row.Paused,
		SuccessCount:             row.SuccessCount,
		ErrTotal:                 row.ErrTotal,
		CreditsExpiring:          row.CreditsExpiring,
		CreditsEarliestExpiry:    row.CreditsEarliestExpiry,
		CreditsEarliestRemaining: row.CreditsEarliestRemaining,
		CreditsExpiryBuckets:     append([]ExpiryBucket(nil), row.CreditsExpiryBuckets...),
	}
}

// ExpiryRow 到期提醒页的一行（只带到期相关字段：整页要全量账号，不该顺带
// 把积分/冷却/用量一起序列化）。
type ExpiryRow struct {
	UID                      string         `json:"uid"`
	Nickname                 string         `json:"nickname"`
	Realm                    string         `json:"realm"`
	Credits                  int64          `json:"credits"`
	CreditsExpiryBuckets     []ExpiryBucket `json:"credits_expiry_buckets,omitempty"`
	CreditsEarliestExpiry    string         `json:"credits_earliest_expiry,omitempty"`
	CreditsEarliestRemaining int64          `json:"credits_earliest_remaining,omitempty"`
}

// ExpiryRows 全部账号的到期快照（按 uid 排序）。只有带未到期批次的账号才返回，
// 前端本来就丢掉空行——在服务端丢掉可以少传一大截 JSON。
func (p *Pool) ExpiryRows() []ExpiryRow {
	cutoff := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]ExpiryRow, 0, len(uids))
	for _, uid := range uids {
		e := p.byUID[uid]
		row := ExpiryRow{
			UID:      uid,
			Nickname: e.a.Nickname,
			Realm:    e.a.Realm(),
			Credits:  e.credits,
		}
		buckets := make([]ExpiryBucket, 0, len(e.expiryBuckets))
		for _, b := range e.expiryBuckets {
			if b.Remain <= 0 {
				continue
			}
			if !b.ExpireAt.IsZero() && b.ExpireAt.Before(cutoff) {
				continue
			}
			buckets = append(buckets, b)
		}
		// 兜底：没有按日批次但有「最早到期」快照的账号也要留在页上（前端拿
		// earliest_expiry/remaining 顶一行），否则这类账号在到期页凭空消失。
		earliest := !e.creditsEarliestExpiry.IsZero() && !e.creditsEarliestExpiry.Before(cutoff) && e.creditsEarliestRemaining > 0
		if len(buckets) == 0 && !earliest {
			continue
		}
		row.CreditsExpiryBuckets = buckets
		if !e.creditsEarliestExpiry.IsZero() {
			row.CreditsEarliestExpiry = e.creditsEarliestExpiry.Format(time.RFC3339)
		}
		row.CreditsEarliestRemaining = e.creditsEarliestRemaining
		out = append(out, row)
	}
	return out
}

// ---------------------------------------------------------------------------
// 账号导出（可与 /panel/api/import/cockpit 互相喂）
// ---------------------------------------------------------------------------

// ExportAccount 一条导出记录，形状与 cockpit tools「账号详情」导出逐字对齐：
//
//	{"account":{"uid":"…","nickname":"…","enterpriseId":"…"},
//	 "auth":{"accessToken":"…","refreshToken":"…","expiresAt":<秒>,"domain":"…","realm":"cn"}}
//
// 为什么不是直接序列化 Status：这份文件的用途是**凭据搬运**（换部署、备份、
// 给另一台面板导入）。积分/冷却/用量这些是运行态，导进去也没有意义；而字段名
// 必须和导入侧认识的一致，否则「导出再导入」这种最基本的往返就断了。
type ExportAccount struct {
	Account ExportAccountInfo `json:"account"`
	Auth    ExportAuthInfo    `json:"auth"`
}

// ExportAccountInfo 账号身份部分（不含令牌）。
type ExportAccountInfo struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterpriseId"`
}

// ExportAuthInfo 凭据部分。ExpiresAt 是 **Unix 秒**（cockpit 单账号详情导出的口径；
// 批量导出用毫秒，导入侧 normalizeExpiresAtMillis 两种都认）。
type ExportAuthInfo struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
}

// ExportAccounts 导出账号凭据。
//
// uids 非空 → 只导这几号（按传入顺序去重，未知 uid 跳过：前端刚在别的窗口删掉
// 的号不该让整批导出 404）；为空 → 按 realm/group 过滤整池，顺序与 Roster 一致
// （uid 升序）。
//
// 锁序沿用 Roster 的既有约定（p.mu → a.mu 只在 Realm() 这类单次读上取）；真正的
// 取值在释放 p.mu 之后做，避免长时间握着池锁等某个账号的 refresh。
func (p *Pool) ExportAccounts(uids []string, realm, group string) []ExportAccount {
	type picked struct {
		uid string
		a   *auth.Auth
	}
	var items []picked

	p.mu.RLock()
	if len(uids) > 0 {
		seen := make(map[string]struct{}, len(uids))
		for _, uid := range uids {
			uid = strings.TrimSpace(uid)
			if uid == "" {
				continue
			}
			if _, dup := seen[uid]; dup {
				continue
			}
			seen[uid] = struct{}{}
			e, ok := p.byUID[uid]
			if !ok {
				continue
			}
			items = append(items, picked{uid: uid, a: e.a})
		}
	} else {
		all := make([]string, 0, len(p.byUID))
		for uid := range p.byUID {
			all = append(all, uid)
		}
		sort.Strings(all)
		for _, uid := range all {
			e := p.byUID[uid]
			if !realmMatches(e.a.Realm(), realm) {
				continue
			}
			if group != "" && e.group != group {
				continue
			}
			items = append(items, picked{uid: uid, a: e.a})
		}
	}
	p.mu.RUnlock()

	out := make([]ExportAccount, 0, len(items))
	for _, it := range items {
		a := it.a
		a.Lock()
		expiresAt, nickname, enterpriseID := a.ExpiresAt, a.Nickname, a.EnterpriseID
		a.Unlock()
		out = append(out, ExportAccount{
			Account: ExportAccountInfo{UID: it.uid, Nickname: nickname, EnterpriseID: enterpriseID},
			Auth: ExportAuthInfo{
				AccessToken:  a.AccessTokenValue(),
				RefreshToken: a.RefreshTokenValue(),
				ExpiresAt:    expiresAt,
				Domain:       a.DomainValue(),
				Realm:        a.Realm(),
			},
		})
	}
	return out
}
