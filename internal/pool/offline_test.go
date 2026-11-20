// offline_test.go 「掉线」这一类禁用原因的**分类口径**。
//
// 为什么要钉住历史文案：运维报的是「12153 session dead / refresh session dead 这些号
// 明明也是掉线，却删不掉」——分类口径漏一个文案，「删除所有已掉线」就会留一堆死号在池里
// 天天吃 401。新增/修改文案时，这里的表决定了哪些原因还算「掉线」。
package pool

import "testing"

func TestIsOfflineReasonText(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
		why    string
	}{
		{OfflineReason("签到 401"), true, "新口径前缀"},
		{OfflineReason(""), true, "空 detail 也要认"},
		{"12153 session dead", true, "旧 NoteSessionDead 达阈的持久化原因"},
		{"refresh session dead", true, "面板单号刷新判死"},
		{"上游 12153 session dead（业务码明说登录态失效，需重新登录）", true, "导入校验路径的本地化文案"},
		{"网关 401 invalid_token", true, "网关层 401 判死"},
		{"会话已失效，请重新登录", true, "上游本地化的 12153"},
		{ReasonManualDisable, false, "面板手动禁用是运维意图"},
		{"面板手动禁用（管理员操作）", false, "同上，标准文案"},
		{"account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction", false, "风控封禁：可能过几天放行"},
		{"account restricted by upstream (11140 unusual activity)", false, "同上"},
		{"余额耗尽（credits=0）", false, "余额问题不是掉线"},
		{"test disable", false, "无关键词的自定义原因"},
		{"", false, "原因丢失时不能猜"},
	}
	for _, c := range cases {
		if got := IsOfflineReasonText(c.reason); got != c.want {
			t.Errorf("IsOfflineReasonText(%q) = %v want %v（%s）", c.reason, got, c.want, c.why)
		}
	}
}

func TestIsOfflineDisabled(t *testing.T) {
	cases := []struct {
		st   Status
		want bool
	}{
		{Status{Disabled: false, DisabledReason: OfflineReason("401")}, false},
		{Status{Disabled: true, DisabledReason: OfflineReason("401")}, true},
		{Status{Disabled: true, DisabledReason: "12153 session dead"}, true},
		{Status{Disabled: true, DisabledReason: ReasonManualDisable}, false},
		{Status{Disabled: true, DisabledReason: ""}, false},
	}
	for _, c := range cases {
		if got := IsOfflineDisabled(c.st); got != c.want {
			t.Errorf("IsOfflineDisabled(%+v) = %v want %v", c.st, got, c.want)
		}
	}
}
