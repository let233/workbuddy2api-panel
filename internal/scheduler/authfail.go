package scheduler

import (
	"log"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/offline"
)

// 任务路径（签到/活跃/旅行/成长任务）撞到 401 的统一处置：与余额探测同一口径
// （用户口径 2026-11-21：401 直接封禁，禁用原因「已掉线」，不再等连续 3 次计数）。
//
// 为什么任务路径要自己判：签到/活跃/旅行的上游调用都在各自的 loop 里散着，每个
// 分支都写一遍「Signal → refresh 求证 → 禁用」必然走样。这里只做一件事——把
// 「这一次 401 是谁的问题」判清楚，然后交给 markOffline：
//   - refresh 成功 = 降级 token（billing/任务端点 401、chat 仍活）→ 换 token 后返回
//     false，调用方照常把这次失败当业务错误处理；
//   - refresh 判死 → 已掉线，直接禁用，返回 true；
//   - 判不出来（网络/5xx）→ 不动状态，返回 true 让调用方别再当 401 处理。
//
// 返回 true 表示「这个错误已经按登录态处置过」，调用方不该再对它做业务级重试。
func (s *Scheduler) noteAuth401(a *auth.Auth, uid, action string, err error) offline.Outcome {
	if a == nil {
		return offline.NotAuth
	}
	ue, hit := offline.Signal(err)
	if !hit {
		return offline.NotAuth
	}
	label := logfmt.Label(uid, a.Nickname)
	if a.RefreshTokenValue() == "" {
		s.markOffline(uid, label, action+"："+offline.Describe(ue, "静态凭证 401（无 refresh token）"))
		return offline.Offline
	}
	verdict, detail := offline.Judge(s.cfg.Upstream, a, true)
	switch verdict {
	case offline.Alive:
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("WARN: %s %s: 401 后刷新成功但落盘失败: %v", action, label, serr)
		}
		s.cfg.Pool.ClearSessionDead(uid)
		log.Printf("INFO: %s %s: 401 经 refresh 求证为降级 token（会话仍活），已换新 token", action, label)
		return offline.Healed
	case offline.Dead:
		s.markOffline(uid, label, action+"："+detail)
		return offline.Offline
	default:
		log.Printf("WARN: %s %s: 401 但 refresh 未给出确定判词（%s），本轮不动账号状态", action, label, detail)
		return offline.NotAuth
	}
}
