package main

import (
	"strings"
	"testing"
)

// 掩码只留首尾各两位，短密钥/空密钥整条隐去——横幅是写进 journal 的，不能出明文。
func TestMaskSecret(t *testing.T) {
	const key = "gZb95oiQqT-A7VdssgI27FO"
	cases := []struct{ in, want string }{
		{"", "(未设置)"},
		{"abc", "***"},
		{key, "gZ" + strings.Repeat("*", len(key)-4) + "FO"},
		{"密钥密钥密钥密钥密钥", "密钥" + strings.Repeat("*", 6) + "密钥"},
	}
	for _, c := range cases {
		if got := maskSecret(c.in); got != c.want {
			t.Errorf("maskSecret(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// issue #155：改了文件却不生效时，旧横幅只写 api_key=true，看不出是 WB2A_API_KEY
// 覆盖了文件，还是改错了文件。现在 api_key 根本不在文件里，横幅必须报「数据库」，
// 且明确那个文件不含密钥——否则同一个困惑只是换了个形态。
func TestListenBannerReportsKeySource(t *testing.T) {
	const key = "gZb95oiQqT-A7VdssgI27FO"
	t.Setenv("WB2A_API_KEY", "")

	got := listenBanner("127.0.0.1:7863", "/etc/wb/config.json", key)
	want := "workbuddy2api listening on 127.0.0.1:7863 (api_key=gZ" + strings.Repeat("*", len(key)-4) +
		"FO, 来源 数据库（/etc/wb/config.json 不含密钥）)，管理面板 http://127.0.0.1:7863/panel/"
	if got != want {
		t.Fatalf("横幅=%q want %q", got, want)
	}
	if strings.Contains(got, key) {
		t.Fatalf("横幅泄露了明文 api_key：%q", got)
	}
	// 前缀不能动：面板「运行日志」按它归类（ring_test.go 的 ChSys 用例）。
	if !strings.HasPrefix(got, "workbuddy2api listening on 127.0.0.1:7863") {
		t.Fatalf("横幅前缀被改动，面板日志归类会失配：%q", got)
	}

	// env 非空即覆盖库里的值（同 applyEnv）：来源必须改口。
	t.Setenv("WB2A_API_KEY", "env-key-12345678")
	got = listenBanner(":7863", "/etc/wb/config.json", "env-key-12345678")
	if !strings.Contains(got, "来源 环境变量 WB2A_API_KEY") {
		t.Fatalf("env 覆盖时来源仍报库，issue #155 的困惑照旧：%q", got)
	}

	// 首次部署（还没有引导文件）：来源报自动生成，不许提配置文件。
	t.Setenv("WB2A_API_KEY", "")
	got = listenBanner(":7863", "", "abc")
	if !strings.Contains(got, "来源 数据库（首次部署自动生成 sk- key）") || strings.Contains(got, "/etc/wb") {
		t.Fatalf("首次部署时来源应报自动生成：%q", got)
	}
}
