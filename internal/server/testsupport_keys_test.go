package server

import (
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/gatewaykeys"
)

// testKeys 造一份「有且只有一把可用 Key」的表，secret 由用例指定。
//
// 网关已经只认 Key 表（没有全局 api_key 这把万能钥匙），Config.Keys 为 nil 只表示
// 「本进程不鉴权」，那只该出现在明确不测鉴权的用例里；凡是断言 200/400 的请求都得
// 先有一把真 Key，否则拿到的永远是 401。
func testKeys(t *testing.T, secret string) *gatewaykeys.Manager {
	t.Helper()
	m := gatewaykeys.NewManager(nil)
	if _, err := m.Create(gatewaykeys.Key{Name: "test", Secret: secret}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return m
}
