package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 导出端点：形状必须与 cockpit tools 的账号详情导出一致（裸数组、驼峰、expiresAt 秒），
// 口径必须与列表页一致（带 uid 只导所选、整池导出受 realm/group 限制）。

type exportResp struct {
	Account struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	} `json:"auth"`
}

func (e *accountsEnv) export(t *testing.T, url string) ([]exportResp, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	e.panel.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		return nil, rec
	}
	var items []exportResp
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil {
		t.Fatalf("%s 解析失败: %v body=%s", url, err, rec.Body)
	}
	return items, rec
}

func TestAccountsExportByUIDKeepsOrder(t *testing.T) {
	e := newAccountsEnv(t)
	items, rec := e.export(t, "/panel/api/accounts/export?uid=u02&uid=u01")
	if len(items) != 2 {
		t.Fatalf("导出 %d 条，want 2（body=%s）", len(items), rec.Body)
	}
	if items[0].Account.UID != "u02" || items[1].Account.UID != "u01" {
		t.Fatalf("顺序 = %s,%s，want u02,u01（按请求顺序，不是 uid 升序）",
			items[0].Account.UID, items[1].Account.UID)
	}
	if items[0].Account.Nickname != "昵称-u02" || items[0].Auth.AccessToken != "at-u02" {
		t.Fatalf("字段没取到内存真值：%+v", items[0])
	}
	if items[0].Auth.Realm != "cn" {
		t.Fatalf("realm=%q want cn（导出必须自带域，否则国际版号导回会串域）", items[0].Auth.Realm)
	}
	// 凭据落盘前必须拦掉缓存；文件名要能直接看出来是哪个面板导的、导了几个。
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q want no-store", got)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "workbuddy-accounts-2-") ||
		!strings.Contains(cd, ".json") {
		t.Fatalf("Content-Disposition=%q", cd)
	}
}

func TestAccountsExportAcceptsCommaUIDs(t *testing.T) {
	e := newAccountsEnv(t)
	items, _ := e.export(t, "/panel/api/accounts/export?uids=u01,u03")
	if len(items) != 2 || items[0].Account.UID != "u01" || items[1].Account.UID != "u03" {
		t.Fatalf("uids=逗号串没被拆开：%+v", items)
	}
}

func TestAccountsExportWholePoolFollowsFilter(t *testing.T) {
	e := newAccountsEnv(t)
	all, _ := e.export(t, "/panel/api/accounts/export?realm=all")
	if len(all) != 7 {
		t.Fatalf("realm=all 导出 %d 条，want 7", len(all))
	}
	gl, _ := e.export(t, "/panel/api/accounts/export?realm=global")
	if len(gl) != 2 || gl[0].Account.UID != "g01" {
		t.Fatalf("realm=global 导出 %+v", gl)
	}
	grp, _ := e.export(t, "/panel/api/accounts/export?group=A")
	if len(grp) != 2 || grp[0].Account.UID != "u01" || grp[1].Account.UID != "u02" {
		t.Fatalf("group=A 导出 %+v", grp)
	}
}

func TestAccountsExportEmptyIs404(t *testing.T) {
	e := newAccountsEnv(t)
	_, rec := e.export(t, "/panel/api/accounts/export?uid=nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("空结果 code=%d want 404（0 字节的「成功」会让用户以为导出了）", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "没有匹配的账号") {
		t.Fatalf("body=%s", rec.Body)
	}
}
