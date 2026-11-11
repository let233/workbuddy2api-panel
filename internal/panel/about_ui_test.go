package panel

import (
	"os"
	"strings"
	"testing"
)

// TestAboutPageHostsUpdatePanel 关于页承载「版本与更新」与项目信息。
//
// 背景：版本与更新原来挂在配置页末尾。搬到独立的「关于」页之后有三件事必须同时成立，
// 少一件就是静默坏掉、且不会报错：
//  1. 更新控件的 id 全都还在——renderUpdateStatus / checkForUpdates / installUpdate
//     都按 id 取节点，HTML 少一个 id 就是运行时 TypeError（面板整页白屏）；
//  2. 更新配置项的 name 字段跟着搬到了 #aboutForm，而 CFG_MAP 与 collectConfig 是
//     两张表单共用的，所以既不能在 cfgForm 里重复一份，也不能在 aboutForm 里漏掉；
//  3. 顶栏「有更新」角标要跳到关于页，否则它指向一个再也没有更新控件的页面。
func TestAboutPageHostsUpdatePanel(t *testing.T) {
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)

	// 1) 视图、导航、标题三者齐备，缺任一都会让 #about 打不开或标题是 undefined。
	if !strings.Contains(html, `<section class="view" id="view-about" hidden>`) {
		t.Error("缺少 #view-about 视图")
	}
	if !strings.Contains(html, `data-view="about"`) {
		t.Error("导航里没有「关于」入口")
	}
	if !strings.Contains(js, "about: '关于'") {
		t.Error("TITLES 里没有 about，顶栏标题会是 undefined")
	}
	if !strings.Contains(js, "if (v === 'about')") {
		t.Error("go() 里没有 about 分支，进页不会回填更新配置")
	}

	// 2) 更新控件按 id 取，一个都不能少。
	aboutAt := strings.Index(html, `<section class="view" id="view-about" hidden>`)
	if aboutAt < 0 {
		t.Fatal("缺少 #view-about 视图")
	}
	about := html[aboutAt:]
	if end := strings.Index(about, "</section>"); end > 0 {
		about = about[:end]
	}
	for _, id := range []string{
		`id="aboutForm"`, `id="updateNote"`, `id="updateChannel"`, `id="updateProbeOut"`,
		`id="btnAboutSave"`, `id="btnUpdateCheck"`, `id="btnUpdateProbe"`,
		`id="btnUpdateInstall"`, `id="btnUpdateRelease"`,
		`id="aboutVer"`, `id="aboutUptime"`, `id="aboutRedis"`, `id="aboutCfgPath"`,
	} {
		if !strings.Contains(about, id) {
			t.Errorf("关于页缺少 %s", id)
		}
	}

	// 3) 更新配置字段在 aboutForm 里，且不再留在 cfgForm（否则同一 name 出现两次，
	//    cfgField 按表单顺序取第一个，改关于页的输入框会被读成配置页的旧值）。
	for _, name := range []string{
		"update_auto_enabled", "update_auto_time", "update_proxy",
		"update_mirror", "update_proxy_from_env",
	} {
		if !strings.Contains(about, `name="`+name+`"`) {
			t.Errorf("关于页的更新表单里没有 %q", name)
		}
	}
	cfgStart := strings.Index(html, `<form id="cfgForm">`)
	cfgEnd := strings.Index(html[cfgStart:], "</form>")
	if cfgStart < 0 || cfgEnd < 0 {
		t.Fatal("找不到 #cfgForm")
	}
	cfg := html[cfgStart : cfgStart+cfgEnd]
	if strings.Contains(cfg, "update_auto_enabled") || strings.Contains(cfg, "版本与更新") {
		t.Error("配置页里仍留着版本与更新：同一配置项有两处输入框，保存时会互相覆盖观感")
	}

	// 4) 共用的读写路径：cfgField 要真的会去 aboutForm 找，保存按钮要走同一个 saveConfig。
	if !strings.Contains(js, "function cfgField(") || !strings.Contains(js, "'aboutForm'") {
		t.Error("cfgField 没有覆盖 aboutForm，关于页的更新配置既存不下也回填不了")
	}
	if !strings.Contains(js, "$('btnAboutSave').onclick = () => saveConfig(") {
		t.Error("「保存更新设置」没有复用 saveConfig")
	}
}
