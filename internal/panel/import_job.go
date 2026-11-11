package panel

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// 导入进度（异步任务）与真正的写入循环。
//
// 为什么要把循环从 importCockpit 里拆出来：确认导入不是「一次请求一次返回」的
// 瞬时动作——每个覆盖项都要先拿 refresh token 去上游求证一次（几百毫秒到几秒），
// 上百个号能跑十几分钟。用户点了确认后看到的是一动不动的按钮，分不清是在跑、
// 卡死还是已经悄悄失败。拆成后台任务后，前端能按 GET …/jobs/{id} 轮询出
// 「已处理 37/120、新增 5、覆盖 30、跳过 2、正在导入 936****4」。

// importFileInfo 单个上传文件的解析结果：前端要按文件列出「哪个文件几条」，
// 否则多选三个文件只看得到一个总数，用户分不清少了的那个是没选上还是没解析。
type importFileInfo struct {
	Name  string `json:"name"`
	Items int    `json:"items"`
}

// importProgress 导入任务的可观测进度。字段直接透给前端画进度条。
type importProgress struct {
	State      string   `json:"state"` // running / done / failed
	Total      int      `json:"total"`
	Processed  int      `json:"processed"`
	Imported   int      `json:"imported"`
	Skipped    int      `json:"skipped"`
	Creates    int      `json:"creates"`
	Overwrites int      `json:"overwrites"`
	Errors     []string `json:"errors"`
	// Current 当前正在处理的账号（昵称优先，缺省 uid）：进度条之外那句「现在在跑谁」。
	Current string `json:"current"`
	// StartedAt/FinishedAt 是 unix 毫秒；ElapsedMS 让前端不必依赖本地时钟。
	StartedAt  int64 `json:"started_at"`
	FinishedAt int64 `json:"finished_at,omitempty"`
	ElapsedMS  int64 `json:"elapsed_ms"`
	// DryRun 让前端知道这是预览任务（预览不会往上游发请求，因此也快）。
	DryRun bool `json:"dry_run,omitempty"`
	// Error 任务级失败（不是单条跳过）：整个任务没跑起来的原因。
	Error string `json:"error,omitempty"`
}

// importProgressErrCap 进度里最多带多少条跳过明细：几百条清单传给前端只会
// 把进度条顶出视野，完整的明细仍在最终响应与日志里。
const importProgressErrCap = 200

// importOutcome 一次导入的最终统计（同步与异步共用）。
type importOutcome struct {
	Total      int
	Imported   int
	Skipped    int
	Creates    int
	Overwrites int
	Errors     []string
}

// importJob 一个后台导入任务的进度槽。只被持有者的 goroutine 写，读侧拿锁拍快照。
type importJob struct {
	mu   sync.Mutex
	prog importProgress
}

// newImportJob 建一个「已开跑」的任务槽：快照必须在后台 goroutine 真正起来之前
// 就报 running，否则第一次轮询拿到零值（state=""、total=0），调用方会当成
// 「任务已经结束」而提前收工——测试里表现为进度永远为空。
func newImportJob(total int) *importJob {
	j := &importJob{}
	j.prog = importProgress{State: "running", Total: total, StartedAt: time.Now().UnixMilli()}
	return j
}

func (j *importJob) snapshot() importProgress {
	j.mu.Lock()
	defer j.mu.Unlock()
	p := j.prog
	p.Errors = append([]string(nil), j.prog.Errors...)
	if len(p.Errors) > importProgressErrCap {
		p.Errors = p.Errors[:importProgressErrCap]
	}
	if !p.DryRun && p.State == "running" {
		p.ElapsedMS = time.Since(time.UnixMilli(p.StartedAt)).Milliseconds()
	}
	return p
}

// set 把一次快照写进任务槽（report 回调内部用）。
func (j *importJob) set(p importProgress) {
	j.mu.Lock()
	j.prog = p
	j.mu.Unlock()
}

