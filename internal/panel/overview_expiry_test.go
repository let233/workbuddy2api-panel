package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// TestExpiryEndpointIncludesBuckets 到期看板的数据源接线：/panel/api/expiry 必须把池内
// 按日到期批次原样透出，并给出余额后台刷新的节奏/年龄（没有调度器时为 0）。
// 前端 renderExpirySnapshot 只吃这两个字段，缺一个就会退回「只列最早一批」的旧口径。
//
// 为什么从 /overview 搬到独立端点：账号列表改成数据库分页后，overview 不再下发整池
// 账号（只给统计与合计），而到期看板恰恰要看**整池**——两者数据源必须分开。
func TestExpiryEndpointIncludesBuckets(t *testing.T) {
	p := pool.New(nil)
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at"})
	at := time.Now().Add(72 * time.Hour)
	p.SetCreditsSnapshot("u1", pool.CreditsSnapshot{
		Remain: 100, Total: 100,
		ExpiryBuckets: []pool.ExpiryBucket{
			{Date: at.Format("2006-01-02"), ExpireAt: at, Remain: 40},
		},
	})
	pn := New(Config{Version: "test", Pool: p})

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/expiry", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expiry code=%d body=%s", rec.Code, rec.Body)
	}
	var d struct {
		BalanceRefreshSec  int       `json:"balance_refresh_sec"`
		BalanceRefreshedAt time.Time `json:"balance_refreshed_at"`
		Accounts           []struct {
			UID                  string              `json:"uid"`
			CreditsExpiryBuckets []pool.ExpiryBucket `json:"credits_expiry_buckets"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Accounts) != 1 || len(d.Accounts[0].CreditsExpiryBuckets) != 1 {
		t.Fatalf("expiry 未透出 buckets: %s", rec.Body)
	}
	b := d.Accounts[0].CreditsExpiryBuckets[0]
	if b.Date != at.Format("2006-01-02") || b.Remain != 40 {
		t.Fatalf("bucket=%+v want %s/40", b, at.Format("2006-01-02"))
	}
	if d.BalanceRefreshSec != 0 {
		t.Fatalf("无调度器时 balance_refresh_sec=%d want 0", d.BalanceRefreshSec)
	}
}

// TestOverviewDropsAccountList overview 不得再夹带整池账号：它现在是分页端点
// （/panel/api/accounts）的职责，两边都下发会让前端出现两份互相打架的账号快照
// （统计卡算全池、表格只有一页）。这条断言就是防止有人「顺手加回来」。
func TestOverviewDropsAccountList(t *testing.T) {
	p := pool.New(nil)
	p.Add(&auth.Auth{UID: "u1", Nickname: "A", AccessToken: "at"})
	p.SetCreditsSnapshot("u1", pool.CreditsSnapshot{Remain: 30, Total: 100})
	pn := New(Config{Version: "test", Pool: p})

	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview code=%d body=%s", rec.Code, rec.Body)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"accounts"`)) {
		t.Fatalf("overview 不应再下发 accounts（改走分页端点）: %s", rec.Body)
	}
	var d struct {
		Total         int   `json:"total"`
		CreditsSum    int64 `json:"credits_sum"`
		CreditsTotSum int64 `json:"credits_total_sum"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Total != 1 || d.CreditsSum != 30 || d.CreditsTotSum != 100 {
		t.Fatalf("overview 汇总=%+v want total=1 credits_sum=30 credits_total_sum=100", d)
	}
}
