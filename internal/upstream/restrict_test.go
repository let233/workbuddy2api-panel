package upstream

import (
	"testing"
)

// 11140 的几种形态各归各位：只有「code + 账号专属措辞」才是风控限制；同 code 的模型级
// 限制 / 限流、只有措辞没有 code 的都不算。
func TestIsRestricted(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, true},
		{`{"code": 11140, "msg":"Your account is restricted due to Unusual Activity"}`, true},
		{`{"code":"11140","msg":"账号存在异常行为"}`, true},
		// 泛化措辞不算：模型 / 功能级的限制同样会说「已被限制」。
		{`{"code":11140,"msg":"该模型已被限制使用，请稍后再试"}`, false},
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":11141,"msg":"账号存在异常行为"}`, false},
		{`{"msg":"账号存在异常行为，已被限制使用"}`, false},
		{`{"code":11140,"msg":"request illegal"}`, false},
		{`{"code":50400,"msg":"账号存在异常行为"}`, false},
		{`账号存在异常行为，已被限制使用`, false},
	}
	for _, c := range cases {
		if got := IsRestricted(c.body); got != c.want {
			t.Errorf("IsRestricted(%s)=%v want %v", c.body, got, c.want)
		}
	}
}

// 流式首帧的 11140：帧里 code 在 error.code 上、不在 message 里，FrameKind 不能只拿 message
// 去 Classify（那样文书被判成普通 4xx，首帧否决后照样换号）。同 code 的限流帧保持软限流。
func TestFrameKindRestrictedFrame(t *testing.T) {
	cases := []struct {
		payload string
		want    ErrKind
	}{
		{`{"error":{"code":11140,"message":"账号存在异常行为，已被限制使用"}}`, ErrAccountFault},
		{`{"error":{"code":"11140","message":"Your account shows unusual activity"}}`, ErrAccountFault},
		{`{"error":{"code":11140,"message":"该模型已被限制使用"}}`, ErrClient},
	}
	for _, c := range cases {
		if got := FrameKind(c.payload); got != c.want {
			t.Errorf("FrameKind(%s)=%v want %v", c.payload, got, c.want)
		}
	}
}

func TestIsAccountRestrictionAndClassify(t *testing.T) {
	cases := []struct {
		status     int
		body       string
		restricted bool
		kind       ErrKind
	}{
		{403, `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, true, ErrAccountFault},
		{403, `{"code":11140,"msg":"request illegal"}`, true, ErrAccountFault},
		// 11-128 渠道校验：同一句 request illegal，但属于请求级可自愈拦截。
		{403, `{"code":"11-128","msg":"request illegal"}`, false, ErrAccountFault},
		// 别的 code 上的 request illegal：照旧按账号故障处置，但不是 11140 限制，不停轮转、不计连坐。
		{403, `{"code":10001,"msg":"request illegal"}`, false, ErrAccountFault},
		{403, `{"msg":"request illegal"}`, false, ErrAccountFault},
		{429, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false, ErrSoftRate},
		{403, `{"code":11140,"msg":"该模型已被限制使用"}`, false, ErrClient},
	}
	for _, c := range cases {
		if got := IsAccountRestriction(c.body); got != c.restricted {
			t.Errorf("IsAccountRestriction(%s)=%v want %v", c.body, got, c.restricted)
		}
		if got := Classify(c.status, c.body); got != c.kind {
			t.Errorf("Classify(%d, %s)=%v want %v", c.status, c.body, got, c.kind)
		}
	}
}

// TestClassifyRestrictionIsAccountFault 中文风控文书必须归账号级：落通用 4xx 兜底
// （ErrClient）只会「换号继续打」，一个请求就能把整池账号按顺序送进风控。
func TestClassifyRestrictionIsAccountFault(t *testing.T) {
	if got := Classify(403, `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`); got != ErrAccountFault {
		t.Fatalf("中文 11140 文书应归 ErrAccountFault，得到 %v", got)
	}
	// 同一 code 的模型限流不得被这条判据吃掉（仍是可自愈的软限流）。
	if got := Classify(429, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`); got != ErrSoftRate {
		t.Fatalf("11140 + 限流文案应保持 ErrSoftRate，得到 %v", got)
	}
}
