package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
)

// TestPurgeOfflineDeletesLegacySessionDeadReasons 钉住「删除所有已掉线」的分类口径。
//
// 运维报的现象：升级前被判死的号（禁用原因是 "12153 session dead" / "refresh session
// dead"）实际同样打不动，但按钮点下去只删掉了新口径的「已掉线」，那些号留在池里。
// 判据见 pool.IsOfflineReasonText —— 认历史 session dead 文案，但不认风控封禁与手动禁用。
func TestPurgeOfflineDeletesLegacySessionDeadReasons(t *testing.T) {
	p := pool.New(nil)
	dir := t.TempDir()
	fileOf := func(uid string) string {
		f := filepath.Join(dir, uid+".json")
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatalf("写凭证文件失败: %v", err)
		}
		return f
	}
	add := func(uid string) {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, FilePath: fileOf(uid)})
	}

	add("alive")      // 可用号：绝不能被删
	add("new")        // 新口径：401 掉线
	add("old12153")   // 旧口径：连续 3 次 12153 达阈
	add("oldrefresh") // 旧口径：面板刷新判死
	add("banned")     // 风控封禁：可能过几天放行，不能删
	add("manual")     // 面板手动禁用：运维意图，不能删

	p.Disable("new", pool.OfflineReason("签到 401"))
	p.Disable("old12153", "12153 session dead")
	p.Disable("oldrefresh", "refresh session dead")
	p.Disable("banned", "account restricted by upstream (11140 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction")
	p.Disable("manual", pool.ReasonManualDisable)

	pn := New(Config{Version: "test", Pool: p})
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/panel/api/accounts/purge_offline", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("purge code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		OK     bool             `json:"ok"`
		Count  int              `json:"count"`
		UIDs   []string         `json:"uids"`
		Failed []map[string]any `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body)
	}
	sort.Strings(resp.UIDs)
	want := []string{"new", "old12153", "oldrefresh"}
	if resp.Count != len(want) || strings.Join(resp.UIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("删了 %d 个 %v，want %v", resp.Count, resp.UIDs, want)
	}
	if len(resp.Failed) != 0 {
		t.Fatalf("failed=%v want 空", resp.Failed)
	}

	for _, uid := range []string{"alive", "banned", "manual"} {
		if _, ok := p.Status(uid); !ok {
			t.Fatalf("%s 不该被删（风控封禁/手动禁用/可用号都不在清理范围）", uid)
		}
	}
	for _, uid := range want {
		if _, ok := p.Status(uid); ok {
			t.Fatalf("%s 应已出池", uid)
		}
		if _, err := os.Stat(filepath.Join(dir, uid+".json")); !os.IsNotExist(err) {
			t.Fatalf("%s 的凭证文件应被删除（否则重启会按目录把它读回来）", uid)
		}
	}
}
