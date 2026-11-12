package panel

// /panel/api/usage 的「按 API Key」维度端到端：写入带 APIKey 的用量后，接口必须
// 回 by_api_key / credit_by_api_key，昵称取自网关 Key 管理器的 Key 名，且各行
// 相加等于总计（否则「按 Key 分组」对不上账）。

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/gatewaykeys"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/usage"
)

func TestUsageHandlerByAPIKey(t *testing.T) {
	km := gatewaykeys.NewManager(nil)
	k, err := km.Create(gatewaykeys.Key{Name: "生产 Key", Enabled: true})
	if err != nil {
		t.Fatalf("创建测试 Key 失败: %v", err)
	}

	rec := usage.New(nil)
	base := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)
	add := func(apiKey string, credit float64) {
		rec.Add(base, "cn", "u1", "glm-5.2", usage.Delta{
			PromptTokens: 10, HasPromptTokens: true,
			TotalTokens: 10, HasTotal: true,
			Credit: 3, HasCredit: true,
			APIKey: apiKey,
		}, true)
	}
	add("", 1)   // 升级前的历史（无 Key）
	add(k.ID, 3) // 有 Key 的两次调用
	add(k.ID, 3)

	p := New(Config{Version: "test", Usage: rec, Pool: pool.New(nil), Keys: km})
	req := httptest.NewRequest("GET", "/panel/api/usage?hours=24", nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("usage -> %d %s", rr.Code, rr.Body)
	}
	var out struct {
		Totals struct {
			Requests float64 `json:"requests"`
			Credits  float64 `json:"credits"`
		} `json:"totals"`
		ByAPIKey []struct {
			Key      string  `json:"key"`
			Extra    string  `json:"extra"`
			Requests float64 `json:"requests"`
		} `json:"by_api_key"`
		CreditByAPIKey []struct {
			Key      string  `json:"key"`
			Nickname string  `json:"nickname"`
			Credits  float64 `json:"credits"`
		} `json:"credit_by_api_key"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v\n%s", err, rr.Body)
	}
	if len(out.ByAPIKey) != 2 {
		t.Fatalf("by_api_key 应有 2 行（本 Key + 空 Key 历史），实际 %d: %+v", len(out.ByAPIKey), out.ByAPIKey)
	}
	var named, blank bool
	var sum float64
	for _, row := range out.ByAPIKey {
		sum += row.Requests
		switch row.Key {
		case k.ID:
			named = true
			if row.Extra != "生产 Key" {
				t.Fatalf("Key 昵称应取自 Key 名，实际 %q", row.Extra)
			}
			if row.Requests != 2 {
				t.Fatalf("本 Key 请求数 = %v, want 2", row.Requests)
			}
		case "":
			blank = true
			if row.Requests != 1 {
				t.Fatalf("空 Key 行请求数 = %v, want 1", row.Requests)
			}
		default:
			t.Fatalf("出现意外的 Key 行: %+v", row)
		}
	}
	if !named || !blank {
		t.Fatalf("缺少本 Key 或空 Key 行: %+v", out.ByAPIKey)
	}
	if sum != out.Totals.Requests {
		t.Fatalf("by_api_key 请求数合计 %v != totals %v", sum, out.Totals.Requests)
	}
	if len(out.CreditByAPIKey) != 2 {
		t.Fatalf("credit_by_api_key 应有 2 行，实际 %+v", out.CreditByAPIKey)
	}
	var creditSum float64
	for _, row := range out.CreditByAPIKey {
		creditSum += row.Credits
		if row.Key == k.ID && row.Nickname != "生产 Key" {
			t.Fatalf("积分行昵称 = %q, want 生产 Key", row.Nickname)
		}
	}
	if creditSum != out.Totals.Credits {
		t.Fatalf("credit_by_api_key 积分合计 %v != totals %v", creditSum, out.Totals.Credits)
	}
}
