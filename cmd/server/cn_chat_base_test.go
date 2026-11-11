package main

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 老配置（无 cn 段）必须保留内置 CN base，零回归。
func TestUpstreamRuntimeDefaultKeepsBuiltinCN(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	up := upstream.New()
	builtin := up.ChatBaseCN()
	up.SetRuntime(upstreamRuntime(cfg))
	if got := up.ChatBaseCN(); got != builtin {
		t.Fatalf("ChatBaseCN=%q want builtin %q", got, builtin)
	}
	// global base 不受 cn 段影响：空 = 按 auth 的 domain 推默认。
	if got := up.ChatBaseGlobal(); got != "" {
		t.Fatalf("ChatBaseGlobal=%q want untouched", got)
	}
}

func TestUpstreamRuntimeCNOverride(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"cn":{"chat_base":"https://cn.example.invalid","billing_base":"https://billing.example.invalid"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	up := upstream.New()
	up.SetRuntime(upstreamRuntime(cfg))
	if got := up.ChatBaseCN(); got != "https://cn.example.invalid" {
		t.Fatalf("ChatBaseCN=%q want override", got)
	}
	if got := up.BillingBaseCN(); got != "https://billing.example.invalid" {
		t.Fatalf("BillingBaseCN=%q want override", got)
	}
	if got := up.ChatBaseGlobal(); got != "" {
		t.Fatalf("ChatBaseGlobal=%q want untouched", got)
	}
}

// 本次改造的核心：cn/global base、UA、超时都随 SetRuntime 立即生效，保存配置不再需要
// 重启进程（旧实现这些字段只在启动装配期写一次）。
func TestUpstreamRuntimeAppliesImmediately(t *testing.T) {
	up := upstream.New()
	base := Default()
	if err := base.normalize(); err != nil {
		t.Fatal(err)
	}
	up.SetRuntime(upstreamRuntime(base))

	next, err := ParseConfig([]byte(`{
		"cn": {"chat_base": "https://cn2.example.invalid"},
		"upstream": {"user_agent": "ua-hot", "timeout_seconds": 45},
		"global": {"enabled": false}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	up.SetRuntime(upstreamRuntime(next))

	if got := up.ChatBaseCN(); got != "https://cn2.example.invalid" {
		t.Fatalf("ChatBaseCN=%q 没热生效", got)
	}
	if got := up.UserAgent(); got != "ua-hot" {
		t.Fatalf("UserAgent=%q 没热生效", got)
	}
	if got := up.Timeout(); got != 45*time.Second {
		t.Fatalf("Timeout=%v 没热生效", got)
	}
	if up.GlobalEnabled() {
		t.Fatal("GlobalEnabled 没热生效")
	}
	if fields := restartRequiredFields(base, next); len(fields) != 0 {
		t.Fatalf("这些字段现在热生效，不该要求重启：%v", fields)
	}
}
