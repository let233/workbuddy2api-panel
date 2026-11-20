// restrict_test.go 11140 风控保护的两道闸：
//  1. 命中即禁用该号，并在 upstream.restriction_failover 预算内换一个号（0 = 命中即停手）——
//     号池里还有健康号时用户该拿到正常回答，但「一个请求顺着轮转撞穿整池」必须被预算挡住；
//  2. 同一域 15 分钟窗内 2 个不同账号被限制 → 暂停该域派发（期间不打上游，503 + Retry-After），
//     暂停中一律停手：换号预算不越过连坐闸。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

const restrictBody = `{"code":11140,"msg":"账号存在异常行为，已被限制使用","requestId":"aaaabbbbccccdddd"}`

func postChatModel(h *Handler, model string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	return rec
}

func postChatRestricted(h *Handler) *httptest.ResponseRecorder { return postChatModel(h, "glm-5.2") }

// 单号反复命中不凑数；hold<=0 永不暂停；两个域互不牵连。
func TestRestrictionGate(t *testing.T) {
	var g restrictionGate
	if g.note("cn", "a", time.Minute, 0) || g.note("cn", "a", time.Minute, 0) {
		t.Fatal("同一个账号重复命中不该凑成第二个号")
	}
	if g.note("global", "b", time.Minute, 0) {
		t.Fatal("另一个域的命中不该凑进 cn 的计数")
	}
	if !g.note("cn", "b", time.Minute, 0) {
		t.Fatal("同域窗口内第 2 个不同账号被限制应触发暂停")
	}
	if d := g.remaining("cn"); d <= 0 || d > time.Minute {
		t.Fatalf("cn remaining=%v want (0, 1m]", d)
	}
	if d := g.remaining("global"); d != 0 {
		t.Fatalf("cn 的暂停不该波及 global, remaining=%v", d)
	}

	var off restrictionGate
	for _, uid := range []string{"a", "b", "c"} {
		if off.note("cn", uid, 0, 0) {
			t.Fatal("hold=0 时不该暂停")
		}
	}
}

