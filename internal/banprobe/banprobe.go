// Package banprobe 探测「被封禁的账号在上游是否已解封」——真实发一次最小 AI 对话。
//
// 为什么必须是真实 chat（用户口径），而不是余额查询：
//   - 余额端点只证明 billing 侧放行；真正决定这个号能不能干活的是 chat 通道。
//     风控拦截（11140）、会话注销（12153 / 网关 401）、模型侧限制都只在 chat 上暴露，
//     余额 200 而 chat 401 的「降级 token」是实测存在的形态（issue #215）。
//   - 探针要能证明「解封」这个结论：只有真的跑完一次对话，才说明上游重新接受了这个号。
//
// 请求形态与生产第一个 token 完全相同（与 scheduler 的额度窗口探针同口径）：
// stream=true（上游只认流式）、max_tokens=16、一句话 "Hi"。AgentType=auxiliary
// 把它标记成内部维护请求，不算用户主对话。
//
// **只读**：本包不改任何池状态。解冻/删除由调用方按配置决定——探测是观测行为，
// 私自把号放回池子等于变相 revive（会立刻再撞一次风控）。
package banprobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

const (
	// DefaultModel 探测用模型：免费档、全池可用（面板可配覆盖，见 config
	// schedule.ban_probe_model）。探测只需要「一次成功的对话」，选最便宜的即可。
	DefaultModel = "deepseek-v4.1-flash"
	// Prompt / MaxTokens 探针请求要尽量小：目的只是走完一次对话，产出无所谓。
	Prompt    = "Hi"
	MaxTokens = 16
	// Timeout 单次探测的硬上限。取 60s：上游劣化期 TTFB 可能到几十秒，但探测
	// 不值得为它等更久（与额度窗口探针同口径）。
	Timeout = 60 * time.Second
)

// Verdict 探测结论。
type Verdict string

const (
	// Lifted 上游已放行（chat 200）：可以解冻。
	Lifted Verdict = "lifted"
	// StillBanned 仍被封禁：11140 账号级风控等账号故障。
	StillBanned Verdict = "still_banned"
	// Blocked 请求被拦截但会话仍有效：refresh 成功说明登录态没死，401/403 是
	// 风控层拦的（含 11140 封禁），结论仍是「没解封」。
	Blocked Verdict = "blocked"
	// SessionDead 登录态真失效：不是封禁问题，该重登。
	SessionDead Verdict = "session_dead"
	// Inconclusive 无结论：网络/限流/未知错误，稍后再试。
	Inconclusive Verdict = "inconclusive"
)

// Result 一次探测的结果（JSON 字段直接给面板用）。
type Result struct {
	Verdict Verdict `json:"verdict"`
	// Detail 上游原文或失败原因（运维/日志用）。
	Detail string `json:"detail,omitempty"`
	// Advice 给运维的一句话处置建议。
	Advice string `json:"advice,omitempty"`
	// Status 上游 HTTP 状态码（0 = 没拿到响应）。
	Status int `json:"status,omitempty"`
}

// Probe 发一次真实对话探测账号 a。
//
// model 为空用 DefaultModel。c 为 nil 时返回 Inconclusive（未装配上游的部署/测试）。
func Probe(ctx context.Context, c *upstream.Client, a *auth.Auth, model string) Result {
	if c == nil || a == nil {
		return Result{Verdict: Inconclusive, Detail: "upstream client not configured"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if model == "" {
		model = DefaultModel
	}
	body, err := requestBody(model)
	if err != nil {
		return Result{Verdict: Inconclusive, Detail: "构造请求失败: " + err.Error()}
	}
	pctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	rc, status, respBody, err := c.ChatStreamContext(pctx, a, body, "",
		upstream.ChatMeta{AgentType: upstream.AgentTypeAuxiliary}, nil)
	if rc != nil {
		// 读干净再关：不读完的响应体不归还连接（net/http 语义），而这条连接后面
		// 还要被真实请求复用。max_tokens=16 时这一步是毫秒级。
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) {
			return classify(c, a, ue)
		}
		return Result{Verdict: Inconclusive, Status: status, Detail: clip(err.Error())}
	}
	if status >= 400 {
		// ErrNone 的防御分支（≥400 未产生分类）：原文照给，判无结论。
		return Result{Verdict: Inconclusive, Status: status,
			Detail: "HTTP " + strconv.Itoa(status) + ": " + clip(string(respBody))}
	}
	return Result{Verdict: Lifted, Status: status,
		Advice: "上游已解封，可以解冻恢复使用"}
}

// classify 把上游错误翻译成探测结论。
//
// 401 两种成因在**响应体上完全一样**（会话被注销 vs 降级 token 仍可用），单看 401
// 分不出是「封禁」还是「该重登」——refresh 才是权威判词（与网关 chat 路径
// server/handler.go 的网关 401 分支同一套判据）。
func classify(c *upstream.Client, a *auth.Auth, ue *upstream.Error) Result {
	res := Result{Status: ue.Status, Detail: clip(ue.Msg)}
	switch {
	case ue.Kind == upstream.ErrAccountFault:
		res.Verdict = StillBanned
		res.Advice = "仍被封禁，继续等待；勿反复解冻（可能加重风控）"
	case ue.Kind == upstream.ErrSessionDead || ue.Status == 401:
		switch c.VerifySessionByRefresh(a) {
		case upstream.VerifyAlive:
			// 会话仍活：401 是风控层拦的（APISIX 拦 11140 也是这个形态）。
			res.Verdict = Blocked
			res.Advice = "会话仍有效，但请求被上游风控拦截（可能仍是封禁）—— 继续观察，勿反复解冻"
		case upstream.VerifyDead:
			res.Verdict = SessionDead
			res.Advice = "不是封禁问题，是登录态失效 —— 需重新登录"
		default:
			res.Verdict = Inconclusive
			res.Advice = "refresh 未给出明确判词（网络/限流等），稍后再试"
		}
	default:
		res.Verdict = Inconclusive
		res.Advice = "无明确结论（网络/限流等），稍后再试"
	}
	return res
}

// requestBody 构造探测请求体。
//
// stream:true 是必须的（上游只认流式，非流式会 400/挂住）。
func requestBody(model string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": Prompt},
		},
		"stream":     true,
		"max_tokens": MaxTokens,
	})
}

// clip 截断上游原文：日志/面板提示都不该被一整页 HTML 撑爆。
func clip(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
