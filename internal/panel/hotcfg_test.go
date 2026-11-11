// hotcfg_test.go 面板侧热改 Setter 的最小契约：保存配置后**下一条请求**就按新值走。
//
// 只钉两件最容易回归的事：①运行期改的值确实盖住了装配期快照（而不是被 cfg 反盖）；
// ②关掉某个能力时对外语义正确（关日志 = 停写不停读，别把已经落库的现场丢了）。
package panel

import (
	"os"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// TestSetAuthDirHot 账号目录热改后，新增账号立刻落到新目录（而不是等重启）。
func TestSetAuthDirHot(t *testing.T) {
	pn, _, _ := apiKeyPanel(t, enterpriseUsageOK)
	newDir := t.TempDir()
	pn.SetAuthDir(newDir)
	if got := pn.authDir(); got != newDir {
		t.Fatalf("authDir() = %q want %q", got, newDir)
	}
	if rec := postAPIKey(t, pn, `{"access_token":"ck_hotdir.secret","nickname":"热改目录"}`); rec.Code != 200 {
		t.Fatalf("add_apikey code=%d body=%s", rec.Code, rec.Body)
	}
	entries, err := os.ReadDir(newDir)
	if err != nil {
		t.Fatalf("读新目录: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("热改后的账号目录里没有落盘文件 —— 说明写入点还在用装配期快照")
	}
}

// TestSetPollIntervalAndPackageDetailLimit 两个「只影响展示口径」的整型槽位：
// 设过就盖住 cfg，且 0 也是合法显式值（不能被当成「没设置」）。
func TestSetPollIntervalAndPackageDetailLimit(t *testing.T) {
	pn := New(Config{Version: "test", PollIntervalSec: 30})
	if got := pn.pollIntervalSec(); got != 30 {
		t.Fatalf("未热改时应回退 cfg, got %d", got)
	}
	pn.SetPollIntervalSec(5)
	if got := pn.pollIntervalSec(); got != 5 {
		t.Fatalf("热改后 pollIntervalSec() = %d want 5", got)
	}
	if _, ok := pn.PackageDetailLimit(); ok {
		t.Fatal("没设置过时 ok 应为 false")
	}
	pn.SetPackageDetailLimit(0) // 0 是显式值（不限/未设置），必须能读回来
	if n, ok := pn.PackageDetailLimit(); !ok || n != 0 {
		t.Fatalf("PackageDetailLimit() = %d,%v want 0,true", n, ok)
	}
}

// TestLogStoreConfigureHot 运行期关掉面板日志仓 = 停写不停读（已落库的行还在）。
//
// 反向代价：直接 Close/换空 store 会让页面上的历史行一起消失，事故现场就没了。
func TestLogStoreConfigureHot(t *testing.T) {
	db := storetest.Open(t)
	s := NewLogStore(db, 7, 0)
	defer s.Close()

	s.Append(LogEntry{TS: dayTS("2026-11-08", 10), Ch: ChSys, Text: "before-off"})
	s.Flush()
	if rows := logRows(t, db); len(rows) != 1 {
		t.Fatalf("种子行数 = %d want 1", len(rows))
	}

	s.Configure(7, 0, false)
	if !s.Disabled() {
		t.Fatal("Configure(enabled=false) 后应处于关闭态")
	}
	s.Append(LogEntry{TS: dayTS("2026-11-08", 11), Ch: ChSys, Text: "while-off"})
	s.Flush()
	if rows := logRows(t, db); len(rows) != 1 {
		t.Fatalf("关闭期间不应落库, 行数 = %d want 1", len(rows))
	}

	s.Configure(7, 0, true)
	if s.Disabled() {
		t.Fatal("重新开启后不应仍处于关闭态")
	}
	s.Append(LogEntry{TS: dayTS("2026-11-08", 12), Ch: ChSys, Text: "after-on"})
	s.Flush()
	if rows := logRows(t, db); len(rows) != 2 {
		t.Fatalf("重新开启后应继续落库, 行数 = %d want 2", len(rows))
	}
}
