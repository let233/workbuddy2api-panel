// reconfigure_test.go 告警配置热改：enabled 门控、webhook/间隔/静默期即时生效。
package alert

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// toggleSrc 健康数在坏/好之间交替：配合 ForTicks=1 / ClearTicks=1，每轮 Tick 都会
// 产生一条事件（alert 与 resolve 交替），便于断言「循环真的在按新间隔跑」。
type toggleSrc struct{ n atomic.Int32 }

func (s *toggleSrc) Health(realm string) Health {
	if realm != "cn" {
		return Health{}
	}
	if s.n.Add(1)%2 == 1 {
		return Health{Total: 1, Healthy: 0}
	}
	return Health{Total: 1, Healthy: 1}
}

func (s *toggleSrc) WAFActive() bool { return false }

// counter 数 webhook 命中次数。
func counter(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(204)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// waitTrue 轮询等待条件成立（测试最多等 within）。
func waitTrue(t *testing.T, what string, within time.Duration, cond func() bool) {
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

// TestReconfigureGatesTickAndNotify enabled=false 时 Tick 与 Notify 都是 no-op；
// 重新打开后 Notify 立刻恢复（面板「关掉告警」必须真的静音）。
func TestReconfigureGatesTickAndNotify(t *testing.T) {
	srv, posts := counter(t)
	m := New(Config{
		Enabled: true, WebhookURL: srv.URL, Interval: time.Hour, Timeout: time.Second,
		StartupGrace: time.Nanosecond, ForTicks: 1, ClearTicks: 1, MinHealthyCN: 1,
	}, fakeSrc{healthy: 0})

	now := m.startedAt.Add(time.Second)
	m.Tick(now)
	if n := posts.Load(); n != 1 {
		t.Fatalf("前置：首轮 Tick 应触发一次告警，posts=%d", n)
	}

	m.Reconfigure(Config{Enabled: false, WebhookURL: srv.URL, Interval: time.Hour, Timeout: time.Second,
		StartupGrace: time.Nanosecond, ForTicks: 1, ClearTicks: 1, MinHealthyCN: 1})
	m.Tick(now.Add(2 * time.Second))
	m.Notify("关掉告警后不该外发")
	time.Sleep(50 * time.Millisecond)
	if n := posts.Load(); n != 1 {
		t.Fatalf("enabled=false 时 Tick/Notify 必须 no-op，posts=%d want 1", n)
	}

	m.Reconfigure(Config{Enabled: true, WebhookURL: srv.URL, Interval: time.Hour, Timeout: time.Second,
		StartupGrace: time.Nanosecond, ForTicks: 1, ClearTicks: 1, MinHealthyCN: 1})
	m.Notify("重新打开")
	waitTrue(t, "重新打开后 Notify 应恢复", 2*time.Second, func() bool { return posts.Load() == 2 })
}

// TestReconfigureWebhookAndIntervalImmediate webhook 与 interval 都是热改：
// Run 里的等待必须立刻按新值重排（旧值是 1 小时，不热改就要等一小时）。
func TestReconfigureWebhookAndIntervalImmediate(t *testing.T) {
	srv1, posts1 := counter(t)
	srv2, posts2 := counter(t)
	src := &toggleSrc{}
	// 静默期 30ms + 间隔 1 小时：静默期一过就该检查一次（间隔 1h 保证「立刻生效」
	// 只能来自热改，而不是等下一次自然轮询）。
	m := New(Config{
		Enabled: true, WebhookURL: srv1.URL, Interval: time.Hour, Timeout: time.Second,
		StartupGrace: 30 * time.Millisecond, ForTicks: 1, ClearTicks: 1, SendResolve: true, MinHealthyCN: 1,
	}, src)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	defer m.Stop()

	waitTrue(t, "启动后应立刻按旧 webhook 报一次", 5*time.Second, func() bool { return posts1.Load() >= 1 })

	// 换成 srv2 + 20ms 间隔：两条都必须在 1 小时内立刻生效（实际应在毫秒级）。
	m.Reconfigure(Config{
		Enabled: true, WebhookURL: srv2.URL, Interval: 20 * time.Millisecond, Timeout: time.Second,
		StartupGrace: time.Nanosecond, ForTicks: 1, ClearTicks: 1, SendResolve: true, MinHealthyCN: 1,
	})
	waitTrue(t, "热改后应打到新 webhook 且按新间隔连发", 5*time.Second, func() bool { return posts2.Load() >= 2 })
}
