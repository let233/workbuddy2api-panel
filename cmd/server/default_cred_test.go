package main

import "testing"

// defaultCredWarning 只在「管理员还是初始口令 + 监听非回环」时告警：
// 那是把面板直接交给整个内网的组合；改过密码或只听回环就不打扰启动日志。
func TestDefaultCredWarning(t *testing.T) {
	cases := []struct {
		listen       string
		defaultAdmin bool
		want         bool
	}{
		{":7863", true, true},            // 缺省监听绑所有网卡 + 初始口令
		{"0.0.0.0:7863", true, true},     // 显式全网卡
		{"192.168.1.5:7863", true, true}, // 具体内网地址
		{"127.0.0.1:7863", true, false},  // 只听回环
		{"[::1]:7863", true, false},      // IPv6 回环
		{"localhost:7863", true, false},  // localhost
		{":7863", false, false},          // 已改过密码
		{"127.0.0.1:7863", false, false}, // 回环 + 已改密
	}
	for _, c := range cases {
		got := defaultCredWarning(c.listen, c.defaultAdmin)
		if (got != "") != c.want {
			t.Errorf("defaultCredWarning(%q, %v) = %q, want 告警=%v", c.listen, c.defaultAdmin, got, c.want)
		}
	}
}
