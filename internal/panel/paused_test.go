package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// 暂停选号（paused）的面板接线：POST /pause 与 /resume 必须落到池状态上，
// 且全程不动 disabled（吸收上游 fd835df）。
func TestAccountPauseAndResumeWireToPool(t *testing.T) {
	p := pool.New(nil)
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at1"})
	pn := New(Config{Version: "test", Pool: p})

	call := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		return rec
	}
	if rec := call("/panel/api/accounts/u1/pause"); rec.Code != http.StatusOK {
		t.Fatalf("pause code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Paused {
		t.Errorf("pause 后 Status.Paused 应为 true: %+v", st)
	}
	if st.Disabled {
		t.Errorf("暂停不是禁用：Disabled 必须为 false: %+v", st)
	}
	// 暂停后选号应跳过该号（唯一号 → nil）
	if got := p.Pick(); got != nil {
		t.Errorf("暂停号不应被选中，got %+v", got)
	}

	if rec := call("/panel/api/accounts/u1/resume"); rec.Code != http.StatusOK {
		t.Fatalf("resume code=%d body=%s", rec.Code, rec.Body)
	}
	if st, _ := p.Status("u1"); st.Paused {
		t.Errorf("resume 后 Paused 应为 false: %+v", st)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Errorf("resume 后应可选，got %+v", got)
	}

	// 未知 uid → 404（供前端区分「账号不存在」）
	if rec := call("/panel/api/accounts/nope/pause"); rec.Code != http.StatusNotFound {
		t.Errorf("pause 未知 uid code=%d want 404", rec.Code)
	}
	if rec := call("/panel/api/accounts/nope/resume"); rec.Code != http.StatusNotFound {
		t.Errorf("resume 未知 uid code=%d want 404", rec.Code)
	}
}

// 账号列表端点必须把 paused 透出给面板（前端据此渲染「已暂停选号」与「恢复选号」按钮）。
//
// 数据源从 /overview 搬到 /panel/api/accounts：账号列表改成数据库分页后，overview
// 只留统计数字，行级字段（paused/disabled/积分…）由分页端点负责。
func TestAccountsPageExposesPausedFlag(t *testing.T) {
	p := pool.New(nil)
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at1"})
	p.Pause("u1")
	pn := New(Config{Version: "test", Pool: p})

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts code=%d body=%s", rec.Code, rec.Body)
	}
	var d struct {
		Total    int `json:"total"`
		Accounts []struct {
			UID    string `json:"uid"`
			Paused bool   `json:"paused"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Accounts) != 1 || !d.Accounts[0].Paused {
		t.Fatalf("accounts 未透出 paused: %s", rec.Body)
	}

	// 概况卡片分开计数（吸收上游 PR #130 / issue #125）：暂停只关选号、照常签到保活，
	// 与禁用是两种运维状态。此前这里断言 paused 计入 disabled——那正是被修掉的口径。
	ovRec := httptest.NewRecorder()
	pn.ServeHTTP(ovRec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if ovRec.Code != http.StatusOK {
		t.Fatalf("overview code=%d body=%s", ovRec.Code, ovRec.Body)
	}
	var o struct {
		Disabled int `json:"disabled"`
		Paused   int `json:"paused"`
	}
	if err := json.Unmarshal(ovRec.Body.Bytes(), &o); err != nil {
		t.Fatal(err)
	}
	if o.Paused != 1 {
		t.Errorf("overview.paused=%d want 1（暂停单独计数）", o.Paused)
	}
	if o.Disabled != 0 {
		t.Errorf("overview.disabled=%d want 0（暂停不得算进已禁用）", o.Disabled)
	}
}

// 模型锁池的接线：overview 的 model_locks 必须与池内真实的模型级冷却同源，无锁时为 null。
func TestOverviewExposesModelLocks(t *testing.T) {
	p := pool.New(nil)
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", Nickname: "B", AccessToken: "at2"})
	pn := New(Config{Version: "test", Pool: p})

	read := func() []map[string]any {
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
		if rec.Code != 200 {
			t.Fatalf("overview code=%d", rec.Code)
		}
		var d struct {
			ModelLocks []map[string]any `json:"model_locks"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		return d.ModelLocks
	}
	if got := read(); len(got) != 0 {
		t.Fatalf("无冷却时 model_locks 应为空，得到 %+v", got)
	}
	// 一个号对该模型停车 → 出一个 partial 行（口径与选号一致）。
	p.CooldownSoftForModel("u1", 10*time.Minute, time.Time{}, "glm-5.3", "model rate limit")
	rows := read()
	if len(rows) != 1 || rows[0]["model"] != "glm-5.3" || rows[0]["state"] != "partial" {
		t.Fatalf("model_locks = %+v want 一行 partial glm-5.3", rows)
	}
}
