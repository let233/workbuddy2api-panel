// runtime_test.go 运行期配置快照（配置全热生效）的回归。
//
// 三条必须钉住的语义：
//  1. UpdateRuntime 改完，读侧**下一个请求**就用上新值（不重启、不重建 process）；
//  2. 影响连接层的项（响应头超时 / TLS 指纹 / dialer）改完必须换一条新 Transport
//     —— 旧实现直接写 *http.Transport 字段，既数据竞争又常常半生效；
//  3. 总时长只加在非流式请求上，聊天 SSE 流永不被总上限砍断。
package upstream

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestRuntimeHotSwapAppliesImmediately 面板保存配置 → 下一发请求即按新值出站。
func TestRuntimeHotSwapAppliesImmediately(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	if got := c.chatBase(a); got != "https://www.workbuddy.cn" {
		t.Fatalf("默认 chat base = %q", got)
	}
	rtPatch(c, func(rt *Runtime) {
		rt.ChatBaseCN = "https://cn.example"
		rt.BillingBaseCN = "https://bill.example"
		rt.WebBaseCN = "https://web.example"
		rt.UserAgent = "HotReload/1.0"
		rt.ClientName = "HotName"
		rt.PassthroughIP = true
	})
	if got := c.chatBase(a); got != "https://cn.example" {
		t.Errorf("热改后 chat base = %q want https://cn.example", got)
	}
	if got := c.billingBase(a); got != "https://bill.example" {
		t.Errorf("热改后 billing base = %q want https://bill.example", got)
	}
	if got := c.webBase(a); got != "https://web.example" {
		t.Errorf("热改后 web base = %q want https://web.example", got)
	}
	if got := c.userAgent(a); got != "HotReload/1.0" {
		t.Errorf("热改后 UA = %q want HotReload/1.0", got)
	}
	// 管理端 IP 透传开关同样是热改项：出站头必须立刻跟着变。
	req, _ := http.NewRequest(http.MethodPost, "https://cn.example/v2/chat/completions", nil)
	c.injectClientIP(req, "203.0.113.9")
	if got := req.Header.Get("X-Forwarded-For"); got != "203.0.113.9" {
		t.Errorf("热改 PassthroughIP 后 X-Forwarded-For = %q", got)
	}

	// 整体替换（SetRuntime）同样是「下一发即生效」，且能一键关回去。
	rt := c.Runtime()
	rt.PassthroughIP = false
	c.SetRuntime(rt)
	req2, _ := http.NewRequest(http.MethodPost, "https://cn.example/v2/chat/completions", nil)
	c.injectClientIP(req2, "203.0.113.9")
	if got := req2.Header.Get("X-Forwarded-For"); got != "" {
		t.Errorf("关掉 PassthroughIP 后仍注入了 IP：%q", got)
	}
}

// TestRuntimeRebuildsTransport 连接层参数热改必须换一条新 Transport（构造期参数，
// 就地改字段既会数据竞争，又对已有连接池不生效）。
func TestRuntimeRebuildsTransport(t *testing.T) {
	c := New()
	tr0 := c.BaseTransport()
	if tr0.ResponseHeaderTimeout != 60*time.Second {
		t.Fatalf("默认 ResponseHeaderTimeout=%v want 60s", tr0.ResponseHeaderTimeout)
	}

	rtPatch(c, func(rt *Runtime) {
		rt.HeaderTimeout = 7 * time.Second
		rt.IdleTimeout = 42 * time.Second
	})
	tr1 := c.BaseTransport()
	if tr1 == tr0 {
		t.Fatal("改 header/idle 超时后 Transport 未重建")
	}
	if tr1.ResponseHeaderTimeout != 7*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 7s", tr1.ResponseHeaderTimeout)
	}
	if tr1.IdleConnTimeout != 42*time.Second {
		t.Errorf("IdleConnTimeout=%v want 42s", tr1.IdleConnTimeout)
	}
	// <=0 的 IdleTimeout 回落构造默认（池里的空闲连接总要有回收期限）。
	rtPatch(c, func(rt *Runtime) { rt.IdleTimeout = 0 })
	if got := c.BaseTransport().IdleConnTimeout; got != idleConnTimeout {
		t.Errorf("IdleTimeout<=0 应回落 %v，实际 %v", idleConnTimeout, got)
	}
	// <=0 的 HeaderTimeout = 不限（不设响应头上限）。
	rtPatch(c, func(rt *Runtime) { rt.HeaderTimeout = 0 })
	if got := c.BaseTransport().ResponseHeaderTimeout; got != 0 {
		t.Errorf("HeaderTimeout<=0 应为 0（不限），实际 %v", got)
	}
}

// TestSetRuntimeKeepsFingerprintOnInvalid 非法指纹不污染当前档位：快照与 Transport
// 都保持原样（一次手滑的配置不该悄悄改掉全部请求的握手形态）。
func TestSetRuntimeKeepsFingerprintOnInvalid(t *testing.T) {
	c := New()
	tr0 := c.BaseTransport()
	rt := c.Runtime()
	rt.TLSFingerprint = "chrome"
	c.SetRuntime(rt)
	if got := c.TLSFingerprint(); got != TLSFingerprintNode {
		t.Errorf("非法指纹把档位改成了 %q want node", got)
	}
	if c.BaseTransport() != tr0 {
		t.Error("非法指纹不该重建 Transport")
	}
}

// TestDynTransportTotalTimeoutOnlyForNonStream 总时长只加在非流式请求上：
// 聊天 SSE 流（ChatHTTP）刻意不带总上限，否则长回答会被拦腰砍断。
func TestDynTransportTotalTimeoutOnlyForNonStream(t *testing.T) {
	const delay = 300 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	c := New()
	rtPatch(c, func(rt *Runtime) { rt.Timeout = 30 * time.Millisecond })
	defer func() { rtPatch(c, func(rt *Runtime) { rt.Timeout = defaultRuntimeTimeout }) }()

	// 非流式：30ms 总上限 < 服务器 300ms 延迟 → 必须失败（且是 ctx 超时）。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
	if resp, err := c.HTTP.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("非流式请求应在总时长上限处失败")
	} else {
		// 具体错误形状随平台而定（ctx deadline / 读写超时），只记录不求形状。
		t.Logf("非流式请求按预期超时：%v", err)
	}

	// 聊天流：同一份快照，但 ChatHTTP 那份 dynTransport 不加总上限 → 正常拿到流。
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
	req2.Header.Set("Accept", "text/event-stream")
	resp2, err := c.ChatHTTP.Do(req2)
	if err != nil {
		t.Fatalf("聊天流被总时长砍断：%v", err)
	}
	resp2.Body.Close()
}

// TestUpdateRuntimeConcurrent 并发读快照 + 并发热改：不 panic、不出撕裂值
// （无 -race 环境时的最小烟雾测试；数据竞争判据靠 atomic.Pointer，见 runtime.go）。
func TestUpdateRuntimeConcurrent(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = c.Runtime()
					_ = c.UserAgent()
					_ = c.chatBase(&auth.Auth{AccessToken: "at"})
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		rtPatch(c, func(rt *Runtime) {
			rt.UserAgent = fmt.Sprintf("UA/%d", i)
			rt.ChatBaseCN = "https://cn.example"
		})
	}
	close(stop)
	wg.Wait()
	if got := c.UserAgent(); got == "" {
		t.Fatal("热改后 UA 为空")
	}
}
