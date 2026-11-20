package panel

import (
	"log"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/offline"
)

// 面板手工运维（签到/刷新余额/任务中心）撞到 401 的统一处置。
// 与 scheduler.noteAuth401 同一口径（用户口径 2026-11-21：401 直接封禁，原因
// 「已掉线」）：面板是运维唯一能立刻看到结果的地方，401 之后还把号留在「可用」
// 列表里，下一次对话就会再撞一次 401。
//
// 返回 true 表示这次错误已按登录态处置（禁用或换 token），调用方不必再当业务错误
// 解释；返回 false 表示不是登录态问题（5xx/限流/业务拒绝），照旧走原有错误分支。
func (p *Panel) noteAuth401(a *auth.Auth, uid, action string, err error) offline.Outcome {
	if a == nil {
		return offline.NotAuth
	}
	ue, hit := offline.Signal(err)
	if !hit {
		return offline.NotAuth
	}
	label := logfmt.Label(uid, a.Nickname)
	if a.RefreshTokenValue() == "" {
		p.markOffline(uid, label, action+"："+offline.Describe(ue, "静态凭证 401（无 refresh token）"))
		return offline.Offline
	}
	verdict, detail := offline.Judge(p.cfg.Upstream, a, true)
	switch verdict {
	case offline.Alive:
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("WARN: panel %s uid=%s: 401 后刷新成功但落盘失败: %v", action, uid, serr)
		}
		p.cfg.Pool.ClearSessionDead(uid)
		log.Printf("INFO: panel %s uid=%s: 401 经 refresh 求证为降级 token（会话仍活），已换新 token", action, uid)
		return offline.Healed
	case offline.Dead:
		p.markOffline(uid, label, action+"："+detail)
		return offline.Offline
	default:
		log.Printf("WARN: panel %s uid=%s: 401 但 refresh 未给出确定判词（%s），不动账号状态", action, uid, detail)
		return offline.NotAuth
	}
}

// markOffline 把账号判为「已掉线」并禁用（原因由 pool.OfflineReason 统一组装）。
func (p *Panel) markOffline(uid, label, detail string) {
	if p.cfg.Pool == nil {
		return
	}
	if p.cfg.Pool.DisableOffline(uid, detail) {
		log.Printf("WARN: %s 已掉线（%s）— 已禁用，需重新登录（用户口径：401 直接封禁）", label, detail)
		return
	}
	log.Printf("INFO: %s 401 判死，但账号已是禁用态（%s），不改状态", label, detail)
}
