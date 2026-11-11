// watchdog.go 上游目录改版监控：定期对「客户端实际看到的 /v1/models」取指纹，变了就通知。
//
// 为什么要这个：上游改目录/改能力字段时（换模型名、加档位表、改 can_disable_thinking、
// 下架某模型）客户端只会表现为「报错」或「选项没了」，看不出是上游改的。盯住这份指纹，
// 变化就能第一时间落到告警通道，而不是等用户来报。
//
// 不用另起一套抓取：modelList() 就是网关自己维护、已被各类轮询刷新的那份数据——复用
// 它等于零新增上游调用，而且盯的正是客户端真正拿到的东西（含白名单/隐藏过滤后的结果）。
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WatchCatalog 常驻循环比对目录指纹；ctx 结束即停。notify 为 nil 时只写日志。
// 首轮只建基线不通知（否则每次重启都报一次「变化」）。
//
// 热改（SetWatchdog）：开关与间隔每轮从原子槽位现读——关掉只是「这一轮不探测」
// （零上游调用、不刷日志），打开或改间隔会立刻打断当前等待（watchdogKick），
// 不必等满一个旧周期。main 因此只需启动一次，不用在配置保存后重启/再启一条。
//
// every 只作为**首次打底**：若启动前没人显式 SetWatchdog 过（常见路径），就用它
// 播种；已经显式设过则尊重热改值，否则启动参数会把刚下发的配置顶回去。
func (h *Handler) WatchCatalog(ctx context.Context, every time.Duration, notify func(string)) {
	if h.hot.watchdogSeeded.CompareAndSwap(false, true) {
		if every <= 0 {
			every = time.Hour
		}
		h.hot.watchdogInterval.Store(int64(every))
		h.hot.watchdogEnabled.Store(true)
	}
	// prevFP/prevIDs 跨「关闭期」保留：重新打开后仍能对照关闭前那一眼，
	// 关掉期间的上游变化不会被悄悄吞掉。
	var prevFP string
	var prevIDs []string
	for {
		enabled, interval := h.watchdog()
		if !enabled {
			select {
			case <-ctx.Done():
				return
			case <-h.hot.watchdogKick:
			}
			continue
		}
		fp, ids := catalogFingerprint(h.modelList())
		switch {
		case prevFP == "":
			log.Printf("watchdog: 上游目录基线 %d 个模型 fp=%s", len(ids), fp[:12])
		case fp != prevFP:
			msg := "上游模型目录发生变化：" + catalogDiff(prevIDs, ids)
			log.Printf("watchdog: %s", msg)
			if notify != nil {
				notify(msg)
			}
		}
		prevFP, prevIDs = fp, ids
		// 用 Timer + kick 而不是 Ticker：间隔可热改，Ticker 的周期改不了。
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-h.hot.watchdogKick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// catalogFingerprint 对目录条目排序后取 sha256：与顺序无关，任一字段变化都会变。
func catalogFingerprint(list []map[string]any) (string, []string) {
	type item struct {
		ID   string
		JSON string
	}
	items := make([]item, 0, len(list))
	ids := make([]string, 0, len(list))
	for _, e := range list {
		id, _ := e["id"].(string)
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		items = append(items, item{ID: id, JSON: string(b)})
		ids = append(ids, id)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	sort.Strings(ids)
	h := sha256.New()
	for _, it := range items {
		h.Write([]byte(it.ID))
		h.Write([]byte{0})
		h.Write([]byte(it.JSON))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), ids
}

// catalogDiff 给出新增/下线的模型（只报前若干个，避免一条消息刷屏）。
func catalogDiff(prev, cur []string) string {
	seen := make(map[string]bool, len(prev))
	for _, id := range prev {
		seen[id] = true
	}
	now := make(map[string]bool, len(cur))
	var added []string
	for _, id := range cur {
		now[id] = true
		if !seen[id] {
			added = append(added, id)
		}
	}
	var removed []string
	for _, id := range prev {
		if !now[id] {
			removed = append(removed, id)
		}
	}
	parts := make([]string, 0, 2)
	if len(added) > 0 {
		parts = append(parts, "新增 "+preview(added))
	}
	if len(removed) > 0 {
		parts = append(parts, "下线 "+preview(removed))
	}
	if len(parts) == 0 {
		// 模型集合没变，变的是某个模型的字段（能力/档位/上限）。
		return "模型集合不变，某个模型的字段有改动（能力/档位/上限等）"
	}
	return strings.Join(parts, "；")
}

func preview(ids []string) string {
	const max = 8
	if len(ids) <= max {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:max], ", ") + " 等 " + strconv.Itoa(len(ids)) + " 个"
}
