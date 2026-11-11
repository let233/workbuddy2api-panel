package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/gatewaykeys"
)

// 客户端 API Key（网关下发给用户的多把 Key）。与 accounts/add_apikey 里的
// 「上游账号 Key」是两回事：那边是入池的上游凭证，这边是本网关签发、按 Key
// 限定到期时间/可用模型/积分额度/提示词策略的准入凭证。
//
// 全量配置（含分组模板）都落在 gateway_keys / gateway_key_templates 两张表，
// 由 gatewaykeys.Manager 持有内存副本并定时回写；这里只做 HTTP 编解码。

// gatewayKeyView 在 Key 之上补一个「当前状态」，省得前端各写一遍判定逻辑。
type gatewayKeyView struct {
	gatewaykeys.Key
	Status string `json:"status"`
}

func gatewayKeyViews(list []gatewaykeys.Key, now time.Time) []gatewayKeyView {
	out := make([]gatewayKeyView, 0, len(list))
	for _, k := range list {
		out = append(out, gatewayKeyView{Key: k, Status: k.StatusAt(now)})
	}
	return out
}

func (p *Panel) gatewayKeyMgr(w http.ResponseWriter) *gatewaykeys.Manager {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "gateway keys not enabled")
		return nil
	}
	return p.cfg.Keys
}

