// gateway_keys.go 客户端 API Key 鉴权（多 Key）。
//
// 多 Key 表是网关唯一的凭据来源：没有全局 api_key，也不存在万能钥匙。请求按明文
// secret 查表（sha256 + 常量时间比较），命中即带上该 Key 的配额/到期/模型/提示词
// 策略；查不到一律 401。
//
// cfg.Keys == nil 表示「本进程不做 Key 鉴权」——只有测试与纯内存用途会这样构造，
// cmd/server 永远传入非 nil 的 Manager 并靠首启播种的系统 Key 提供服务。
package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/gatewaykeys"
)

// gatewayKeyCtxKey 请求上下文里携带的客户端 Key（nil = 用全局 key 或未鉴权）。
type gatewayKeyCtxKey struct{}

// requestGatewayKey 取本次请求命中的客户端 Key；未命中返回 nil。
func requestGatewayKey(r *http.Request) *gatewaykeys.Key {
	if r == nil {
		return nil
	}
	k, _ := r.Context().Value(gatewayKeyCtxKey{}).(*gatewaykeys.Key)
	return k
}

// gatewayKeyID 取本次请求命中的客户端 Key ID；未命中（未鉴权/鉴权关闭）返回空。
func gatewayKeyID(r *http.Request) string {
	if k := requestGatewayKey(r); k != nil {
		return k.ID
	}
	return ""
}

// gatewayKeyIDCtx 同 gatewayKeyID，但用于没有 *http.Request 的执行路径
// （如托管工具里的出图调用，只拿得到父请求的 context）。
func gatewayKeyIDCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if k, _ := ctx.Value(gatewayKeyCtxKey{}).(*gatewaykeys.Key); k != nil {
		return k.ID
	}
	return ""
}

// withGatewayKey 把 Key 挂到请求上下文（中间件用）。
func withGatewayKey(r *http.Request, k *gatewaykeys.Key) *http.Request {
	if k == nil {
		return r
	}
	return r.WithContext(contextWithGatewayKey(r.Context(), k))
}

func contextWithGatewayKey(ctx context.Context, k *gatewaykeys.Key) context.Context {
	return context.WithValue(ctx, gatewayKeyCtxKey{}, k)
}

// bearerSecret 取请求携带的明文密钥：Authorization: Bearer 优先，其次 x-api-key。
// （Anthropic 系客户端只发 x-api-key；两个头都在时以标准头为准。）
func bearerSecret(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		if tok := strings.TrimSpace(authz[len("Bearer "):]); tok != "" {
			return tok
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// authenticate 统一鉴权：只查客户端 Key（含生命周期校验）。没有全局 api_key 可退回。
// 返回的 status/code/message 非零即应当拒绝。
func (h *Handler) authenticate(r *http.Request) (*gatewaykeys.Key, int, string, string) {
	if h.cfg.Keys == nil {
		return nil, 0, "", ""
	}
	{
		if secret := bearerSecret(r); secret != "" {
			if k, ok := h.cfg.Keys.Resolve(secret); ok {
				kk := k
				switch k.StatusAt(time.Now()) {
				case gatewaykeys.StatusDisabled:
					return nil, http.StatusForbidden, "key_disabled", "this API key is disabled by the gateway administrator"
				case gatewaykeys.StatusExpired:
					return nil, http.StatusForbidden, "key_expired", "this API key has expired"
				case gatewaykeys.StatusQuotaExceeded:
					return nil, http.StatusTooManyRequests, "insufficient_quota", "this API key has used up its assigned credit quota"
				}
				return &kk, 0, "", ""
			}
		}
	}
	return nil, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key"
}

// anthropicErrorType 把 HTTP 状态映射到 Anthropic 错误类型（SDK 按 type 分支处理）。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	return "invalid_request_error"
}

// writeAuthFailure 按请求方协议写鉴权失败响应。
func writeAuthFailure(w http.ResponseWriter, r *http.Request, status int, code, msg string, anthropic bool) {
	if anthropic {
		writeAnthropicError(w, status, anthropicErrorType(status), msg)
		return
	}
	writeOpenAIError(w, status, code, msg)
}

// gatewayKeyAllowsModel 判断本次请求的 Key 是否允许该模型；无 Key（全局钥匙）时恒允许。
func gatewayKeyAllowsModel(r *http.Request, realm, bare string) bool {
	k := requestGatewayKey(r)
	if k == nil {
		return true
	}
	return k.AllowsModel(realm, bare)
}

// filterModelListForKey 按客户端 Key 的模型白名单过滤 /v1/models 列表。
// 列表条目 id 形如 "cn:xxx"/"global:xxx"，与 resolveModel 对称。
func filterModelListForKey(list []map[string]any, k *gatewaykeys.Key) []map[string]any {
	if k == nil || len(k.Models) == 0 {
		return list
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		id, _ := entry["id"].(string)
		realm, bare := resolveModel(id)
		if k.AllowsModel(realm, bare) {
			out = append(out, entry)
		}
	}
	return out
}
