package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// healFake 假上游：余额端点恒 401；refresh 端点按 account 返回 12153 或新 token。
type healFake struct {
	mu         sync.Mutex
	refresh    int    // refresh 被调用次数
	probe      int    // 余额探测次数
	sessionDie bool   // refresh 是否回 12153
	dieMsg     string // 12153 的文案（默认英文原文；另有本地化变体用例）
}

func (f *healFake) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			f.refresh++
			if f.sessionDie {
				msg := f.dieMsg
				if msg == "" {
					msg = "Offline user session not found"
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"code":12153,"msg":%q}`, msg)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":5184000}}`)
			return
		}
		f.probe++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func healScheduler(t *testing.T, srvURL string, a *auth.Auth) *Scheduler {
	t.Helper()
	up := testUpClient(upstream.Runtime{ChatBaseCN: srvURL, BillingBaseCN: srvURL, WebBaseCN: srvURL}, &http.Client{})
	p := pool.New(nil)
	p.Add(a)
	return New(Config{Pool: p, Upstream: up})
}

// refresh 回 12153 = 会话真死：401 直接禁用并写明「已掉线」（用户口径：401 直接封禁），
// 不再等 3 次计数——真死号每轮重试只是白打上游。
func TestBalanceProbe401RefreshSaysSessionDead(t *testing.T) {
	f := &healFake{sessionDie: true}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	s.RunBalanceRefreshNow()
	st, _ := s.cfg.Pool.Status("u1")
	if !st.Disabled {
		t.Fatalf("refresh 判死必须当场禁用（面板靠它把号移出轮转）: %+v", st)
	}
	if !pool.IsOfflineReason(st.DisabledReason) {
		t.Fatalf("禁用原因=%q want 前缀「%s」", st.DisabledReason, pool.ReasonOffline)
	}
	if f.refresh != 1 {
		t.Fatalf("refresh 调用=%d want 1", f.refresh)
	}
}

// refresh 成功 = 会话仍活（降级 token）：换新 token、清计数、不判死；
// 且 30 分钟内再探测不许重复刷新（否则一天 288 次 RT 轮换）。
func TestBalanceProbe401RefreshOKIsDegradedToken(t *testing.T) {
	f := &healFake{}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	s.RunBalanceRefreshNow()
	st, _ := s.cfg.Pool.Status("u1")
	if st.SessionDeadFails != 0 || st.Disabled {
		t.Fatalf("refresh 成功说明会话仍活，不该计罚: %+v", st)
	}
	if got := a.AccessTokenValue(); got != "at-new" {
		t.Fatalf("accessToken=%q want at-new（求证顺手换新 token）", got)
	}
	if f.refresh != 1 {
		t.Fatalf("refresh 调用=%d want 1", f.refresh)
	}
	// 第二轮：退避窗口内不再刷新，但仍照常探测（409 记账不重复）。
	s.RunBalanceRefreshNow()
	if f.refresh != 1 {
		t.Fatalf("退避窗口内 refresh 调用=%d want 1（每轮刷新=一天 288 次 RT 轮换）", f.refresh)
	}
	if f.probe != 2 {
		t.Fatalf("余额探测=%d want 2（退避只挡 refresh，不挡探测）", f.probe)
	}
}

// 非 401 不参与求证：5xx/限流/传输层各有既有惩罚路径，不在这里重复计罚。
// 直接喂 upstream.Error 打靶，避免被 UserResourceSnapshot 的重试/包装层干扰。
func TestBalanceProbeNonAuthDoesNotRefresh(t *testing.T) {
	f := &healFake{}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	for _, err := range []error{
		&upstream.Error{Kind: upstream.ErrUpstreamBusy, Status: http.StatusInternalServerError, Msg: "boom"},
		&upstream.Error{Kind: upstream.ErrSoftRate, Status: http.StatusTooManyRequests, Msg: "429 rate limit"},
		&upstream.Error{Kind: upstream.ErrClient, Status: http.StatusForbidden, Msg: "forbidden"},
	} {
		s.noteBalanceAuthFailure(a, "u1", err)
	}
	if f.refresh != 0 {
		t.Fatalf("非 401 不该触发 refresh 求证，实际 %d 次", f.refresh)
	}
	if st, _ := s.cfg.Pool.Status("u1"); st.SessionDeadFails != 0 {
		t.Fatalf("非 401 不该计入 session dead: %+v", st)
	}
}

// 退避只记账「成功求证」：真死号必须每轮都试，否则连续计数永远停在 1/阈。
func TestAuthHealBackoffOnlyOnSuccess(t *testing.T) {
	s := New(Config{})
	if !s.allowAuthHeal("u1") {
		t.Fatal("首次应允许求证")
	}
	s.noteAuthHeal("u1")
	if s.allowAuthHeal("u1") {
		t.Fatal("成功后应进入退避窗口")
	}
	// 模拟窗口过期。
	s.authHealMu.Lock()
	s.authHealAt["u1"] = time.Now().Add(-authHealBackoff - time.Minute)
	s.authHealMu.Unlock()
	if !s.allowAuthHeal("u1") {
		t.Fatal("窗口过期后应恢复求证")
	}
	if !s.allowAuthHeal("u2") {
		t.Fatal("不同账号互不影响")
	}
}

// 12153 文案本地化（不含英文 marker）时也必须判死：doJSON 只拿 env.Msg 做分类，
// 靠文案匹配会漏，这里按 code 兜一层。
func TestBalanceProbe401SessionDeadLocalizedMessage(t *testing.T) {
	f := &healFake{sessionDie: true, dieMsg: "会话已失效，请重新登录"}
	srv := f.server(t)
	a := &auth.Auth{UID: "u1", AccessToken: "at-old", RefreshToken: "rt-1", ExpiresAt: 1}
	s := healScheduler(t, srv.URL, a)

	s.RunBalanceRefreshNow()
	st, _ := s.cfg.Pool.Status("u1")
	if !st.Disabled || !pool.IsOfflineReason(st.DisabledReason) {
		t.Fatalf("本地化 12153 文案也必须判死并标「已掉线」: %+v", st)
	}
}
