package pool

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// 这一组用例钉住「导出文件能被导入吃回去」：字段名必须是 cockpit tools 的口径
// （驼峰 accessToken / refreshToken / expiresAt / realm），expiresAt 是 Unix 秒，
// 且 uid 集合与过滤口径要和列表页看到的完全一致——否则「导出再导入」丢号或串域。

func exportPool(t *testing.T) *Pool {
	t.Helper()
	p := New(tempTestDB(t))
	p.Add(&auth.Auth{UID: "u01", Nickname: "昵称-u01", AccessToken: "at-u01", RefreshToken: "rt-u01",
		ExpiresAt: 1796940002, Domain: "www.codebuddy.cn", EnterpriseID: "ent-01"})
	p.Add(&auth.Auth{UID: "u02", Nickname: "昵称-u02", AccessToken: "at-u02", RefreshToken: "rt-u02"})
	p.Add(&auth.Auth{UID: "g01", Nickname: "昵称-g01", AccessToken: "at-g01", RefreshToken: "rt-g01",
		Domain: "api.workbuddy.ai"})
	p.SetGroup("u01", "A")
	p.SetGroup("g01", "B")
	p.SyncPersist()
	return p
}

func TestExportAccountsShape(t *testing.T) {
	p := exportPool(t)
	got := p.ExportAccounts([]string{"u01"}, "", "")
	if len(got) != 1 {
		t.Fatalf("导出 %d 条，want 1", len(got))
	}
	one := got[0]
	if one.Account.UID != "u01" || one.Account.Nickname != "昵称-u01" || one.Account.EnterpriseID != "ent-01" {
		t.Fatalf("account 段 = %+v", one.Account)
	}
	if one.Auth.AccessToken != "at-u01" || one.Auth.RefreshToken != "rt-u01" {
		t.Fatalf("auth 段令牌 = %+v", one.Auth)
	}
	if one.Auth.ExpiresAt != 1796940002 {
		t.Fatalf("expiresAt=%d want 1796940002（Unix 秒，不是毫秒）", one.Auth.ExpiresAt)
	}
	if one.Auth.Domain != "www.codebuddy.cn" || one.Auth.Realm != "cn" {
		t.Fatalf("domain/realm = %q/%q", one.Auth.Domain, one.Auth.Realm)
	}

	// 落盘形状：键名必须与样例文件逐字一致，否则导入侧认不出来。
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, key := range []string{"\"account\"", "\"auth\"", "\"uid\"", "\"nickname\"", "\"enterpriseId\"",
		"\"accessToken\"", "\"refreshToken\"", "\"expiresAt\"", "\"domain\"", "\"realm\""} {
		if !strings.Contains(s, key) {
			t.Fatalf("导出 JSON 缺键 %s：%s", key, s)
		}
	}
	if strings.Contains(s, "\"access_token\"") {
		t.Fatalf("导出用了蛇形键名，导入侧的驼峰分支之外还得靠扁平形兜：%s", s)
	}
}

func TestExportAccountsFilterAndOrder(t *testing.T) {
	p := exportPool(t)

	all := p.ExportAccounts(nil, "", "")
	var uids []string
	for _, it := range all {
		uids = append(uids, it.Account.UID)
	}
	if strings.Join(uids, ",") != "g01,u01,u02" {
		t.Fatalf("整池导出顺序 = %v，want [g01 u01 u02]（uid 升序）", uids)
	}

	if got := p.ExportAccounts(nil, "global", ""); len(got) != 1 || got[0].Account.UID != "g01" {
		t.Fatalf("global 域导出 = %+v，want 只有 g01", got)
	}
	if got := p.ExportAccounts(nil, "cn", ""); len(got) != 2 || got[0].Account.UID != "u01" || got[1].Account.UID != "u02" {
		t.Fatalf("cn 域导出 = %+v，want [u01 u02]", got)
	}
	if got := p.ExportAccounts(nil, "", "A"); len(got) != 1 || got[0].Account.UID != "u01" {
		t.Fatalf("分组 A 导出 = %+v", got)
	}

	// 指定 uid：按传入顺序、去重、未知 uid 静默跳过（前端跨页选择里可能混进刚被删的号）。
	got := p.ExportAccounts([]string{"u02", "u01", "u02", "nope", " "}, "", "")
	if len(got) != 2 || got[0].Account.UID != "u02" || got[1].Account.UID != "u01" {
		t.Fatalf("指定 uid 导出 = %+v，want [u02 u01]", got)
	}

	if empty := p.ExportAccounts([]string{"nope"}, "", ""); len(empty) != 0 {
		t.Fatalf("全未知 uid 应导出 0 条，got %d", len(empty))
	}
}
