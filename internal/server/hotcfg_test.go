// hotcfg_test.go 热改 Setter 的最小契约：面板保存配置后**下一条请求**就按新值走。
//
// 只钉「生效时机」与「关掉时的对外语义」，不重复各业务路径的用例——那些在
// handler_test.go / bodylimit_test.go / admin_tasks_test.go 里已经有了。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// TestSetMaxPayloadBytesHot 改上限后同一个 Handler 的下一请求立即按新值裁决。
// 这条最值得钉：上限写错方向的后果是「要么读死进程、要么拦掉正常大请求」。
func TestSetMaxPayloadBytesHot(t *testing.T) {
	h := NewHandler(Config{MaxPayloadBytes: 1 << 10})
	body := strings.Repeat("a", 4<<10)
	read := func() *bodyReadError {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		_, berr := h.readBody(rec, req)
		return berr
	}
	if berr := read(); berr == nil || !berr.TooLarge {
		t.Fatalf("4KB 在 1KB 上限下应超限, berr=%v", berr)
	}
	h.SetMaxPayloadBytes(64 << 10) // 热改放宽
	if berr := read(); berr != nil {
		t.Fatalf("热改到 64KB 后 4KB 应放行, berr=%v", berr)
	}
	h.SetMaxPayloadBytes(1 << 10) // 热改收紧
	if berr := read(); berr == nil || !berr.TooLarge {
		t.Fatalf("热改回 1KB 后应重新超限, berr=%v", berr)
	}
}

// TestSetPromptHotRewrite 热改提示词后，下一个请求的出站 body 即用新文本。
//
// 为什么端到端验证而不是只读 h.prompt()：提示词是「写进出站 messages 的东西」，
// 只有看到上游实际收到的 body 才能证明 SetPrompt 真落到了改写路径上。
func TestSetPromptHotRewrite(t *testing.T) {
	var sent string
	up := testUpstream(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		sent = string(raw)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})}, func(rt *upstream.Runtime) { rt.ChatBaseCN = "https://fake.example" })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 初始 passthrough：不注入任何网关提示词（证明后面的变化来自 SetPrompt）。
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})
	send := func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
			`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}
	send()
	if strings.Contains(sent, "热改提示词A") {
		t.Fatalf("passthrough 下不该出现网关提示词: %s", sent)
	}
	h.SetPrompt("custom", "热改提示词A")
	send()
	if !strings.Contains(sent, "热改提示词A") {
		t.Fatalf("SetPrompt 后出站 body 未用新提示词: %s", sent)
	}
	h.SetPrompt("custom", "热改提示词B") // 再改一次：不是「只生效一次」
	send()
	if !strings.Contains(sent, "热改提示词B") || strings.Contains(sent, "热改提示词A") {
		t.Fatalf("第二次 SetPrompt 未生效: %s", sent)
	}
	// mode 空 → custom（与启动时归一化口径一致），不能变成「无提示词」。
	h.SetPrompt("", "热改提示词C")
	send()
	if !strings.Contains(sent, "热改提示词C") {
		t.Fatalf("mode 空串应按 custom 处理: %s", sent)
	}
}

// TestSetAdminEnabledHotToggle 管理面路由常注册 + 运行期门禁。
//
// 关掉后必须 404（与「路由没注册」同语义），而且**不带鉴权头也是 404**：门禁在
// withAuth 外层，否则关掉时回 401 就等于告诉匿名者「这里本来有东西」。
func TestSetAdminEnabledHotToggle(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := NewHandler(Config{AdminEnabled: true, Tasks: &fakeTasks{block: block}})
	call := func(withAuth bool) int {
		r := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
		if withAuth {
			r.Header.Set("Authorization", "Bearer k")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := call(true); code != http.StatusAccepted {
		t.Fatalf("开启时应能触发任务, code=%d", code)
	}
	h.SetAdminEnabled(false)
	if code := call(true); code != http.StatusNotFound {
		t.Fatalf("关闭后应 404, code=%d", code)
	}
	if code := call(false); code != http.StatusNotFound {
		t.Fatalf("关闭后匿名也应 404（不是 401）, code=%d", code)
	}
	h.SetAdminEnabled(true) // 再打开：路由一直在，不需要重建 mux
	if code := call(true); code != http.StatusConflict {
		t.Fatalf("重新打开后应恢复可用（首个 run 仍占着 → 409）, code=%d", code)
	}
}

// TestSetMetricsEnabledHotToggle /metrics 同样是常注册 + 运行期门禁。
func TestSetMetricsEnabledHotToggle(t *testing.T) {
	h := NewHandler(Config{MetricsEnabled: true})
	get := func() int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return w.Code
	}
	if code := get(); code == http.StatusNotFound {
		t.Fatal("开启时 /metrics 不该 404")
	}
	h.SetMetricsEnabled(false)
	if code := get(); code != http.StatusNotFound {
		t.Fatalf("关闭后应 404, code=%d", code)
	}
	h.SetMetricsEnabled(true)
	if code := get(); code == http.StatusNotFound {
		t.Fatal("重新打开后 /metrics 不该再 404")
	}
}

// TestSetAuditNilDisables 审计器可热换：nil = 关闭审计（不落库，但不阻断请求）。
func TestSetAuditNilDisables(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	h := NewHandler(Config{AdminEnabled: true, Tasks: &fakeTasks{block: block}})
	h.SetAudit(nil)
	if h.auditLog() != nil {
		t.Fatal("SetAudit(nil) 后应为关闭状态")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	r.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("审计关闭不该影响请求, code=%d", w.Code)
	}
}

// TestSetWatchdogHot 监控开关/间隔可热改，且关掉后 getter 如实回报。
func TestSetWatchdogHot(t *testing.T) {
	h := NewHandler(Config{})
	h.SetWatchdog(false, 5*time.Second)
	if on, iv := h.watchdog(); on || iv != 5*time.Second {
		t.Fatalf("on=%v iv=%s want false/5s", on, iv)
	}
	h.SetWatchdog(true, 0) // 间隔 <=0 回 1h 缺省
	if on, iv := h.watchdog(); !on || iv != time.Hour {
		t.Fatalf("on=%v iv=%s want true/1h", on, iv)
	}
}
