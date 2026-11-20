// Package offline 判定「账号已掉线」（上游 401 且登录态确认失效），供面板与排程共用。
//
// 用户口径（2026-11-21）：执行签到 / 任务 / 刷新余额时**遇到 401 直接封禁该账号**，
// 原因是「已掉线」——以前要连续 3 次才禁用（sessionDeadThreshold），运维看到的是
// 「面板显示可用、实际打不动」，或者干脆几天后才发现。
//
// 但 401 这个信号本身是二义的（issue #215 实测）：网关拦 401 与登录态失效**响应体
// 完全一样**，而「降级 token」（billing 恒 401、chat 仍可用）的号还能正常干活。
// 见到 401 就禁用等于用一次网关抖动杀掉一个健康账号。所以这里的「直接」指**不再等
// 计数阈值**，但仍然要一次 refresh 求证：
//
//	refresh 成功          → 会话活着（降级 token 或网关瞬时拦截）→ 顺手换新 token，不禁用
//	refresh 回 12153/判死 → 登录态真失效 → 立刻禁用（reason = 已掉线）
//	refresh 其它失败      → 无结论（网络/限流）→ 不动，下一轮再说
//
// 静态 API Key 没有 refresh 可言：401 就是凭据失效，直接判掉线。
package offline

import (
	"errors"
	"fmt"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// Outcome 调用点对「这次 401 该怎么接着做」的处置结果。
//
// 与 Verdict 的区别：Verdict 是判断（活/死/不知道），Outcome 是判断之后**调用点
// 的下一步**——他只需要知道「能不能继续打这个号」，不必关心 refresh 细节。
type Outcome int

const (
	// NotAuth 不是登录态问题（5xx/限流/业务拒绝），或没有确定判词：照旧走原有错误分支。
	NotAuth Outcome = iota
	// Healed 降级 token：已换到新 token，账号仍可用，后续步骤照常进行。
	Healed
	// Offline 已判掉线并禁用：这个号本轮不必再打任何上游接口。
	Offline
)

// Verdict 一次 401 求证之后的结论。
type Verdict string

const (
	// Alive 会话仍有效：401 是网关/风控瞬时拦截，或降级 token（billing 401、chat 可用）。
	Alive Verdict = "alive"
	// Dead 登录态确认失效：按「已掉线」处理。
	Dead Verdict = "dead"
	// Unknown 无结论（网络、限流、未求证）：不动账号状态。
	Unknown Verdict = "unknown"
)

// Signal 判断错误是否是「登录态可疑」信号（401 或 ErrSessionDead）。
// 返回上游错误对象便于调用方记录 status/message。
func Signal(err error) (*upstream.Error, bool) {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return nil, false
	}
	if ue.Status == 401 || ue.Kind == upstream.ErrSessionDead {
		return ue, true
	}
	return nil, false
}

// Judge 对一次 401 做求证，给出「是否已掉线」的结论。
//
// heal=false 时跳过 refresh（调用方有自己的退避，例如余额刷新每 5 分钟一轮，
// 每轮都换 RT 会放大风控），直接返回 Unknown。
//
// 免 import 循环：本包只依赖 auth/upstream，pool 与 scheduler 各自调用后自行
// 决定 Disable 与否（pool 不知道 upstream 的存在，反过来也不行）。
func Judge(c *upstream.Client, a *auth.Auth, heal bool) (Verdict, string) {
	if a == nil {
		return Unknown, "账号凭据已不在内存"
	}
	// 静态 API Key / 无 refresh token：没有求证路径，401 即凭据失效。
	if a.RefreshTokenValue() == "" {
		return Dead, "静态凭证 401（无 refresh token 可求证）"
	}
	if !heal || c == nil {
		return Unknown, "跳过 refresh 求证"
	}
	err := c.RefreshToken(a)
	if err == nil {
		// refresh 成功 = 会话活着（降级 token 的号换一次 token 就恢复）。落盘由调用方
		// 做——它才知道失败该记什么日志（本包只做判断）。
		return Alive, "refresh 成功（会话仍有效）"
	}
	var ue *upstream.Error
	msg := err.Error()
	if errors.As(err, &ue) {
		msg = ue.Msg
		// 12153 = 会话不存在（登录态被注销）。业务码比 HTTP 状态更权威：
		// 上游本地化文案可能不含英文 marker，但 code 恒定。
		if ue.Kind == upstream.ErrSessionDead || strings.Contains(ue.Msg, "12153") {
			return Dead, "refresh 判死: " + clip(ue.Msg)
		}
		return Unknown, "refresh 失败（非会话失效）: " + clip(msg)
	}
	return Unknown, "refresh 失败: " + clip(msg)
}

// Describe 生成禁用原因里的细节串（pool.OfflineReason 会包装成「已掉线（…）」）。
func Describe(ue *upstream.Error, detail string) string {
	if detail != "" {
		return detail
	}
	if ue != nil {
		return fmt.Sprintf("HTTP %d: %s", ue.Status, clip(ue.Msg))
	}
	return ""
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