// newImportJobID 任务 id：随机 12 位十六进制。导入任务只有本进程活着时才有意义，
// 不需要持久化，也不需要防重放——但也不能让前端猜（同一个面板可能多标签页）。
func newImportJobID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("j%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// registerImportJob 登记任务并顺手清理：已结束且超过 10 分钟的任务丢掉，
// 再把总量压到 importJobsCap 以内（常驻进程，不能让每次导入永久占一个槽）。
func (p *Panel) registerImportJob(j *importJob) string {
	id := newImportJobID()
	p.importJobsMu.Lock()
	if p.importJobs == nil {
		p.importJobs = map[string]*importJob{}
	}
	cutoff := time.Now().Add(-10 * time.Minute).UnixMilli()
	for k, v := range p.importJobs {
		prog := v.snapshot()
		if prog.State != "running" && prog.FinishedAt > 0 && prog.FinishedAt < cutoff {
			delete(p.importJobs, k)
		}
	}
	if len(p.importJobs) >= importJobsCap {
		// 超限时丢最老的已结束任务；全都还在跑就只能超一点（跑着的任务不能丢，
		// 否则前端轮询到 404，会以为导入失败）。
		var oldestKey string
		var oldest int64 = 1<<63 - 1
		for k, v := range p.importJobs {
			prog := v.snapshot()
			if prog.State == "running" {
				continue
			}
			if prog.StartedAt < oldest {
				oldest, oldestKey = prog.StartedAt, k
			}
		}
		if oldestKey != "" {
			delete(p.importJobs, oldestKey)
		}
	}
	p.importJobs[id] = j
	p.importJobsMu.Unlock()
	return id
}

// importJobsCap 同时在册的导入任务上限（正常运行下只有 1 个）。
const importJobsCap = 32

// lookupImportJob 取任务，不存在返回 nil。
func (p *Panel) lookupImportJob(id string) *importJob {
	p.importJobsMu.Lock()
	j := p.importJobs[id]
	p.importJobsMu.Unlock()
	return j
}

// importJobStatus 轮询入口：
//
//	GET /panel/api/import/cockpit/jobs/{id}
//
// 返回 importProgress。任务不存在 → 404（前端据此停下轮询并提示「任务已失效」，
// 而不是无限打下去）。
func (p *Panel) importJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j := p.lookupImportJob(id)
	if j == nil {
		writeErr(w, http.StatusNotFound, "导入任务不存在或已过期")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, j.snapshot())
}

