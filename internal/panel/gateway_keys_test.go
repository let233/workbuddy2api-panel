package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/gatewaykeys"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// 面板层测的是「路由有没有挂上、body 有没有解、状态码对不对」——
// 这三类错法在 gatewaykeys 包的单测里都看不见，面板上却表现为点了保存没反应。

func gkPanelEnv(t *testing.T) (*Panel, *gatewaykeys.Manager) {
	t.Helper()
	m := gatewaykeys.NewManager(storetest.Open(t))
	t.Cleanup(m.Close)
	return New(Config{Version: "test", Keys: m}), m
}

func gkCall(t *testing.T, p *Panel, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == nil {
		rd = strings.NewReader("")
	} else if s, ok := body.(string); ok {
		rd = strings.NewReader(s)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

type gkListResp struct {
	Keys []struct {
		gatewaykeys.Key
		Status string `json:"status"`
	} `json:"keys"`
	Templates []gatewaykeys.Template `json:"templates"`
	Global    struct {
		PromptMode string `json:"prompt_mode"`
		PromptText string `json:"prompt_text"`
	} `json:"global"`
	Now int64 `json:"now"`
}

type gkKeyResp struct {
	OK  bool `json:"ok"`
	Key struct {
		gatewaykeys.Key
		Status string `json:"status"`
	} `json:"key"`
}

func gkDecode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

// TestGatewayKeysPanelCRUD 建 / 读 / 改 / 重置用量 / 删，以及各种错码。
func TestGatewayKeysPanelCRUD(t *testing.T) {
	p, m := gkPanelEnv(t)

	var list gkListResp
	rec := gkCall(t, p, "GET", "/panel/api/apikeys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	gkDecode(t, rec, &list)
	if len(list.Keys) != 0 || len(list.Templates) != 0 {
		t.Fatalf("新库应为空, got %+v", list)
	}
	if list.Now == 0 {
		t.Fatal("应回 now 供前端算状态")
	}

	rec = gkCall(t, p, "POST", "/panel/api/apikeys/create", map[string]any{
		"name":         "试用A",
		"credit_limit": 5,
		"models":       []string{"cn:glm-5.2"},
		"prompt_mode":  "custom",
		"prompt_text":  "KEYPROMPT",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var created gkKeyResp
	gkDecode(t, rec, &created)
	id := created.Key.ID
	if id == "" || !strings.HasPrefix(created.Key.Secret, "sk-") {
		t.Fatalf("应自动发号, got %+v", created.Key.Key)
	}
	if !created.Key.Enabled || created.Key.Status != gatewaykeys.StatusOK {
		t.Fatalf("新建应启用且可用, got %+v", created.Key)
	}
	if created.Key.CreditLimit != 5 || len(created.Key.Models) != 1 || created.Key.PromptMode != "custom" {
		t.Fatalf("配置没落库, got %+v", created.Key.Key)
	}

	gkDecode(t, gkCall(t, p, "GET", "/panel/api/apikeys", nil), &list)
	if len(list.Keys) != 1 || list.Keys[0].ID != id {
		t.Fatalf("列表应有刚建的 Key, got %+v", list.Keys)
	}

	// 不带 enabled 的更新不应顺手把 Key 停掉（改额度不该改变可用性）。
	var kept gkKeyResp
	gkDecode(t, gkCall(t, p, "POST", "/panel/api/apikeys/update", map[string]any{
		"id": id, "name": "试用A2", "credit_limit": 8,
	}), &kept)
	if !kept.Key.Enabled || kept.Key.Status != gatewaykeys.StatusOK {
		t.Fatalf("未传 enabled 应保持启用, got enabled=%v status=%s", kept.Key.Enabled, kept.Key.Status)
	}

	// 改配置：名字 + 额度 + 禁用；secret 留空表示不改。
	rec = gkCall(t, p, "POST", "/panel/api/apikeys/update", map[string]any{
		"id": id, "name": "试用B", "credit_limit": 10, "enabled": false, "secret": created.Key.Secret,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("更新应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var updated gkKeyResp
	gkDecode(t, rec, &updated)
	if updated.Key.Name != "试用B" || updated.Key.CreditLimit != 10 {
		t.Fatalf("更新没生效, got %+v", updated.Key.Key)
	}
	if updated.Key.Status != gatewaykeys.StatusDisabled {
		t.Fatalf("禁用后状态应 disabled, got %q", updated.Key.Status)
	}
	// 显式传 enabled=true 要能重新启用（指针接参的另一半）。
	var on gkKeyResp
	gkDecode(t, gkCall(t, p, "POST", "/panel/api/apikeys/update", map[string]any{
		"id": id, "name": "试用B", "credit_limit": 10, "enabled": true,
	}), &on)
	if !on.Key.Enabled || on.Key.Status != gatewaykeys.StatusOK {
		t.Fatalf("应重新启用, got enabled=%v status=%s", on.Key.Enabled, on.Key.Status)
	}
	// 之后再停用，验下面「禁用不参与鉴权」的语义。
	gkDecode(t, gkCall(t, p, "POST", "/panel/api/apikeys/update", map[string]any{
		"id": id, "name": "试用B", "credit_limit": 10, "enabled": false,
	}), &updated)

	// 用量计入后再重置。
	m.Charge(id, 4, true, 9)
	rec = gkCall(t, p, "POST", "/panel/api/apikeys/reset_usage", map[string]any{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("重置应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var reset gkKeyResp
	gkDecode(t, rec, &reset)
	if reset.Key.CreditUsed != 0 || reset.Key.Requests != 0 {
		t.Fatalf("重置后应清零, got used=%v req=%d", reset.Key.CreditUsed, reset.Key.Requests)
	}

	if rec := gkCall(t, p, "POST", "/panel/api/apikeys/delete", map[string]any{"id": id}); rec.Code != http.StatusOK {
		t.Fatalf("删除应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	gkDecode(t, gkCall(t, p, "GET", "/panel/api/apikeys", nil), &list)
	if len(list.Keys) != 0 {
		t.Fatalf("删除后应为空, got %+v", list.Keys)
	}

	// 新建时取消勾选「启用」→ 建出来就是停用的（不能悄悄建一把能用的）。
	var off gkKeyResp
	gkDecode(t, gkCall(t, p, "POST", "/panel/api/apikeys/create", map[string]any{
		"name": "建即停用", "enabled": false,
	}), &off)
	if off.Key.Enabled || off.Key.Status != gatewaykeys.StatusDisabled {
		t.Fatalf("enabled=false 应建成停用, got %+v", off.Key)
	}
	if rec := gkCall(t, p, "POST", "/panel/api/apikeys/delete", map[string]any{"id": off.Key.ID}); rec.Code != http.StatusOK {
		t.Fatalf("清理停用 Key 应 200, got %d", rec.Code)
	}

	// 错码：未知 id 404、坏 body 400、重复 secret 409。
	if rec := gkCall(t, p, "POST", "/panel/api/apikeys/update", map[string]any{"id": "gk_nope", "name": "x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 id 应 404, got %d", rec.Code)
	}
	if rec := gkCall(t, p, "POST", "/panel/api/apikeys/create", "not-json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400, got %d", rec.Code)
	}
	if rec := gkCall(t, p, "POST", "/panel/api/apikeys/delete", map[string]any{"id": "gk_nope"}); rec.Code != http.StatusNotFound {
		t.Fatalf("删未知 id 应 404, got %d", rec.Code)
	}
	first := gkCall(t, p, "POST", "/panel/api/apikeys/create", map[string]any{"name": "dup", "secret": "sk-dup-secret"})
	if first.Code != http.StatusOK {
		t.Fatalf("首次建应 200, got %d (%s)", first.Code, first.Body.String())
	}
	if rec := gkCall(t, p, "POST", "/panel/api/apikeys/create", map[string]any{"name": "dup2", "secret": "sk-dup-secret"}); rec.Code != http.StatusConflict {
		t.Fatalf("重复 secret 应 409, got %d", rec.Code)
	}
}

// TestGatewayKeyTemplateApply 模板可复用：只补空缺，显式填的字段不被覆盖。
func TestGatewayKeyTemplateApply(t *testing.T) {
	p, _ := gkPanelEnv(t)

	rec := gkCall(t, p, "POST", "/panel/api/apikey_templates/save", map[string]any{
		"name":         "7天试用",
		"expiry_days":  7,
		"credit_limit": 3,
		"models":       []string{"cn:glm-5.2"},
		"prompt_mode":  "append",
		"prompt_text":  "TPL",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("存模板应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var saved struct {
		OK       bool                 `json:"ok"`
		Template gatewaykeys.Template `json:"template"`
	}
	gkDecode(t, rec, &saved)
	tplID := saved.Template.ID
	if tplID == "" {
		t.Fatalf("模板应自动发 id, got %+v", saved)
	}

	// 只给模板：模型/额度/模式/到期全部按模板填。
	var k gkKeyResp
	gkDecode(t, gkCall(t, p, "POST", "/panel/api/apikeys/create", map[string]any{
		"name": "套模板", "template_id": tplID,
	}), &k)
	if len(k.Key.Models) != 1 || k.Key.Models[0] != "cn:glm-5.2" {
		t.Fatalf("模型应从模板来, got %+v", k.Key.Models)
	}
	if k.Key.CreditLimit != 3 || k.Key.PromptMode != "append" || k.Key.PromptText != "TPL" {
		t.Fatalf("额度/提示词应从模板来, got %+v", k.Key.Key)
	}
	want := time.Now().AddDate(0, 0, 7).Unix()
	if d := k.Key.ExpiresAt - want; d < -3600 || d > 3600 {
		t.Fatalf("到期应按 7 天推算, got %d want≈%d", k.Key.ExpiresAt, want)
	}
	if k.Key.TemplateID != tplID {
		t.Fatalf("应记住来源模板, got %q", k.Key.TemplateID)
	}

	// 显式填模型：模板只补空缺，不覆盖。
	var k2 gkKeyResp
	gkDecode(t, gkCall(t, p, "POST", "/panel/api/apikeys/create", map[string]any{
		"name": "指定模型", "template_id": tplID, "models": []string{"cn:kimi-k3-1"},
	}), &k2)
	if len(k2.Key.Models) != 1 || k2.Key.Models[0] != "cn:kimi-k3-1" {
		t.Fatalf("显式模型应优先, got %+v", k2.Key.Models)
	}
	if k2.Key.CreditLimit != 3 {
		t.Fatalf("没填的额度仍应从模板补, got %v", k2.Key.CreditLimit)
	}

	// 模板不存在 → 404（而不是静悄悄建出一把没约束的 Key）。
	if rec := gkCall(t, p, "POST", "/panel/api/apikeys/create", map[string]any{"name": "x", "template_id": "gt_nope"}); rec.Code != http.StatusNotFound {
		t.Fatalf("未知模板应 404, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 模板列表随 /apikeys 一起下发，前端拉一次就能填下拉框。
	var list gkListResp
	gkDecode(t, gkCall(t, p, "GET", "/panel/api/apikeys", nil), &list)
	if len(list.Templates) != 1 || list.Templates[0].ID != tplID {
		t.Fatalf("模板应出现在列表里, got %+v", list.Templates)
	}

	if rec := gkCall(t, p, "POST", "/panel/api/apikey_templates/delete", map[string]any{"id": tplID}); rec.Code != http.StatusOK {
		t.Fatalf("删模板应 200, got %d", rec.Code)
	}
	gkDecode(t, gkCall(t, p, "GET", "/panel/api/apikeys", nil), &list)
	if len(list.Templates) != 0 {
		t.Fatalf("删后模板应为空, got %+v", list.Templates)
	}
}

// TestGatewayKeysPanelDisabled 没接 Manager 的实例（纯内存/未接线）要明确说没开，
// 而不是回一个空列表让人以为「本来就没有 Key」。
func TestGatewayKeysPanelDisabled(t *testing.T) {
	p := New(Config{Version: "test"})
	rec := gkCall(t, p, "GET", "/panel/api/apikeys", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未启用应 501, got %d (%s)", rec.Code, rec.Body.String())
	}
}