// 不带 code 11140 的 "request illegal"：不是账号风控限制，照常换号，也不计入连坐。
func TestChatRequestIllegalWithoutCodeKeepsRotating(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, `{"code":10001,"msg":"request illegal"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "i1", AccessToken: "at-i1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "i2", AccessToken: "at-i2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: time.Minute, RestrictionFailover: 1, MaxRotate: 3})
	postChatModel(h, "glm-5.2")
	if calls != 2 {
		t.Fatalf("calls=%d want 2（非 11140 不该停轮转）", calls)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("非 11140 不该凑成连坐暂停, remaining=%v", d)
	}
}

// 命中 11140：被扣的号禁用，预算内换一个健康号，用户拿到正常回答（非流式聚合）。
func TestChatRestrictionFailsOverToHealthyAccount(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if strings.Contains(authz, "at-f1") {
			return 403, restrictBody, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "f1", AccessToken: "at-f1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "f2", AccessToken: "at-f2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: time.Minute, RestrictionFailover: 1, MaxRotate: 3})
	rec := postChatModel(h, "glm-5.2")

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（预算内换号后应成功，而不是把封禁原文回给客户端）", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Wb-Account"); got != "f2" {
		t.Fatalf("X-Wb-Account=%q want f2（成功落在健康号上）", got)
	}
	if calls["Bearer at-f1"] != 1 || calls["Bearer at-f2"] != 1 {
		t.Fatalf("calls=%v want 各 1 次（被限制的那个号不再重试）", calls)
	}
	if _, _, _, n, _ := p.CountsDetailed(); n != 1 {
		t.Fatalf("被限制的号应禁用, disabled=%d", n)
	}
	if d := h.restrict.remaining("cn"); d != 0 {
		t.Fatalf("单个号被限制不该触发连坐暂停, remaining=%v", d)
	}
}

// 流式首帧就是 11140 文书（HTTP 200 内嵌 {code,msg}）：首帧否决之后同样在预算内换号。
func TestChatStreamHeadRestrictionFailsOver(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if strings.Contains(authz, "at-s1") {
			return 200, "data: " + restrictBody + "\n\ndata: [DONE]\n\n", true
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "s1", AccessToken: "at-s1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "s2", AccessToken: "at-s2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: time.Minute, RestrictionFailover: 1, MaxRotate: 3})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("首帧 11140 应在预算内换号后正常出流: code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-s1"] != 1 || calls["Bearer at-s2"] != 1 {
		t.Fatalf("calls=%v want 各 1 次", calls)
	}
	if _, _, _, n, _ := p.CountsDetailed(); n != 1 {
		t.Fatalf("被限制的号应禁用, disabled=%d", n)
	}
}

// upstream.restriction_failover=0：命中即停手（旧行为），一个号都不换。
func TestChatRestrictionFailoverZeroStopsRotation(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "z1", AccessToken: "at-z1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "z2", AccessToken: "at-z2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: time.Minute, MaxRotate: 3})
	postChatModel(h, "glm-5.2")

	if calls != 1 {
		t.Fatalf("calls=%d want 1（预算 0 = 命中即停手）", calls)
	}
	if _, _, _, n, _ := p.CountsDetailed(); n != 1 {
		t.Fatalf("被限制的号应禁用, disabled=%d", n)
	}
}

// 连坐闸优先于换号预算：预算给到 5，窗内第 2 个号被限制就暂停该域并停手——绝不顺着轮转
// 把整池挨个撞一遍。暂停期内该域 503、不打上游；另一个域照常放行。
func TestChatRestrictionWaveStopsDespiteFailoverBudget(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "w1", AccessToken: "at-w1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "w2", AccessToken: "at-w2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "w3", AccessToken: "at-w3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, RestrictionHold: 30 * time.Minute, RestrictionFailover: 5, MaxRotate: 6})

	postChatModel(h, "glm-5.2")
	if calls != 2 {
		t.Fatalf("calls=%d want 2（第 2 个号触发连坐暂停即停手，预算再大也不越过连坐闸）", calls)
	}
	if d := h.restrict.remaining("cn"); d <= 0 {
		t.Fatalf("应触发连坐暂停, remaining=%v", d)
	}
	rec := postChatModel(h, "glm-5.2")
	if calls != 2 || rec.Code != 503 || !strings.Contains(rec.Body.String(), "account_restricted_hold") ||
		rec.Header().Get("Retry-After") == "" {
		t.Fatalf("暂停期内应 503 + Retry-After 且不打上游: calls=%d code=%d body=%s", calls, rec.Code, rec.Body)
	}
	if rec := postChatModel(h, "global:glm-5.2"); strings.Contains(rec.Body.String(), "account_restricted_hold") {
		t.Fatalf("cn 的暂停不该挡 global 请求: %s", rec.Body)
	}
}

// hold=0（关掉连坐暂停）：换号预算仍然有界，一个请求最多打到「1 + budget」个号。
func TestChatRestrictionNoHoldKeepsFailoverBounded(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 403, restrictBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "h1", AccessToken: "at-h1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "h2", AccessToken: "at-h2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "h3", AccessToken: "at-h3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, RestrictionFailover: 1})

	postChatModel(h, "glm-5.2")
	if calls != 2 {
		t.Fatalf("calls=%d want 2（1 个号 + 1 个预算，与 max_rotate 3 无关）", calls)
	}
	if h.restrict.remaining("cn") != 0 {
		t.Fatalf("hold=0 时不该暂停, remaining=%v", h.restrict.remaining("cn"))
	}
	// 第二个请求：只剩一个未禁用号，打一次后池子空了 → 直接失败，不会无限换号。
	rec := postChatModel(h, "glm-5.2")
	if calls != 3 {
		t.Fatalf("calls=%d want 3（每个未禁用号各命中一次）", calls)
	}
	if strings.Contains(rec.Body.String(), "account_restricted_hold") {
		t.Fatalf("hold=0 不该回连坐暂停: %s", rec.Body)
	}
	if _, _, _, n, _ := p.CountsDetailed(); n != 3 {
		t.Fatalf("被限制的号应全部禁用, disabled=%d", n)
	}
}
