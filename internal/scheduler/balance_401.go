package scheduler

// 余额探测 401 的「登录态求证」（借鉴 fork Albert-PZY 9f264ca + c6a3900 / issue #215）：
// 余额刷新是池内唯一的分钟级健康观测量，但拿到错误此前只写一行日志、不碰池状态——
// 上游批量注销会话后，号在 billing/chat 两侧都已 401，面板仍显示「可用」，要等每日
// 22:00 的保活连败满阈（≈3 天）才转 disabled，运维期间看不到任何信号。
//
// 不能把 401 直接当 session dead 计罚：401 有两种成因，
//  1. 会话被上游注销（refresh 回 12153）→ 需重登，应计罚；
//  2. 降级 token（issue #215 评论实测：billing 恒 401、chat 仍可用）→ 号还能干活，计罚会误杀。
// 用 refresh 做一次权威求证即可分开。非 401 不参与：5xx/限流/传输层各有既有惩罚路径。

import (
	"log"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/logfmt"
	"github.com/misakano7545/workbuddy2api-panel/internal/offline"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

func (s *Scheduler) noteBalanceAuthFailure(a *auth.Auth, uid string, probeErr error) {
	ue, hit := offline.Signal(probeErr)
	if !hit {
		return
	}
	label := logfmt.Label(uid, a.Nickname)
	// 静态凭据（API Key 等）没有 refresh 路径：401 就是凭据失效，直接判掉线。
	if a.RefreshTokenValue() == "" {
		s.markOffline(uid, label, offline.Describe(ue, "静态凭证 401（无 refresh token）"))
		return
	}
	// 退避只记「refresh 成功」：真死号每轮 refresh 都回 12153 且不轮换 RT，必须每轮都
	// 试；把失败也记进退避会让死号一直停在「没判死」。
	if !s.allowAuthHeal(uid) {
		log.Printf("balance %s: 401（%s 内已求证过登录态，本轮跳过 refresh）", label, authHealBackoff)
		return
	}
	verdict, detail := offline.Judge(s.cfg.Upstream, a, true)
	switch verdict {
	case offline.Alive:
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("balance %s: 401 后刷新成功但落盘失败: %v", label, serr)
		}
		s.noteAuthHeal(uid)
		s.cfg.Pool.ClearSessionDead(uid)
		log.Printf("balance %s: 401 经 refresh 求证为降级 token（会话仍活），已换新 token 并清 session dead 计数", label)
	case offline.Dead:
		// refresh 判死但 AT 还没过期：chat 侧此刻可能仍可用（issue #83，与 keepalive
		// 同一判据）。直接禁用会砍掉一个还能干活的号，所以这里只累计计数并在日志里
		// 说清楚；AT 一过期 NeedsRefresh 即真，下轮 401 就按「已掉线」直接禁用。
		if upstream.SessionDeadButUsable(a) {
			s.cfg.Pool.NoteSessionDead(uid)
			log.Printf("balance %s: %s，但 access token 仍有效（chat 可能仍可用）—— 暂不禁用，AT 过期后按已掉线处理", label, detail)
			return
		}
		s.markOffline(uid, label, detail)
	default:
		log.Printf("balance %s: 401 但 refresh 未给出确定判词（%s），本轮不动账号状态", label, detail)
	}
}

// markOffline 把账号判为「已掉线」并禁用（用户口径 2026-11-21：401 直接封禁，
// 不再等连续 3 次计数——三次意味着面板上最长有十几分钟显示「可用」却什么都打不动）。
func (s *Scheduler) markOffline(uid, label, detail string) {
	if s.cfg.Pool == nil {
		return
	}
	if s.cfg.Pool.DisableOffline(uid, detail) {
		log.Printf("WARN: %s 已掉线（%s）— 已禁用，需重新登录（用户口径：401 直接封禁）", label, detail)
		return
	}
	log.Printf("WARN: %s 401 判死，但账号已是禁用态（%s），不改状态", label, detail)
}

// authHealBackoff 同一账号 401→refresh 求证的最小间隔。降级 token 每轮探测都会 401，
// 若每轮都刷新，一天会轮换 288 次 RT。
const authHealBackoff = 30 * time.Minute

// allowAuthHeal 退避窗口判定。
func (s *Scheduler) allowAuthHeal(uid string) bool {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if t, ok := s.authHealAt[uid]; ok && time.Since(t) < authHealBackoff {
		return false
	}
	return true
}

// noteAuthHeal 记录一次「refresh 成功」的求证时刻。
func (s *Scheduler) noteAuthHeal(uid string) {
	s.authHealMu.Lock()
	defer s.authHealMu.Unlock()
	if s.authHealAt == nil {
		s.authHealAt = make(map[string]time.Time)
	}
	s.authHealAt[uid] = time.Now()
}
