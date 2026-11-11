package gatewaykeys

import "testing"

// EnsureSystemKey 只在「一把 Key 都没有」时播种：留空的模型/额度即不限，
// 提示词模式跟随全局。播出来的 secret 必须立刻可用（首启运维直接拿它调 /v1）。
func TestEnsureSystemKeySeedsOnce(t *testing.T) {
	m := NewManager(nil)
	k, created, err := m.EnsureSystemKey()
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if k.Name != SystemKeyName || !k.Enabled || k.Secret == "" {
		t.Fatalf("系统 Key 形态不对: %+v", k)
	}
	if k.CreditLimit != 0 || len(k.Models) != 0 {
		t.Fatalf("系统 Key 必须额度不限、模型不限: limit=%v models=%v", k.CreditLimit, k.Models)
	}
	if k.PromptMode != ModeInherit {
		t.Fatalf("系统 Key 提示词模式应跟随全局，得到 %q", k.PromptMode)
	}
	if got, ok := m.Resolve(k.Secret); !ok || got.ID != k.ID {
		t.Fatalf("播出来的 secret 解析不到本 Key: ok=%v id=%q", ok, got.ID)
	}

	// 第二次（重启）不再播种：否则每次启动都多一把万能 Key。
	if _, again, err := m.EnsureSystemKey(); err != nil || again {
		t.Fatalf("重复播种: created=%v err=%v", again, err)
	}

	// 已有手工建的 Key 时也不插手。
	other := NewManager(nil)
	if _, err := other.Create(Key{Name: "手工"}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := other.EnsureSystemKey(); err != nil || created {
		t.Fatalf("已有 Key 时不该播种: created=%v err=%v", created, err)
	}
	if n := len(other.List()); n != 1 {
		t.Fatalf("Key 数量=%d want 1", n)
	}
}
