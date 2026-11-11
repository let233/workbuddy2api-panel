package main

import (
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/panelauth"
)

// 横幅要报「配置从哪来」，但不再有任何密钥可打：面板改成账号密码登录后，
// 启动日志里出现任何 secret 都是回归。
func TestListenBannerReportsConfigSource(t *testing.T) {
	got := listenBanner("127.0.0.1:7863", "/etc/wb/config.json", false)
	want := "workbuddy2api listening on 127.0.0.1:7863 (配置 /etc/wb/config.json；面板 账号密码登录)，" +
		"管理面板 http://127.0.0.1:7863/panel/"
	if got != want {
		t.Fatalf("横幅=%q want %q", got, want)
	}
	// 前缀不能动：面板「运行日志」按它归类（ring_test.go 的 ChSys 用例）。
	if !strings.HasPrefix(got, "workbuddy2api listening on 127.0.0.1:7863") {
		t.Fatalf("横幅前缀被改动，面板日志归类会失配：%q", got)
	}

	// 没有读到手写的引导文件（全新部署）：来源报默认配置，不许提某个不存在的路径。
	got = listenBanner("127.0.0.1:7863", "", false)
	if !strings.Contains(got, "配置 默认配置") || strings.Contains(got, "/etc/wb") {
		t.Fatalf("默认配置时来源应报「默认配置」：%q", got)
	}

	// 首启（管理员还是初始口令）：横幅提示去日志里找账号，但不能把密码写在这里——
	// 横幅会进 journal，和明文口令一起进日志等于白折腾密码哈希。
	got = listenBanner("127.0.0.1:7863", "/etc/wb/config.json", true)
	if !strings.Contains(got, "初始账号 "+panelauth.DefaultAdminUsername) {
		t.Fatalf("首启横幅应提示初始账号：%q", got)
	}
	if strings.Contains(got, panelauth.DefaultAdminPassword) {
		t.Fatalf("横幅泄露了初始口令：%q", got)
	}
}
