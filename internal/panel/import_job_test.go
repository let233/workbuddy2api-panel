package panel

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 确认导入必须能看到实时进展，而不是一个转圈的按钮。
//
// 这条契约由三件事共同保证，任一缺失都会退化成「点了没反应」：
//  1. 带 async=1 的确认导入立刻回 202 + job_id（不阻塞十几分钟）；
//  2. 后台循环把每条账号的进度写进任务槽（processed/creates/current/state）；
//  3. GET …/jobs/{id} 能把快照读出来，任务不存在时 404 让前端停表。
//
// 变异判据：把 importAsync 的早退删掉（退回同步），或用 nil report 跑后台任务，
// 本用例会红在 202/job_id 与 processed>0 上。

// postImportFiles 按给定顺序发多个文件，附加表单字段（dryRun / async）。
func postImportFiles(t *testing.T, e *importPreviewEnv, fields map[string]string, names []string, files map[string][]byte) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range names {
		raw, ok := files[name]
		if !ok {
			continue
		}
		fw, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

type importAsyncResp struct {
	OK     bool   `json:"ok"`
	Async  bool   `json:"async"`
	DryRun bool   `json:"dry_run"`
	JobID  string `json:"job_id"`
	Total  int    `json:"total"`
	Files  []struct {
		Name  string `json:"name"`
		Items int    `json:"items"`
	} `json:"files"`
}

// getJob 轮询一次任务快照。
func getJob(t *testing.T, e *importPreviewEnv, id string) (int, importProgress) {
	t.Helper()
	req := httptest.NewRequest("GET", "/panel/api/import/cockpit/jobs/"+id, nil)
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	var p importProgress
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode progress: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, p
}

// waitJob 等到任务不再是 running（或超时）。
func waitJob(t *testing.T, e *importPreviewEnv, id string) importProgress {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, p := getJob(t, e, id)
		if code != http.StatusOK {
			t.Fatalf("轮询任务 %s 返回 %d", id, code)
		}
		if p.State != "running" {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("任务 %s 10s 内没结束（processed=%d/%d）", id, p.Processed, p.Total)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 确认导入（async=1）立刻 202 + job_id，随后能被轮询到 done，账号确实落盘。
func TestImportAsyncReturnsJobAndReportsProgress(t *testing.T) {
	e := newImportPreviewEnv(t)
	body := cockpitFile(t,
		map[string]any{"uid": "u1", "nickname": "甲", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().UnixMilli()},
		map[string]any{"uid": "u2", "nickname": "乙", "access_token": "at2", "refresh_token": "rt2", "expires_at": time.Now().UnixMilli()},
	)
	code, out := postImportFiles(t, e, map[string]string{"async": "1"}, []string{"a.json"}, map[string][]byte{"a.json": body})
	if code != http.StatusAccepted {
		t.Fatalf("code=%d want 202（确认导入必须立刻返回任务号）: %s", code, out)
	}
	var r importAsyncResp
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode 202: %v (%s)", err, out)
	}
	if !r.OK || !r.Async || r.JobID == "" {
		t.Fatalf("202 响应缺 ok/async/job_id：%s", out)
	}
	if r.Total != 2 || len(r.Files) != 1 || r.Files[0].Name != "a.json" || r.Files[0].Items != 2 {
		t.Fatalf("202 响应的 total/files 不对：%s", out)
	}

	p := waitJob(t, e, r.JobID)
	if p.State != "done" {
		t.Fatalf("state=%q want done（%+v）", p.State, p)
	}
	if p.Total != 2 || p.Processed != 2 || p.Imported != 2 || p.Skipped != 0 || p.Creates != 2 {
		t.Fatalf("进度计数不对：%+v", p)
	}
	if p.FinishedAt <= 0 || p.ElapsedMS < 0 {
		t.Fatalf("结束时间/耗时不合法：finished=%d elapsed=%d", p.FinishedAt, p.ElapsedMS)
	}
	for _, uid := range []string{"u1", "u2"} {
		if _, err := os.Stat(filepath.Join(e.dir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatalf("%s 未落盘：%v", uid, err)
		}
	}
	if n := len(e.panel.cfg.Pool.List()); n != 2 {
		t.Fatalf("pool=%d want 2", n)
	}
}

// 预览（dryRun）也要按文件报条数：多选三个文件只看得到一个总数，用户分不清
// 少了的那个是没选上还是没解析。
func TestImportPreviewReportsPerFileCounts(t *testing.T) {
	e := newImportPreviewEnv(t)
	a := cockpitFile(t,
		map[string]any{"uid": "u1", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().UnixMilli()},
	)
	b := cockpitFile(t,
		map[string]any{"uid": "u2", "access_token": "at2", "refresh_token": "rt2", "expires_at": time.Now().UnixMilli()},
		map[string]any{"uid": "u3", "access_token": "at3", "refresh_token": "rt3", "expires_at": time.Now().UnixMilli()},
	)
	code, out := postImportFiles(t, e, map[string]string{"dryRun": "true"}, []string{"a.json", "b.json"},
		map[string][]byte{"a.json": a, "b.json": b})
	if code != http.StatusOK {
		t.Fatalf("code=%d body=%s", code, out)
	}
	var r importAsyncResp
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("decode preview: %v (%s)", err, out)
	}
	if !r.DryRun || r.Total != 3 || len(r.Files) != 2 {
		t.Fatalf("预览 total/files 不对：%s", out)
	}
	if r.Files[0].Name != "a.json" || r.Files[0].Items != 1 || r.Files[1].Name != "b.json" || r.Files[1].Items != 2 {
		t.Fatalf("按文件条数不对：%+v", r.Files)
	}
}

// 轮询一个不存在的任务必须 404：前端据此停表，而不是无限打下去。
func TestImportJobStatusUnknown(t *testing.T) {
	e := newImportPreviewEnv(t)
	code, p := getJob(t, e, "nope")
	if code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", code)
	}
	if p.State != "" {
		t.Fatalf("404 不该带进度：%+v", p)
	}
	req := httptest.NewRequest("GET", "/panel/api/import/cockpit/jobs/nope", nil)
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "导入任务不存在") {
		t.Fatalf("404 文案不对：%s", rec.Body.String())
	}
}