// runImport 真正写盘的循环（同步与后台任务共用同一份实现）。
//
// report 为 nil 表示不需要进度（同步调用）；非 nil 时每条账号处理完就回调一次，
// 回调里不要做重活——它在导入的热路径上。
func (p *Panel) runImport(accounts []cockpitAccount, sources []string, dryRun bool, report func(importProgress)) importOutcome {
	started := time.Now()
	var st importProgress
	st.State = "running"
	st.Total = len(accounts)
	st.DryRun = dryRun
	st.StartedAt = started.UnixMilli()

	push := func() {
		if report == nil {
			return
		}
		// 拷贝一份再交出去：回调可能把它塞进 job 槽或直接序列化，之后 st 还会被改。
		out := st
		out.Errors = append([]string(nil), st.Errors...)
		out.ElapsedMS = time.Since(started).Milliseconds()
		report(out)
	}

	// fail 记一条「这条没导成」：计数、明细、进度一起推进，调用方直接 continue。
	fail := func(label, msg string) {
		st.Skipped++
		st.Errors = append(st.Errors, msg)
		st.Processed++
		st.Current = label
		push()
	}
	// ok 记一条成功导入。
	ok := func(label string) {
		st.Imported++
		st.Processed++
		st.Current = label
		push()
	}

	seenUID := map[string]string{} // uid -> 首次出现的文件，用于批内去重
	push()

	for i, acc := range accounts {
		src := sources[i]
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		label := strings.TrimSpace(acc.Nickname)
		if label == "" {
			label = uid
		}
		if label == "" && acc.ID != "" {
			label = acc.ID
		}
		if uid == "" || at == "" || rt == "" {
			fail(label, fmt.Sprintf("%s: missing required fields (id=%s)", src, acc.ID))
			continue
		}
		if !validImportUID(uid) {
			fail(label, fmt.Sprintf("%s: invalid uid (id=%s)", src, acc.ID))
			continue
		}
		// 批内去重：多个文件可能有重叠账号。保留第一次出现的那条（选择顺序即
		// 提交顺序，先选的优先），后面的记跳过并说明来源，不静默丢弃。
		if first, dup := seenUID[uid]; dup {
			if first == src {
				fail(label, fmt.Sprintf("%s: duplicate uid=%s in the same file", src, uid))
			} else {
				fail(label, fmt.Sprintf("%s: duplicate uid=%s (already provided by %s)", src, uid, first))
			}
			continue
		}
		seenUID[uid] = src

		// 文件里带了 realm 就用它（cn/global 之外的值退回按 domain 推断）。
		realm := importRealm(acc.Realm, acc.Domain)

		// expires_at 先统一成毫秒再转秒：批量导出是毫秒，单账号详情导出是秒，
		// 直接除以 1000 会把秒级值算成 1970 年（账号一进池就是「已过期」）。
		expiresAt := normalizeExpiresAtMillis(acc.ExpiresAt) / 1000
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}
		enterpriseID := strings.TrimSpace(acc.EnterpriseID)
		if enterpriseID == "" {
			enterpriseID = strings.TrimSpace(acc.EnterpriseIDCamel)
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			EnterpriseID: enterpriseID,
			Nickname:     nickname,
			FilePath:     filepath.Join(p.authDir(), fmt.Sprintf("workbuddy-%s.json", uid)),
		}

		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				fail(label, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			// 显式写 cn，而不是让 BackfillRealm 按 domain 再推一遍：文件里说了是
			// 国内版就照写，避免 domain 缺失/通用域名时推出的口径与 realm 变量不一致。
			if _, err := auth.BackfillRealmFor(a, "cn"); err != nil {
				fail(label, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		}

		// 覆盖已有凭据是不可逆动作：先用 refresh 求证这份新凭据还活着，只接受
		// 「明确活着」。起因是把一份旧 cockpit 导出重新导入时，文件里的 RT 早已被
		// 轮换，直接覆盖会让本来能用的账号变成要重新登录——.bak 里虽然留着旧凭据，
		// 但没有任何东西会自动拿它回退。判据用 VerifySessionByRefresh 的三态
		// （alive/dead/inconclusive）：dead 是上游明确拒绝，inconclusive 是没判词
		// （网络/5xx/429），后者同样不覆盖——主动销毁时没有判词就不能动手。
		// 顺带一提：alive 分支里 a 已被 refresh 轮换过 RT，下面的落盘存的是新值。
		existing := p.cfg.Pool.AuthByUID(uid)
		if existing == nil {
			st.Creates++
		} else {
			st.Overwrites++
		}
		if dryRun {
			// 预览到此为止，**绝不**往上游发请求：VerifySessionByRefresh 会真的
			// 轮换一次 refresh token（单次有效），只为看一眼就烧掉一个 RT 不行，
			// 更别说它还会顺手改掉内存里的凭据。真实导入时这份校验照旧执行。
			ok(label)
			continue
		}
		if existing != nil {
			verdict := p.cfg.Upstream.VerifySessionByRefresh(a)
			if verdict != upstream.VerifyAlive {
				fail(label, fmt.Sprintf("%s: 新凭据校验未通过（%s），保留原有凭据", src, verdict))
				continue
			}
		}

		if err := a.SaveAtomic(); err != nil {
			fail(label, fmt.Sprintf("%s: save auth failed: %v", src, err))
			continue
		}

		isNewAccount := existing == nil
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", uid, err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", uid)
			}
		} else {
			// 企业版 / 静态 API Key 无个人成长体系：CN 签到端点对企业号直接
			// 400 code 10001「企业账号不支持该操作」。跳过签到，但下方余额查询
			// 照常（企业额度走 get-enterprise-user-usage 口径）。
			if !a.SupportsPersonalGrowth() {
				log.Printf("panel: import 跳过签到 uid=%s（企业版/API Key 无个人成长体系）", uid)
			} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", uid, err)
			}
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
		}

		if isNewAccount {
			p.StartNewAccountTasks(uid)
		}
		ok(label)
	}

	st.State = "done"
	st.FinishedAt = time.Now().UnixMilli()
	st.Current = ""
	push()

	return importOutcome{
		Total:      st.Total,
		Imported:   st.Imported,
		Skipped:    st.Skipped,
		Creates:    st.Creates,
		Overwrites: st.Overwrites,
		Errors:     st.Errors,
	}
}
