// offline.go 「401 掉线」这一种禁用原因的**唯一生成口径**。
//
// 为什么要单开一类原因（而不是复用 12153 session dead / 11140 风控）：
// 面板上有三件按原因分流的事，认错了就会误伤——
//   - 「删除所有已掉线」只该删「请求已经 401，这个号现在打不动」的号；把风控封禁
//     一起删掉，等于把还能等上游放行的号删了；
//   - 每日探测只探「已被封禁」的号，探测到上游解封后按配置自动解冻；面板手动禁用
//     的号是运维的显式意图，不能被自动翻案；
//   - 运维看列表时要能一眼分出「该重登 / 该等上游 / 是我自己停的」。
//
// 所以原因文案只能由本文件生成，调用点禁止手拼字符串（前缀判断见 IsOfflineReason）。
package pool

import "strings"

const (
	// ReasonOffline 「已掉线」禁用原因前缀。完整文案由 OfflineReason 组装。
	ReasonOffline = "已掉线"
	// ReasonManualDisable 面板手动禁用的默认原因。「每日探测自动解冻」必须跳过它：
	// 手动禁用是运维意图，不因上游探活结果被翻案。
	ReasonManualDisable = "面板手动禁用（管理员操作）"
)

// OfflineReason 组装掉线原因：「已掉线（<detail>）」。detail 为空时只回前缀，
// 保证 IsOfflineReason 恒真（匹配靠前缀，不靠括号内容）。
func OfflineReason(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ReasonOffline
	}
	return ReasonOffline + "（" + detail + "）"
}

// IsOfflineReason 判断禁用原因是否为「已掉线」类。
func IsOfflineReason(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), ReasonOffline)
}

// IsManualDisableReason 判断禁用原因是否为面板手动禁用。
func IsManualDisableReason(reason string) bool {
	return strings.HasPrefix(strings.TrimSpace(reason), strings.TrimSuffix(ReasonManualDisable, "（管理员操作）"))
}

// offlineReasonMarkers 历史口径的掉线原因文本。
//
// 本文件接管「掉线」这一类之前，同一个事实（token 已被上游拒收、只能重登）被写进
// 三种文案里，它们至今还留在老账号的持久化原因中：
//   - "12153 session dead"：pool.NoteSessionDead 连续 3 次达阈的持久化原因；
//   - "refresh session dead"：面板单号刷新路径（internal/server/handler.go）判死；
//   - "上游 12153 session dead（业务码明说登录态失效，需重新登录）"：导入校验路径。
//
// 「删除所有已掉线」若只认「已掉线」前缀，升级前就判死的号会永远删不掉——它们早就
// 打不动了，只是原因字符串还没换成新口径。
var offlineReasonMarkers = []string{
	"session dead", "12153", "refresh", "401",
	"掉线", "会话已失效", "登录态失效", "登录状态失效", "凭据失效", "token 失效",
	"需要重新登录", "需重新登录", "重新登录",
}

// notOfflineReasonMarkers 明确**不属于**掉线的禁用原因：上游风控封禁（11140）与
// 面板手动禁用。封禁号可能过几天被放行，删掉就再也回不来；手动禁用是运维的决定。
var notOfflineReasonMarkers = []string{
	"11140", "restricted", "unusual activity", "request illegal", "封禁", "风控",
}

// IsOfflineReasonText 判断任意禁用原因文本是否指向「账号已掉线」，兼容本文件接管
// 前的历史文案（见 offlineReasonMarkers）。
func IsOfflineReasonText(reason string) bool {
	s := strings.TrimSpace(reason)
	if s == "" {
		return false
	}
	if IsOfflineReason(s) {
		return true
	}
	if IsManualDisableReason(s) {
		return false
	}
	r := strings.ToLower(s)
	for _, m := range notOfflineReasonMarkers {
		if strings.Contains(r, m) {
			return false
		}
	}
	for _, m := range offlineReasonMarkers {
		if strings.Contains(r, m) {
			return true
		}
	}
	return false
}

// IsOfflineDisabled 判断一个账号状态是否属于「掉线类禁用」。列表分流、「删除所有已
// 掉线」、每日探测共用这一条口径，避免各处自己比字符串。
func IsOfflineDisabled(st Status) bool {
	return st.Disabled && IsOfflineReasonText(st.DisabledReason)
}

// DisableOffline 401 掉线禁用：签到/任务/余额刷新撞到 401 时立即停用这个号。
//
// 为什么 401 要直接禁用（而不是等连续 3 次 session dead 达阈）：401 是「凭据已不被
// 上游接受」的**同步**事实，这个号现在发任何请求都只会再吃一次 401；继续留它参与
// 选号，就是拿真实用户的对话去撞墙。连续计数的阈值是给「可能是瞬时拦截」的场景
// 留的缓冲，而调用方在这里已经用 refresh 求证过（VerifySessionByRefresh 判 Alive
// 的不会走到这里）。
//
// 返回 true = 本次由「可用」变为「禁用」；账号不存在/本来就禁用返回 false。
func (p *Pool) DisableOffline(uid, detail string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	was := e.disabled
	p.disableLocked(e, OfflineReason(detail))
	return !was
}
