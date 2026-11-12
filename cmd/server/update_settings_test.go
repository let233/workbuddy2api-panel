package main

import "testing"

// update.insecure_tls 必须能一路走到 updater.Settings：这个开关只在面板上出现
// 是不够的——漏掉映射时「保存成功、实际仍在校验证书」，排障会被误导。
func TestUpdateSettingsCarryInsecureTLS(t *testing.T) {
	cfg := Default()
	cfg.Update.InsecureTLS = true
	if got := updateSettingsFrom(cfg); !got.InsecureTLS {
		t.Fatal("update.insecure_tls=true 未传进 updater.Settings")
	}
	cfg.Update.InsecureTLS = false
	if got := updateSettingsFrom(cfg); got.InsecureTLS {
		t.Fatal("update.insecure_tls=false 却传成了 true")
	}
}

// JSON 键名是面板保存时写进库的那一个，改名就会静默失效；同时确认缺省为 false
// （默认不跳过证书校验）。
func TestUpdateInsecureTLSConfigKey(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"update":{"insecure_tls":true}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if !cfg.Update.InsecureTLS {
		t.Fatal("JSON update.insecure_tls 未解析进 Config")
	}
	if Default().Update.InsecureTLS {
		t.Fatal("缺省必须是 false（默认校验证书）")
	}
}
