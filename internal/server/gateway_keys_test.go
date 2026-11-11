package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/gatewaykeys"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// gkSSE 流式成功帧（含 usage.credit=2.5），供配额计数断言。
const gkSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2,\"credit\":2.5}}\n\n" +
	"data: [DONE]\n\n"

func gkChat(t *testing.T, h *Handler, secret, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, model)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func gkHandler(t *testing.T, m *gatewaykeys.Manager, cfg Config) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, gkSSE, true })
	cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cfg.Upstream = up
	cfg.Keys = m
	return NewHandler(cfg)
}

// TestGatewayKeyLifecycle 禁用/过期/超额/未知四种拒绝路径。
func TestGatewayKeyLifecycle(t *testing.T) {
	m := gatewaykeys.NewManager(nil)
	ok, err := m.Create(gatewaykeys.Key{Name: "ok"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	disabled, _ := m.Create(gatewaykeys.Key{Name: "off"})
	upd := disabled.Clone()
	upd.Enabled = false
	disabled, _ = m.Update(upd)
	expired, _ := m.Create(gatewaykeys.Key{Name: "old", ExpiresAt: time.Now().Add(-time.Hour).Unix()})
	quota, _ := m.Create(gatewaykeys.Key{Name: "q", CreditLimit: 1})
	m.Charge(quota.ID, 2, true, 0) // Create 会清零用量，这里用真实计费路径把额度用满

	// 全局 Key 置位，避免"空全局 Key = 鉴权关闭"把未知 Key 放过去。
	h := gkHandler(t, m, Config{PromptMode: "passthrough"})
	cases := []struct {
		name   string
		secret string
		status int
		code   string
	}{
		{"valid", ok.Secret, 200, ""},
		{"disabled", disabled.Secret, 403, "key_disabled"},
		{"expired", expired.Secret, 403, "key_expired"},
		{"quota", quota.Secret, 429, "insufficient_quota"},
		{"unknown", "sk-nope", 401, "invalid_api_key"},
	}
	for _, c := range cases {
		rec := gkChat(t, h, c.secret, "cn:glm-5.2")
		if rec.Code != c.status {
			t.Fatalf("%s: code=%d want %d body=%s", c.name, rec.Code, c.status, rec.Body.String())
		}
		if c.code != "" && !strings.Contains(rec.Body.String(), c.code) {
			t.Fatalf("%s: body 缺 %q: %s", c.name, c.code, rec.Body.String())
		}
	}
}

// TestGatewayKeyNoUniversalKey 系统里已经没有全局 api_key 这把万能钥匙：
// key 表之外的一切 secret（包括旧部署遗留的全局 key、空 bearer）都只能拿到 401。
func TestGatewayKeyNoUniversalKey(t *testing.T) {
	m := gatewaykeys.NewManager(nil)
	if _, err := m.Create(gatewaykeys.Key{Name: "k"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	h := gkHandler(t, m, Config{PromptMode: "passthrough"})
	for _, secret := range []string{"global-secret", "sk-legacykey1234", ""} {
		rec := gkChat(t, h, secret, "cn:glm-5.2")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("secret %q 应 401: code=%d %s", secret, rec.Code, rec.Body.String())
		}
	}
}

// TestGatewayKeyModelAllowlist 每个 Key 的可用模型：未授权的直接 400，不打上游。
func TestGatewayKeyModelAllowlist(t *testing.T) {
	m := gatewaykeys.NewManager(nil)
	k, _ := m.Create(gatewaykeys.Key{Name: "limited", Models: []string{"cn:glm-5.2"}})

	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, gkSSE, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, Keys: m, PromptMode: "passthrough"})

	if rec := gkChat(t, h, k.Secret, "cn:kimi-k3-1"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("未授权模型应 400 model_unavailable: code=%d %s", rec.Code, rec.Body.String())
	}
	if calls != 0 {
		t.Fatalf("未授权模型不该打上游，calls=%d", calls)
	}
	if rec := gkChat(t, h, k.Secret, "cn:glm-5.2"); rec.Code != 200 {
		t.Fatalf("授权模型应放行: code=%d %s", rec.Code, rec.Body.String())
	}
}

// TestGatewayKeyModelsListFiltered /v1/models 与点名调用对称。
func TestGatewayKeyModelsListFiltered(t *testing.T) {
	m := gatewaykeys.NewManager(nil)
	k, _ := m.Create(gatewaykeys.Key{Name: "only-glm", Models: []string{"glm-5.2"}})
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":1000,"maxOutputTokens":8192},{"id":"kimi-k3-1","maxInputTokens":1000,"maxOutputTokens":8192}],"agents":[{"name":"cli","models":["glm-5.2","kimi-k3-1"]}]}}`, false
	})
	// 清动态模型缓存，避免串测试。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, Keys: m})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+k.Secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0]["id"] != "cn:glm-5.2" {
		t.Fatalf("列表应只含 cn:glm-5.2，得到 %v", resp.Data)
	}
}

// TestGatewayKeyPromptOverride 显式模式覆盖全局；空模式跟随全局。
func TestGatewayKeyPromptOverride(t *testing.T) {
	var sent string
	up := testUpstream(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		sent = string(raw)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(gkSSE))}, nil
	})}, func(rt *upstream.Runtime) { rt.ChatBaseCN = "https://fake.example" })
	m := gatewaykeys.NewManager(nil)
	own, _ := m.Create(gatewaykeys.Key{Name: "own", PromptMode: "custom", PromptText: "KEYPROMPT"})
	inherit, _ := m.Create(gatewaykeys.Key{Name: "inherit"})
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		Keys:       m,
		PromptMode: "custom",
		PromptText: "GLOBALPROMPT",
	})

	if rec := gkChat(t, h, own.Secret, "cn:glm-5.2"); rec.Code != 200 {
		t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sent, "KEYPROMPT") || strings.Contains(sent, "GLOBALPROMPT") {
		t.Fatalf("Key 的提示词应覆盖全局，出站 body=%s", sent)
	}

	if rec := gkChat(t, h, inherit.Secret, "cn:glm-5.2"); rec.Code != 200 {
		t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sent, "GLOBALPROMPT") {
		t.Fatalf("空模式应跟随全局提示词，出站 body=%s", sent)
	}
}

// TestGatewayKeyQuotaCharged 成功请求按上游 usage.credit 累计到该 Key。
func TestGatewayKeyQuotaCharged(t *testing.T) {
	m := gatewaykeys.NewManager(nil)
	k, _ := m.Create(gatewaykeys.Key{Name: "charged", CreditLimit: 100})
	h := gkHandler(t, m, Config{PromptMode: "passthrough"})

	if rec := gkChat(t, h, k.Secret, "cn:glm-5.2"); rec.Code != 200 {
		t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
	}
	got, _ := m.Get(k.ID)
	if got.CreditUsed != 2.5 {
		t.Fatalf("CreditUsed=%v want 2.5", got.CreditUsed)
	}
	if got.Requests != 1 || got.Tokens != 2 || got.LastUsedAt == 0 {
		t.Fatalf("计数不对: %+v", got)
	}

	// 额度用满后立即拒绝。
	upd := got.Clone()
	upd.CreditLimit = 2.5
	if _, err := m.Update(upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	if rec := gkChat(t, h, k.Secret, "cn:glm-5.2"); rec.Code != 429 {
		t.Fatalf("超额应 429，得到 %d %s", rec.Code, rec.Body.String())
	}
}