// runImport 的进度必须逐条推进（不是只在末尾给一个总数），且 current 用昵称。
// 同步调用（report=nil）与后台调用共用同一份循环，这里直接测循环本身。
func TestRunImportReportsEachStep(t *testing.T) {
	e := newImportPreviewEnv(t)
	accounts := []cockpitAccount{
		{UID: "u1", Nickname: "甲", AccessToken: "at1", RefreshToken: "rt1", ExpiresAt: time.Now().UnixMilli()},
		{UID: "u2", AccessToken: "", RefreshToken: "", ExpiresAt: time.Now().UnixMilli()}, // 缺字段
		{UID: "u3", Nickname: "丙", AccessToken: "at3", RefreshToken: "rt3", ExpiresAt: time.Now().UnixMilli()},
	}
	sources := []string{"a.json", "a.json", "a.json"}
	var snaps []importProgress
	out := e.panel.runImport(accounts, sources, true, func(pr importProgress) { snaps = append(snaps, pr) })

	if out.Total != 3 || out.Imported != 2 || out.Skipped != 1 {
		t.Fatalf("outcome 不对：%+v", out)
	}
	// 首帧 + 每条 + 末帧：至少 5 次回调，processed 单调不减到 3。
	if len(snaps) < 5 {
		t.Fatalf("回调次数太少（%d）：进度条会一直停在 0", len(snaps))
	}
	last := -1
	for i, s := range snaps {
		if s.Processed < last {
			t.Fatalf("第 %d 帧 processed 回退：%d -> %d", i, last, s.Processed)
		}
		last = s.Processed
		if s.Total != 3 {
			t.Fatalf("第 %d 帧 total=%d want 3", i, s.Total)
		}
	}
	final := snaps[len(snaps)-1]
	if final.State != "done" || final.Processed != 3 || final.Current != "" {
		t.Fatalf("末帧不对：%+v", final)
	}
	// 逐条帧的 current 是正在进行/刚处理完的那个账号（昵称优先，缺昵称退回 uid）。
	var sawNick, sawUID bool
	for _, s := range snaps {
		if s.Current == "甲" || s.Current == "丙" {
			sawNick = true
		}
		if s.Current == "u2" {
			sawUID = true
		}
	}
	if !sawNick || !sawUID {
		t.Fatalf("current 没按账号推进（昵称优先、缺昵称退回 uid）：%+v", snaps)
	}
	if len(final.Errors) != 1 || !strings.Contains(final.Errors[0], "a.json: missing required fields") {
		t.Fatalf("跳过明细不对：%+v", final.Errors)
	}
}

