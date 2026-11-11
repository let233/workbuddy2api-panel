package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	// Realm 导出文件自带的域标识（cn/global）。此前被丢掉、只按 domain 反推：
	// 国际版账号若导出时 domain 是通用域名（或为空），导回来会被当成国内版，
	// 之后所有上游请求打错端点。
	Realm          string `json:"realm"`
	DosageNotify   string `json:"dosage_notify_code"`
	PaymentType    string `json:"payment_type"`
	Status         string `json:"status"`
	UsageUpdatedAt int64  `json:"usage_updated_at"`
	LastCheckin    int64  `json:"last_checkin_time"`
	CheckinStreak  int    `json:"checkin_streak"`
	CreatedAt      int64  `json:"created_at"`
	LastUsed       int64  `json:"last_used"`

	// EnterpriseID 兼容导出工具的两种写法：cockpit 字段多为 snake_case，
	// auth 文件与登录接口用 camelCase。任一带值即视为企业版，供签到 /
	// 成长任务门控与额度口径使用（见 auth.IsEnterprise）。
	EnterpriseID      string `json:"enterprise_id"`
	EnterpriseIDCamel string `json:"enterpriseId"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>（可重复，按表单顺序逐个解码）
//	      dryRun=true|1 → 只做校验与预览，不落盘、不上游校验、不签到
//
// 多个文件是「整体事务」：任一文件不是合法 JSON 或没有任何账号就整体 400，不做
// 「先把前面导进去」（导入不可逆，半成功最难收拾）。
//
// 返回 {ok, total, imported, skipped, errors, creates, overwrites, dry_run}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	dryRun := strings.EqualFold(strings.TrimSpace(r.FormValue("dryRun")), "true") ||
		strings.TrimSpace(r.FormValue("dryRun")) == "1"

	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}

	var accounts []cockpitAccount
	var sources []string
	var files []importFileInfo // 与 accounts 平行：每个条目来自哪个文件，出错时能说清
	for _, fh := range headers {
		f, err := fh.Open()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "open "+fh.Filename+": "+err.Error())
			return
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": read file: "+err.Error())
			return
		}

		list, err := decodeCockpitAccounts(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fh.Filename+": invalid json: "+err.Error())
			return
		}
		if len(list) == 0 {
			writeErr(w, http.StatusBadRequest, fh.Filename+": empty accounts array")
			return
		}
		files = append(files, importFileInfo{Name: fh.Filename, Items: len(list)})
		for _, acc := range list {
			accounts = append(accounts, acc)
			sources = append(sources, fh.Filename)
		}
	}

	// 凭证目录必须在第一次落盘前就位。login.go / apikey.go 各自在写盘前建了目录，
	// 唯独本文件漏了：全新部署（auths/ 还没生成、也没登录过任何账号）时，写盘用的
	// 临时文件创建直接 ENOENT，用户看到的是「导入失败」，而服务本身完全正常
	// ——LoadDir 用 filepath.Glob，目录不存在时返回空列表且不报错，所以启动日志
	// 一切正常（loaded 0 account(s)），问题只在导入时暴露。
	// 放在循环外建一次：目录是进程级前置条件，逐个账号重复 MkdirAll 既无意义，
	// 也会在部分账号失败时给出误导性错误。预览不建任何东西。
	if !dryRun {
		if err := os.MkdirAll(p.authDir(), 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
			return
		}
	}

	if dryRun {
		// 预览：只统计不落盘、不发上游请求，可以同步返回（很快）。
		out := p.runImport(accounts, sources, true, nil)
		log.Printf("panel: cockpit import preview total=%d would_import=%d skipped=%d creates=%d overwrites=%d",
			out.Total, out.Imported, out.Skipped, out.Creates, out.Overwrites)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"dry_run":    true,
			"total":      out.Total,
			"imported":   out.Imported, // 预览时是「将会导入」的条数
			"skipped":    out.Skipped,
			"creates":    out.Creates,
			"overwrites": out.Overwrites,
			"errors":     out.Errors,
			"files":      files,
		})
		return
	}

	// 真实导入：逐号要先用 refresh 向上游求证（覆盖项），上百个号能跑十几分钟。
	// 异步模式下立刻返回 job_id，写入在后台跑，前端轮询 …/jobs/{id} 画进度条；
	// 这是「确认导入要能看到实时进展」的实现方式——同步等待只会让按钮一直转圈，
	// 用户分不清是在跑还是已经挂了。
	if importAsync(r) {
		job := newImportJob(len(accounts))
		id := p.registerImportJob(job)
		go func() {
			out := p.runImport(accounts, sources, false, job.set)
			log.Printf("panel: cockpit import finished job=%s total=%d imported=%d skipped=%d creates=%d overwrites=%d",
				id, out.Total, out.Imported, out.Skipped, out.Creates, out.Overwrites)
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":      true,
			"dry_run": false,
			"async":   true,
			"job_id":  id,
			"total":   len(accounts),
			"files":   files,
		})
		return
	}

	out := p.runImport(accounts, sources, false, nil)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d creates=%d overwrites=%d",
		out.Total, out.Imported, out.Skipped, out.Creates, out.Overwrites)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"dry_run":    false,
		"total":      out.Total,
		"imported":   out.Imported,
		"skipped":    out.Skipped,
		"creates":    out.Creates,
		"overwrites": out.Overwrites,
		"errors":     out.Errors,
		"files":      files,
	})
}

// importAsync 是否为「后台任务 + 进度轮询」模式（确认导入走这条）。
// 只认显式的 async=1/true：默认仍是同步返回，脚本/测试的旧语义不受影响。
func importAsync(r *http.Request) bool {
	v := strings.TrimSpace(r.FormValue("async"))
	return v == "1" || strings.EqualFold(v, "true")
}

// importRealm 归一化导入文件里的 realm：只认 cn/global（大小写/空白不敏感），
// 其它值（"intl"/"sg"/拼错）退回按 domain 推断——脏值写进凭据会被
// BackfillRealmFor 拒绝，或让账号在域统计里凭空少一个。
func importRealm(explicit, domain string) string {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "global":
		return "global"
	case "cn":
		return "cn"
	}
	// 按 domain 推断：workbuddy.ai 家族 → global，否则 cn。
	return auth.ResolveRealm("", domain)
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 导入格式归一化
// ---------------------------------------------------------------------------

// nestedCockpit 是 cockpit tools「账号详情」导出的嵌套形态：
//
//	{"account":{"uid":"…","nickname":"…"},"auth":{"accessToken":"…","refreshToken":"…"}}
//
// 同一个导出工具会产出两种形态：批量导出是「顶层数组 + snake_case」，单账号详情
// 导出是上面这种嵌套驼峰。用户不会区分，故在解码处统一，而不是让人手工改写。
type nestedCockpit struct {
	Account struct {
		UID           string `json:"uid"`
		Nickname      string `json:"nickname"`
		EnterpriseID  string `json:"enterpriseId"`
		EnterpriseID2 string `json:"enterprise_id"`
	} `json:"account"`
	Auth struct {
		AccessToken   string `json:"accessToken"`
		AccessToken2  string `json:"access_token"`
		RefreshToken  string `json:"refreshToken"`
		RefreshToken2 string `json:"refresh_token"`
		Domain        string `json:"domain"`
		Realm         string `json:"realm"`
		ExpiresAt     int64  `json:"expiresAt"`
		ExpiresAt2    int64  `json:"expires_at"`
	} `json:"auth"`
}

// decodeCockpitAccounts 把导入 JSON 归一成账号列表。接受：
//   - 顶层数组（批量导出）
//   - 单个扁平对象（蛇形或驼峰）
//   - 单个嵌套对象 {account,auth}（账号详情导出）
//   - 信封 {accounts|data|list|items|results|auths: [...]}
//   - 字典 {"<uid>": {...}}（键即 uid）
//
// 认不出的结构才返回 error（调用方转 400）；认得出但缺字段的条目照旧进主循环
// 记 skipped——错误信息里要能说出是哪一条，不能被解码层吞掉。
func decodeCockpitAccounts(raw []byte) ([]cockpitAccount, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, err
		}
		return decodeCockpitItems(items), nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, err
		}
		for _, k := range []string{"accounts", "data", "list", "items", "results", "auths"} {
			sub, ok := obj[k]
			if !ok {
				continue
			}
			var items []json.RawMessage
			if err := json.Unmarshal(sub, &items); err == nil {
				return decodeCockpitItems(items), nil
			}
			// 信封的值是**单个账号对象**（用户把单账号详情又包了一层）：
			// 按条目解，但绝不能拿信封键当 uid——见字典分支的说明。
			if acc, ok := decodeCockpitOne(sub); ok {
				return []cockpitAccount{acc}, nil
			}
		}
		if _, ok := obj["account"]; ok {
			return decodeCockpitItems([]json.RawMessage{trimmed}), nil
		}
		if _, ok := obj["auth"]; ok {
			return decodeCockpitItems([]json.RawMessage{trimmed}), nil
		}
		if acc, ok := decodeCockpitOne(trimmed); ok {
			return []cockpitAccount{acc}, nil
		}
		// 字典：{"<uid>": {...}}，键当 uid 兜底
		//
		// 信封名不能当 uid：`{"accounts":{"refresh_token":"r"}}` 是「信封的值是单个
		// 对象」形态，走到这里时 accounts 这个键会命中内层（有 token），兜底就会把
		// 账号的 uid 写成字面量 "accounts"——池里从此多一个格式合法、内容垃圾的号。
		// 信封名本身不是 uid（真实 uid 是手机号/UUID），直接跳过。
		keys := make([]string, 0, len(obj))
		for k := range obj {
			if isCockpitEnvelopeKey(k) {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys) // 稳定顺序，便于前端展示与测试比对
		out := make([]cockpitAccount, 0, len(keys))
		for _, k := range keys {
			acc, ok := decodeCockpitOne(obj[k])
			if !ok {
				continue
			}
			if strings.TrimSpace(acc.UID) == "" {
				acc.UID = k
			}
			out = append(out, acc)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("无法识别的 JSON 结构：既不是账号数组，也不是 {account,auth} 或 {\"<uid>\":{…}}")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected JSON array or object")
	}
}

// isCockpitEnvelopeKey 报告键名是否是导出工具的信封名（不是 uid）。
func isCockpitEnvelopeKey(k string) bool {
	for _, e := range []string{"accounts", "data", "list", "items", "results", "auths"} {
		if strings.EqualFold(strings.TrimSpace(k), e) {
			return true
		}
	}
	return false
}

// decodeCockpitItems 逐条解码；认不出的条目保留一个空占位，
// 让主循环按「missing required fields」记账，而不是从结果里凭空消失。
func decodeCockpitItems(items []json.RawMessage) []cockpitAccount {
	out := make([]cockpitAccount, 0, len(items))
	for _, it := range items {
		if acc, ok := decodeCockpitOne(it); ok {
			out = append(out, acc)
			continue
		}
		out = append(out, cockpitAccount{})
	}
	return out
}

// decodeCockpitOne 依次尝试：蛇形扁平 → 驼峰扁平 → {account,auth} 嵌套。
// 判据统一是「拿到至少一个 token」：cockpitAccount 的 tag 是蛇形，直接
// Unmarshal 一个驼峰对象会成功但字段全空，不检查就会把有效账号当成空账号。
func decodeCockpitOne(raw json.RawMessage) (cockpitAccount, bool) {
	var a cockpitAccount
	if err := json.Unmarshal(raw, &a); err == nil && (a.AccessToken != "" || a.RefreshToken != "") {
		return a, true
	}
	var f struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
		EnterpriseID string `json:"enterpriseId"`
	}
	if err := json.Unmarshal(raw, &f); err == nil && (f.AccessToken != "" || f.RefreshToken != "") {
		a.UID = f.UID
		a.Nickname = f.Nickname
		a.AccessToken = f.AccessToken
		a.RefreshToken = f.RefreshToken
		a.ExpiresAt = f.ExpiresAt
		a.Domain = f.Domain
		a.Realm = f.Realm
		a.EnterpriseIDCamel = f.EnterpriseID
		return a, true
	}
	var n nestedCockpit
	if err := json.Unmarshal(raw, &n); err == nil {
		at := firstNonEmpty(n.Auth.AccessToken, n.Auth.AccessToken2)
		rt := firstNonEmpty(n.Auth.RefreshToken, n.Auth.RefreshToken2)
		if at != "" || rt != "" {
			return cockpitAccount{
				UID:               n.Account.UID,
				Nickname:          n.Account.Nickname,
				EnterpriseIDCamel: firstNonEmpty(n.Account.EnterpriseID, n.Account.EnterpriseID2),
				AccessToken:       at,
				RefreshToken:      rt,
				Domain:            n.Auth.Domain,
				Realm:             n.Auth.Realm,
				ExpiresAt:         firstNonZero(n.Auth.ExpiresAt, n.Auth.ExpiresAt2),
			}, true
		}
	}
	return a, false
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func firstNonZero(v ...int64) int64 {
	for _, n := range v {
		if n != 0 {
			return n
		}
	}
	return 0
}

// normalizeExpiresAtMillis 把 expires_at 统一成毫秒。
// 批量导出是毫秒（≈1.7e12），单账号详情导出是秒（≈1.7e9）。
// 以 1e11 分界：秒制要走 3000 年才到这个数，毫秒制的历史值也远大于它。
func normalizeExpiresAtMillis(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 1e11 {
		return v * 1000
	}
	return v
}
