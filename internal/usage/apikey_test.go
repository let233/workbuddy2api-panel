package usage

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// 面板「用量明细」的「按 API Key」维度：同一窗口内按 bucket.APIKey 分组，
// 行内 Extra 放 Key 名（keyNames 查不到时为空，前端退化成显示 ID 前缀），
// 空 Key 行 = 升级前历史 / 未鉴权调用，必须保留否则各行相加对不上总计。
func TestSnapshotByAPIKey(t *testing.T) {
	r := New(nil)
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{APIKey: "k1", PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "1"}, true)
	r.Add(now, "cn", "u1", "glm-5.2", Delta{APIKey: "k1", PromptTokens: 50, HasPromptTokens: true, TotalTokens: 50, HasTotal: true}, true)
	r.Add(now, "cn", "u2", "glm-5.2", Delta{APIKey: "k2", PromptTokens: 30, HasPromptTokens: true, TotalTokens: 30, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "1"}, true)
	// 未带 Key（鉴权关闭/历史数据）：并入空 Key 行。
	r.Add(now, "global", "u3", "claude-4.6", Delta{PromptTokens: 10, HasPromptTokens: true, TotalTokens: 10, HasTotal: true}, true)

	names := map[string]string{"k1": "生产 Key", "k2": "测试 Key"}
	s := r.SnapshotWindowKeys(Window{Hours: 24}, nil, names, nil)

	if len(s.ByAPIKey) != 3 {
		t.Fatalf("by_api_key = %d 行, want 3: %+v", len(s.ByAPIKey), s.ByAPIKey)
	}
	// 按总 Token 降序：k1=150, k2=30, ""=10
	want := []struct {
		key, name string
		req, tt   int64
	}{{"k1", "生产 Key", 2, 150}, {"k2", "测试 Key", 1, 30}, {"", "", 1, 10}}
	for i, w := range want {
		got := s.ByAPIKey[i]
		if got.Key != w.key || got.Extra != w.name || got.Requests != w.req || got.TotalTokens != w.tt {
			t.Fatalf("by_api_key[%d] = %+v, want key=%q extra=%q req=%d tt=%d", i, got, w.key, w.name, w.req, w.tt)
		}
	}
	// 各行相加必须等于总计（窗口口径一致）。
	var sum int64
	for _, row := range s.ByAPIKey {
		sum += row.TotalTokens
	}
	if sum != s.Totals.TotalTokens {
		t.Fatalf("by_api_key 合计 %d != totals %d", sum, s.Totals.TotalTokens)
	}

	// 积分维度只收有 credit 样本的桶：k2 与空 Key 行不该出现。
	if len(s.CreditByAPIKey) != 2 {
		t.Fatalf("credit_by_api_key = %+v, want 2 行", s.CreditByAPIKey)
	}
	cm := map[string]CreditAgg{}
	for _, row := range s.CreditByAPIKey {
		cm[row.Key] = row
	}
	if row, ok := cm["k1"]; !ok || row.Credits != 2 || row.Nickname != "生产 Key" || row.CreditsPer1MTokens != 20000 {
		t.Fatalf("credit_by_api_key[k1] = %+v", row)
	}
	if _, ok := cm[""]; ok {
		t.Fatal("credit_by_api_key 不该出现无 credit 样本的空 Key 行")
	}
}

// 同一 (小时, 域, 账号, 模型, 倍率) 但不同 Key 必须落成不同桶：否则「按 API Key」
// 维度只能拿到先到的那个 Key 的用量。
func TestAPIKeySplitsBuckets(t *testing.T) {
	r := New(nil)
	now := time.Now()
	r.Add(now, "cn", "u1", "m", Delta{APIKey: "k1", PromptTokens: 1, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{APIKey: "k2", PromptTokens: 2, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u1", "m", Delta{PromptTokens: 4, HasPromptTokens: true}, true)

	if got := len(r.buckets); got != 3 {
		t.Fatalf("桶数 = %d, want 3（两个 Key + 空 Key 各一桶）", got)
	}
	// 空 Key 的桶沿用 5 段老主键，升级前的行不会被劈成新行。
	legacy := bucketKey("h:"+now.Format(hourLayout), "cn", "u1", "m", "", "")
	if r.buckets[legacy] == nil {
		t.Fatalf("空 Key 桶主键应保持旧格式，实际缺 %q", legacy)
	}
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.PromptTokens != 7 {
		t.Fatalf("totals = %d/%d, want 3/7", s.Totals.Requests, s.Totals.PromptTokens)
	}
}

// 折叠成日桶后 Key 维度不能丢（Rollup 重建主键时必须带上 api_key）。
func TestAPIKeySurvivesRollup(t *testing.T) {
	r := New(nil)
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u1", "m", Delta{APIKey: "k1", PromptTokens: 5, HasPromptTokens: true, Credit: 1, HasCredit: true, ModelRate: "1"}, true)
	r.Add(old, "cn", "u1", "m", Delta{APIKey: "k2", PromptTokens: 6, HasPromptTokens: true}, true)

	r.Rollup(time.Now())

	for _, b := range r.buckets {
		if b.Scope[:2] != "d:" {
			t.Fatalf("前置条件：应已折叠为日桶，实际 %+v", b)
		}
	}
	s := r.Snapshot(0, nil)
	if len(s.ByAPIKey) != 2 {
		t.Fatalf("折叠后 by_api_key = %+v, want 2 行", s.ByAPIKey)
	}
	if s.Totals.PromptTokens != 11 {
		t.Fatalf("折叠无损：pt = %d, want 11", s.Totals.PromptTokens)
	}
	if len(s.CreditByAPIKey) != 1 || s.CreditByAPIKey[0].Key != "k1" || s.CreditByAPIKey[0].Credits != 1 {
		t.Fatalf("折叠后 credit_by_api_key = %+v", s.CreditByAPIKey)
	}
}

// Key 维度要落库：换实例（等价重启）后仍能按 Key 聚合。
func TestAPIKeyPersists(t *testing.T) {
	db := storetest.Open(t)
	r1 := New(db)
	now := time.Now()
	r1.Add(now, "cn", "u1", "m", Delta{APIKey: "k1", PromptTokens: 9, HasPromptTokens: true, TotalTokens: 9, HasTotal: true}, true)
	r1.Save()

	rows, err := store.LoadAll[bucket](db)
	if err != nil || len(rows) != 1 || rows[0].APIKey != "k1" {
		t.Fatalf("落库异常: err=%v rows=%+v", err, rows)
	}
	r2 := New(db)
	s := r2.SnapshotWindowKeys(Window{Hours: 24}, nil, map[string]string{"k1": "生产 Key"}, nil)
	if len(s.ByAPIKey) != 1 || s.ByAPIKey[0].Key != "k1" || s.ByAPIKey[0].Extra != "生产 Key" {
		t.Fatalf("重启后 by_api_key = %+v", s.ByAPIKey)
	}
}
