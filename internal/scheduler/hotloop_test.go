// hotloop_test.go 分钟级循环（余额刷新 / 补领清扫 / 窗口探针）的常驻 + 热开关语义。
//
// 设计：循环一律常驻（ctx 结束才停），是否真的跑由原子开关决定，enabled=false 时
// 等 rearm 唤醒（不 busy loop）。这样面板保存配置就能立刻启停，不必重启进程。
package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

func waitUntil(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s，%v）", what, within)
}

// balanceEnv 单账号 + 数 get-user-resource 调用的假上游。
func balanceEnv(t *testing.T) (*Scheduler, *atomic.Int32) {
	t.Helper()
	var resource atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-user-resource") {
			resource.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New(nil)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := testUpClient(upstream.Runtime{ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, srv.Client())
	return New(Config{Pool: p, Upstream: up}), &resource
}

// TestSetBalanceRefreshHotToggles 常驻循环 + 热开关：关着零请求，打开立刻刷，
// 关掉后不再增长（不等重启）。
func TestSetBalanceRefreshHotToggles(t *testing.T) {
	s, calls := balanceEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 启动时关闭（旧入口 interval<=0 不再表示「不启动循环」，只是「先关着」）。
	s.StartBalanceRefresh(ctx, 0)
	time.Sleep(150 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("关闭时不该查余额，calls=%d", n)
	}

	s.SetBalanceRefresh(true, 10*time.Millisecond, 0)
	waitUntil(t, "打开后应开始刷新", 5*time.Second, func() bool { return calls.Load() >= 2 })

	s.SetBalanceRefresh(false, 10*time.Millisecond, 0)
	time.Sleep(100 * time.Millisecond) // 等在途的一轮收尾
	base := calls.Load()
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != base {
		t.Fatalf("关闭后仍在刷新余额：%d → %d", base, got)
	}
	if s.BalanceRefreshEnabled() {
		t.Fatal("BalanceRefreshEnabled() 应为 false")
	}
}

// TestSetTaskSweepHotToggles 补领清扫同款语义。
func TestSetTaskSweepHotToggles(t *testing.T) {
	s := New(Config{})
	var runs atomic.Int32
	s.SetSweepRunner(func() (int, string) {
		runs.Add(1)
		return 0, "hot sweep"
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.StartTaskSweep(ctx, 0) // 先关着
	time.Sleep(150 * time.Millisecond)
	if n := runs.Load(); n != 0 {
		t.Fatalf("关闭时不该清扫，runs=%d", n)
	}

	s.SetTaskSweep(true, 10*time.Millisecond, 0)
	waitUntil(t, "打开后应开始清扫", 5*time.Second, func() bool { return runs.Load() >= 2 })

	s.SetTaskSweep(false, 10*time.Millisecond, 0)
	time.Sleep(100 * time.Millisecond)
	base := runs.Load()
	time.Sleep(200 * time.Millisecond)
	if got := runs.Load(); got != base {
		t.Fatalf("关闭后仍在清扫：%d → %d", base, got)
	}
	if s.TaskSweepEnabled() {
		t.Fatal("TaskSweepEnabled() 应为 false")
	}
}

// TestSetWindowProbeHotToggles 窗口探针：启动时关着（面板打开前）不探测，
// 打开后立刻开始——这是「启动时 enabled=false、之后面板打开」必须能跑通的那条路。
func TestSetWindowProbeHotToggles(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := pool.New(nil)
	p.Add(&auth.Auth{UID: "w1", AccessToken: "at-w1", ExpiresAt: 9999999999})
	up := testUpClient(upstream.Runtime{ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}, srv.Client())
	s := New(Config{Pool: p, Upstream: up})
	// 登记一条 1.5s 后到点的窗口（登记入口只收未来时刻）。
	if !p.ScheduleModelWindow("w1", "glm-5.3", time.Now().Add(1500*time.Millisecond), true, time.Now()) {
		t.Fatal("登记窗口失败")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s.StartModelWindowProbe(ctx) // 老入口 = 启动即启用
	s.SetWindowProbe(false, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if n := probes.Load(); n != 0 {
		t.Fatalf("关闭时不该探测，probes=%d", n)
	}

	s.SetWindowProbe(true, 10*time.Millisecond)
	waitUntil(t, "打开后应在窗口到点时探测", 8*time.Second, func() bool { return probes.Load() >= 1 })
	if !s.WindowProbeEnabled() {
		t.Fatal("WindowProbeEnabled() 应为 true")
	}
}

// TestReconfigureCoversAllScheduleParams Reconfigure 必须覆盖 ScheduleParams 的
// 每一个字段：新增排程字段忘了接进这里，就是「面板改了不生效」的静默 bug。
func TestReconfigureCoversAllScheduleParams(t *testing.T) {
	s := New(Config{})
	s.Reconfigure(ScheduleParams{
		CheckinHours:               []int{6},
		TravelHours:                []int{7},
		ActivityHours:              []int{8},
		KeepaliveHours:             []int{5},
		GrowthHours:                []int{4},
		CheckinDisabled:            true,
		TravelDisabled:             true,
		ActivityDisabled:           true,
		WebActivityDisabled:        true,
		KeepaliveDisabled:          true,
		GrowthDisabled:             true,
		IncludeDisabledInTasks:     true,
		JitterMinutes:              9,
		AccountSpreadMinutes:       11,
		AccountSpreadMinGapSeconds: 3,
	})

	s.schedMu.Lock()
	cfg := s.cfg
	s.schedMu.Unlock()

	check := func(name string, got, want any) {
		t.Helper()
		if got != want {
			// 数组需要逐个比（any 比较对 sclie 不可靠，单独处理见下）。
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	check("JitterMinutes", cfg.JitterMinutes, 9)
	check("AccountSpreadMinutes", cfg.AccountSpreadMinutes, 11)
	check("AccountSpreadMinGapSeconds", cfg.AccountSpreadMinGapSeconds, 3)
	check("IncludeDisabledInTasks", cfg.IncludeDisabledInTasks, true)
	check("CheckinDisabled", cfg.CheckinDisabled, true)
	check("TravelDisabled", cfg.TravelDisabled, true)
	check("ActivityDisabled", cfg.ActivityDisabled, true)
	check("WebActivityDisabled", cfg.WebActivityDisabled, true)
	check("KeepaliveDisabled", cfg.KeepaliveDisabled, true)
	check("GrowthDisabled", cfg.GrowthDisabled, true)

	hours := []struct {
		name string
		got  []int
		want int
	}{
		{"CheckinHours", cfg.CheckinHours, 6},
		{"TravelHours", cfg.TravelHours, 7},
		{"ActivityHours", cfg.ActivityHours, 8},
		{"KeepaliveHours", cfg.KeepaliveHours, 5},
		{"GrowthHours", cfg.GrowthHours, 4},
	}
	for _, h := range hours {
		if len(h.got) != 1 || h.got[0] != h.want {
			t.Errorf("%s=%v want [%d]", h.name, h.got, h.want)
		}
	}
	// 全部禁用后没有可唤醒时点（Run 只等退出/重排信号）。
	if at, kinds := s.nextWake(time.Now()); !at.IsZero() || len(kinds) != 0 {
		t.Errorf("全禁用后 nextWake=%v/%v want 零值", at, kinds)
	}
}