// gatewayGlobalPrompt 读全局提示词策略（config.json 的 prompt 段）：
// Key 的 prompt_mode 留空即跟随这里的值。
func (p *Panel) gatewayGlobalPrompt() map[string]any {
	out := map[string]any{"prompt_mode": "", "prompt_text": ""}
	if p.cfg.LoadConfig == nil {
		return out
	}
	v, err := p.cfg.LoadConfig()
	if err != nil || v == nil {
		return out
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return out
	}
	var cfg struct {
		Prompt struct {
			Mode string `json:"mode"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return out
	}
	out["prompt_mode"] = cfg.Prompt.Mode
	out["prompt_text"] = cfg.Prompt.Text
	return out
}

func gatewayKeyErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, gatewaykeys.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, gatewaykeys.ErrDuplicate):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

func decodeGatewayKeyBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}

// gatewayKeys GET /panel/api/apikeys：Key 列表 + 模板列表 + 全局提示词。
func (p *Panel) gatewayKeys(w http.ResponseWriter, r *http.Request) {
	m := p.gatewayKeyMgr(w)
	if m == nil {
		return
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":      gatewayKeyViews(m.List(), now),
		"templates": m.ListTemplates(),
		"global":    p.gatewayGlobalPrompt(),
		"now":       now.Unix(),
	})
}

// applyTemplateDefaults 用模板补空缺字段：用户显式填的优先，模板只兜底。
// 「可使用模型留空 = 不限」这类默认语义因此不会被模板悄悄改掉。
func (p *Panel) applyTemplateDefaults(in gatewaykeys.Key) (gatewaykeys.Key, bool) {
	id := strings.TrimSpace(in.TemplateID)
	if id == "" {
		return in, true
	}
	tpl, ok := p.cfg.Keys.GetTemplate(id)
	if !ok {
		return in, false
	}
	if len(in.Models) == 0 {
		in.Models = tpl.Models
	}
	if in.CreditLimit == 0 {
		in.CreditLimit = tpl.CreditLimit
	}
	if in.PromptMode == "" {
		in.PromptMode = tpl.PromptMode
	}
	if in.PromptText == "" {
		in.PromptText = tpl.PromptText
	}
	if in.ExpiresAt == 0 && tpl.ExpiryDays > 0 {
		in.ExpiresAt = time.Now().AddDate(0, 0, tpl.ExpiryDays).Unix()
	}
	in.TemplateID = tpl.ID
	return in, true
}

// gatewayKeyCreate POST /panel/api/apikeys/create
func (p *Panel) gatewayKeyCreate(w http.ResponseWriter, r *http.Request) {
	m := p.gatewayKeyMgr(w)
	if m == nil {
		return
	}
	// Enabled 用指针接：Key.Enabled 是 bool，"没传" 与 "传了 false" 在零值上
	// 分不开，而面板新建对话框允许一开始就建成停用状态。
	var body struct {
		gatewaykeys.Key
		Enabled *bool `json:"enabled"`
	}
	if !decodeGatewayKeyBody(w, r, &body) {
		return
	}
	in, ok := p.applyTemplateDefaults(body.Key)
	if !ok {
		writeErr(w, http.StatusNotFound, "模板不存在")
		return
	}
	opt := gatewaykeys.CreateOptions{Disabled: body.Enabled != nil && !*body.Enabled}
	k, err := m.CreateWith(in, opt)
	if err != nil {
		gatewayKeyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": gatewayKeyView{Key: k, Status: k.StatusAt(time.Now())}})
}

// gatewayKeyUpdate POST /panel/api/apikeys/update：配置可改，用量计数不动。
func (p *Panel) gatewayKeyUpdate(w http.ResponseWriter, r *http.Request) {
	m := p.gatewayKeyMgr(w)
	if m == nil {
		return
	}
	// 同上：Enabled 缺省表示「别动这个开关」，不能让一次改额度的请求顺手把 Key 停掉。
	var body struct {
		gatewaykeys.Key
		Enabled *bool `json:"enabled"`
	}
	if !decodeGatewayKeyBody(w, r, &body) {
		return
	}
	in := body.Key
	if body.Enabled != nil {
		in.Enabled = *body.Enabled
	} else if cur, ok := m.Get(in.ID); ok {
		in.Enabled = cur.Enabled
	}
	in, ok := p.applyTemplateDefaults(in)
	if !ok {
		writeErr(w, http.StatusNotFound, "模板不存在")
		return
	}
	k, err := m.Update(in)
	if err != nil {
		gatewayKeyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": gatewayKeyView{Key: k, Status: k.StatusAt(time.Now())}})
}

// gatewayKeyDelete POST /panel/api/apikeys/delete
func (p *Panel) gatewayKeyDelete(w http.ResponseWriter, r *http.Request) {
	m := p.gatewayKeyMgr(w)
	if m == nil {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if !decodeGatewayKeyBody(w, r, &in) {
		return
	}
	if err := m.Delete(in.ID); err != nil {
		gatewayKeyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// gatewayKeyResetUsage POST /panel/api/apikeys/reset_usage：额度周期重置。
func (p *Panel) gatewayKeyResetUsage(w http.ResponseWriter, r *http.Request) {
	m := p.gatewayKeyMgr(w)
	if m == nil {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if !decodeGatewayKeyBody(w, r, &in) {
		return
	}
	k, err := m.ResetUsage(in.ID)
	if err != nil {
		gatewayKeyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": gatewayKeyView{Key: k, Status: k.StatusAt(time.Now())}})
}

// gatewayKeyTemplateSave POST /panel/api/apikey_templates/save
// 分组模板 = 一组可复用的配置（可用模型/额度/提示词/有效天数），新建 Key 时套用。
func (p *Panel) gatewayKeyTemplateSave(w http.ResponseWriter, r *http.Request) {
	m := p.gatewayKeyMgr(w)
	if m == nil {
		return
	}
	var in gatewaykeys.Template
	if !decodeGatewayKeyBody(w, r, &in) {
		return
	}
	tpl, err := m.SaveTemplate(in)
	if err != nil {
		gatewayKeyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "template": tpl})
}

// gatewayKeyTemplateDelete POST /panel/api/apikey_templates/delete
func (p *Panel) gatewayKeyTemplateDelete(w http.ResponseWriter, r *http.Request) {
	m := p.gatewayKeyMgr(w)
	if m == nil {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if !decodeGatewayKeyBody(w, r, &in) {
		return
	}
	if err := m.DeleteTemplate(in.ID); err != nil {
		gatewayKeyErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
