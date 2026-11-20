// banprobe_test.go 每日封禁探测的**写侧**：探测结论如何改变池状态。
//
// 探测本身（internal/banprobe）只读；这里钉住两种写：
//   - session_dead：登录态被证死 → 原因收敛成「已掉线」，否则升级前留下的
//     "12153 session dead" 号永远进不了「删除所有已掉线」的范围；
//   - blocked（会话仍活、请求被风控拦）：原因保持原样，别把封禁改写成掉线。
package scheduler

import (
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// 探测到 session_dead：禁用原因被收敛为「已掉线」，原原因留在括号里对账。
func TestBanProbeSessionDeadConvergesReason(t *testing.T) {
	f := &healFake{sessionDie: true}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)
	s.cfg.Pool.Disable("u1", "12153 session dead")

	s.runBanProbe(t.Context(), false)

	st, ok := s.cfg.Pool.Status("u1")
	if !ok || !st.Disabled {
		t.Fatalf("探测后账号仍应禁用（掉线号不能回池）: %+v", st)
	}
	if !pool.IsOfflineReason(st.DisabledReason) {
		t.Fatalf("原因=%q want 前缀「%s」（否则「删除所有已掉线」认不出它）", st.DisabledReason, pool.ReasonOffline)
	}
	if !strings.Contains(st.DisabledReason, "12153 session dead") {
		t.Fatalf("原因=%q 应保留原原因便于对账", st.DisabledReason)
	}
}

// 探测结论是 blocked（会话仍活、请求被风控拦）：原因原样保留，不能误判成掉线。
func TestBanProbeBlockedKeepsReason(t *testing.T) {
	f := &healFake{} // chat 401，refresh 成功 → Blocked
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)
	const banReason = "account restricted by upstream (11140 request illegal)"
	s.cfg.Pool.Disable("u1", banReason)

	s.runBanProbe(t.Context(), false)

	st, _ := s.cfg.Pool.Status("u1")
	if st.DisabledReason != banReason {
		t.Fatalf("原因=%q want %q（封禁不是掉线，改写会误导运维）", st.DisabledReason, banReason)
	}
}

// 面板手动禁用的号不探：管理员下线的号不该被探测结论翻案。
func TestBanProbeSkipsManualDisabled(t *testing.T) {
	f := &healFake{sessionDie: true}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)
	s.cfg.Pool.Disable("u1", pool.ReasonManualDisable)

	s.runBanProbe(t.Context(), false)

	if f.probe != 0 || f.refresh != 0 {
		t.Fatalf("手动禁用号不该被探测（probe=%d refresh=%d）", f.probe, f.refresh)
	}
	if st, _ := s.cfg.Pool.Status("u1"); st.DisabledReason != pool.ReasonManualDisable {
		t.Fatalf("原因被改写: %q", st.DisabledReason)
	}
}
