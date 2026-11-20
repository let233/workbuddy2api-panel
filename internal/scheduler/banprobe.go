// banprobe.go 每日「封禁探测」排程（schedule.ban_probe_*）。
//
// 用户口径（2026-11-21）：账号池里被封禁的号要定期**真实发一次 AI 对话**去问上游
// 「还在封吗」，一旦上面放行就把池里的号同步解冻——否则运维只能靠人盯面板，或者
// 干脆全部手动解冻撞运气。
//
// 只探**已禁用**的账号：可用号每轮都在真实流量里被验证，探它们纯属白打请求；
// 而封禁号不干活，除了探针没有任何信号能证明它回来了。
//
// 与 windowprobe.go 的分工：那个探「额度窗口是否重置」（清模型冷却），这个探
// 「账号是否解封」（解冻 disabled）。判词都来自上游原文，不做乐观推断。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/banprobe"
	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// banProbeGap 同一轮里两个账号之间的间隔。
//
// 探针打的是 chat 通道，同一个出口 IP 上连打一串是最容易被识别的形状；封禁号本来
// 就在风控名单上，这里更要慢。400ms 与额度窗口探针同口径（量级对、感知不到）。
const banProbeGap = 400 * time.Millisecond

// banProbeParams 探测参数快照（热配置，循环开头取一次，与 Reconfigure 隔离）。
func (s *Scheduler) banProbeParams() (autoRevive bool, model string) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.BanProbeAutoRevive, s.cfg.BanProbeModel
}

// BanProbeModel 探测用模型 id（空则由 banprobe 取默认）。
func (s *Scheduler) BanProbeModel() string {
	_, model := s.banProbeParams()
	if model == "" {
		return banprobe.DefaultModel
	}
	return model
}

// BanProbeNow 立即跑一轮封禁探测（面板「立即探测」用，等价于排程那一轮）。
func (s *Scheduler) BanProbeNow() {
	s.runBanProbe(context.Background(), false)
}

// runBanProbe 遍历已禁用账号，逐个真实发一次对话。
//
// spread=true（排程路径）时按账号级偏移错开，见 spread.go。
//
// 判定与处置（全部只读探测，唯一的写是「确实已解封」时的解冻）：
//   - lifted      上游已放行；auto_revive 打开则同步解冻，否则只记日志等运维点。
//   - still_banned / blocked  仍被封：保持禁用，不改原因（原因指向当时的判词）。
//   - session_dead 登录态失效（不是封禁）：保持禁用；这类号该重登或清掉，
//     解冻只会让它立刻再死一次。
//   - inconclusive 网络/限流等：什么都不做，明天再说。
//
// 「面板手动禁用（管理员操作）」的号**不探**：它是人主动下线的，不是上游判的，
// 自动解冻等于把管理员的决定撤销掉（见 pool.IsManualDisableReason）。
func (s *Scheduler) runBanProbe(ctx context.Context, spread bool) {
	if s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	plan := s.spreadPlan(taskBanProbe, spread, cst.Now())
	autoRevive, model := s.banProbeParams()
	var probed, lifted, revived int
	for _, st := range plan.order(s.cfg.Pool.List()) {
		// 只探已封禁的：可用号不需要问「解封了吗」。
		if !st.Disabled {
			continue
		}
		if pool.IsManualDisableReason(st.DisabledReason) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		// 无凭据的号探不了（API Key 除外：它自带静态凭据，没有 refresh 也能发请求）。
		if a == nil || (a.RefreshTokenValue() == "" && !a.IsAPIKey()) {
			continue
		}
		if probed > 0 {
			if !sleepCtx(ctx, banProbeGap) {
				return // 停机/取消：剩余账号明天再探
			}
		}
		if !plan.wait(ctx, st.UID) {
			return
		}
		res := banprobe.Probe(ctx, s.cfg.Upstream, a, model)
		probed++
		log.Printf("INFO: [banprobe] %s verdict=%s status=%d detail=%s",
			shortUID(st.UID), res.Verdict, res.Status, orNone(res.Detail))
		if res.Verdict == banprobe.SessionDead {
			// 探测自己把「登录态失效」证死了（refresh 判死）。此时把原因收敛成「已掉线」：
			// 升级前留下的 12153 session dead / refresh session dead 不在「删除所有已掉线」
			// 的范围内，运维看到的是一个再也用不了、却删不掉的号。原原因留在括号里对账。
			if !pool.IsOfflineReason(st.DisabledReason) {
				s.cfg.Pool.DisableOffline(st.UID, "封禁探测：登录态已失效，需重新登录（原原因："+orNone(st.DisabledReason)+"）")
				log.Printf("INFO: [banprobe] %s 登录态失效 — 原因已收敛为「%s」，可被「删除所有已掉线」清掉",
					shortUID(st.UID), pool.ReasonOffline)
			}
			continue
		}
		if res.Verdict != banprobe.Lifted {
			continue
		}
		lifted++
		if !autoRevive {
			log.Printf("INFO: [banprobe] %s 上游已解封（未开启自动解冻，等管理员确认）", shortUID(st.UID))
			continue
		}
		// ReviveDisabled 不返回是否命中（号可能刚被运维手动删/解冻），这里按「本轮
		// 确实探到已封禁且把解冻命令发出去」计数，日志里带上原原因便于对账。
		s.cfg.Pool.ReviveDisabled(st.UID)
		revived++
		log.Printf("WARN: [banprobe] %s 上游已解封 — 已同步解冻（原原因：%s）",
			shortUID(st.UID), orNone(st.DisabledReason))
	}
	if probed > 0 {
		log.Printf("INFO: [banprobe] 本轮完成：探测 %d 个封禁账号（解封 %d，同步解冻 %d）",
			probed, lifted, revived)
	}
}