// 任务槽：快照是拷贝（读侧改不动内部状态）、error 明细有上限、登记时清理老任务。
func TestImportJobSnapshotAndRegistry(t *testing.T) {
	j := &importJob{}
	j.set(importProgress{State: "running", Total: 300, Processed: 10, StartedAt: time.Now().Add(-2 * time.Second).UnixMilli()})
	j.mu.Lock()
	for i := 0; i < importProgressErrCap+50; i++ {
		j.prog.Errors = append(j.prog.Errors, "e")
	}
	j.mu.Unlock()

	snap := j.snapshot()
	if len(snap.Errors) != importProgressErrCap {
		t.Fatalf("快照 error 条数=%d want %d（明细要封顶）", len(snap.Errors), importProgressErrCap)
	}
	if snap.ElapsedMS <= 0 {
		t.Fatalf("running 任务应给出已用时：%d", snap.ElapsedMS)
	}
	snap.Errors[0] = "mutated"
	if j.snapshot().Errors[0] == "mutated" {
		t.Fatal("快照与内部状态共享了 errors 切片")
	}

	p := &Panel{}
	// 已结束且超过 10 分钟的任务在下次登记时被清掉。
	old := &importJob{}
	old.set(importProgress{State: "done", StartedAt: time.Now().Add(-time.Hour).UnixMilli(),
		FinishedAt: time.Now().Add(-30 * time.Minute).UnixMilli()})
	oldID := p.registerImportJob(old)
	fresh := &importJob{}
	fresh.set(importProgress{State: "done", StartedAt: time.Now().UnixMilli(), FinishedAt: time.Now().UnixMilli()})
	freshID := p.registerImportJob(fresh)
	if p.lookupImportJob(oldID) != nil {
		t.Fatal("结束超过 10 分钟的任务没被清理")
	}
	if p.lookupImportJob(freshID) == nil {
		t.Fatal("刚登记的任务查不到")
	}

	// 超上限时丢最老的已结束任务，跑着的任务不能丢（丢了前端会以为导入失败）。
	p2 := &Panel{}
	for i := 0; i < importJobsCap; i++ {
		x := &importJob{}
		x.set(importProgress{State: "done", StartedAt: int64(i + 1), FinishedAt: time.Now().UnixMilli()})
		p2.registerImportJob(x)
	}
	running := &importJob{}
	running.set(importProgress{State: "running", StartedAt: 0})
	runningID := p2.registerImportJob(running)
	if p2.lookupImportJob(runningID) == nil {
		t.Fatal("超限清理把正在跑的任务丢了")
	}
	if n := len(p2.importJobs); n > importJobsCap {
		t.Fatalf("在册任务=%d want <= %d", n, importJobsCap)
	}
}

// 后台任务写进度时不能被同步路径的 nil report 影响：dryRun 走同步、真实导入走任务，
// 两者统计口径必须一致（同样的账号 → 同样的 creates/overwrites/imported）。
func TestImportAsyncAndSyncAgree(t *testing.T) {
	mk := func() []byte {
		return cockpitFile(t,
			map[string]any{"uid": "u1", "access_token": "at1", "refresh_token": "rt1", "expires_at": time.Now().UnixMilli()},
			map[string]any{"uid": "u2", "access_token": "", "refresh_token": "rt2"},
		)
	}
	e := newImportPreviewEnv(t)
	code, out := postImportFiles(t, e, nil, []string{"a.json"}, map[string][]byte{"a.json": mk()})
	if code != http.StatusOK {
		t.Fatalf("同步导入 code=%d body=%s", code, out)
	}
	var sync importPreviewResp
	if err := json.Unmarshal([]byte(out), &sync); err != nil {
		t.Fatalf("decode: %v", err)
	}

	e2 := newImportPreviewEnv(t)
	code, out = postImportFiles(t, e2, map[string]string{"async": "true"}, []string{"a.json"},
		map[string][]byte{"a.json": mk()})
	if code != http.StatusAccepted {
		t.Fatalf("异步导入 code=%d body=%s", code, out)
	}
	var ar importAsyncResp
	if err := json.Unmarshal([]byte(out), &ar); err != nil {
		t.Fatalf("decode async: %v", err)
	}
	p := waitJob(t, e2, ar.JobID)
	if p.Total != sync.Total || p.Imported != sync.Imported || p.Skipped != sync.Skipped ||
		p.Creates != sync.Creates || p.Overwrites != sync.Overwrites {
		t.Fatalf("同步/异步口径不一致：async=%+v sync=%+v", p, sync)
	}
	if len(p.Errors) != len(sync.Errors) {
		t.Fatalf("跳过明细条数不一致：async=%d sync=%d", len(p.Errors), len(sync.Errors))
	}
}
