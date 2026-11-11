// dns_test.go 出站 DNS 覆盖（config dns_servers / WB2A_DNS）回归。
//
// 这条接线的价值全在「系统解析器不可用」的环境（Android 无 /etc/resolv.conf、本地
// DNS 被污染）。测试不依赖外网：用本地 TCP 监听冒充 DNS 服务器，验证拨号器确实
// 按配置的服务器建连、并按顺序在失败后换下一台。
package upstream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// 未配置服务器时必须是「系统解析器」原样：Resolver 为 nil，SetDNSDialer 也不动
// Transport——否则一个默认配置的空列表会悄悄改掉所有出站解析行为。
func TestNewDNSDialerWithoutServersKeepsSystemResolver(t *testing.T) {
	d := NewDNSDialer(nil)
	if d == nil || d.Resolver != nil {
		t.Fatalf("空列表应返回系统解析器拨号器，got %+v", d)
	}
	if d.Timeout != dialTimeout || d.KeepAlive != dialKeepAlive {
		t.Fatalf("连接层参数应沿用 newDialer：timeout=%v keepalive=%v", d.Timeout, d.KeepAlive)
	}

	c := New()
	c.SetDNSDialer(NewDNSDialer(nil))
	if c.BaseTransport().DialContext == nil {
		t.Fatal("不应把 DialContext 清成 nil")
	}
}

// 配置了服务器时：PreferGo 必须开（cgo 解析器无视 Resolver.Dial，会继续走系统配置），
// 且首台连不上要自动换下一台，而不是整轮解析失败。
func TestNewDNSDialerAdvancesThroughServers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	// 第一台指向必然拒绝的端口（tcp 拨号会立刻 ECONNREFUSED），第二台是真监听。
	dead := "127.0.0.1:1"
	alive := ln.Addr().String()
	d := NewDNSDialer([]string{dead, alive})
	if d.Resolver == nil || !d.Resolver.PreferGo {
		t.Fatalf("Resolver 必须存在且 PreferGo=true：%+v", d.Resolver)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 只验证「按配置的服务器建连」这一段（Dial 本身），不验证真实查询/应答：
	// 冒充的服务器不回应 DNS 报文，但建连成功即证明走的是配置的解析器。
	conn, err := d.Resolver.Dial(ctx, "tcp", "ignored:53")
	if err != nil {
		t.Fatalf("应在首台失败后落到第二台：%v", err)
	}
	if got := conn.RemoteAddr().String(); got != alive {
		conn.Close()
		t.Fatalf("建连地址=%q want %q（未按顺序尝试配置的服务器）", got, alive)
	}
	conn.Close()
}

// 全部服务器都连不上时返回错误（不是 panic、不是 nil 连接）。
func TestNewDNSDialerAllServersFail(t *testing.T) {
	d := NewDNSDialer([]string{"127.0.0.1:1"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.Resolver.Dial(ctx, "tcp", "ignored:53")
	if err == nil {
		conn.Close()
		t.Fatal("全台失败应返回 error")
	}
}

// SetDNSDialer 必须让两个 client 共用那条可热替换的 Transport，且拨号真的走自定义
// resolver。
//
// 结构说明（热改改造后）：HTTP / ChatHTTP 的 Transport 是同一个 dynTransport 门面
// （指针地址稳定，热改只换它背后的 *http.Transport），真正带 DialContext /
// DialTLSContext 的实例由 c.BaseTransport() 给出 —— 所以断言打在 BaseTransport() 上，
// 而不是 cl.Transport 的类型上。
func TestSetDNSDialerWiresBothClients(t *testing.T) {
	c := New()
	// 假 resolver：任何查询都直接返回哨兵错误。拨号失败即证明「确实问了这个
	// resolver」——不依赖外网，也不用真跑一次 TLS 握手。
	var hits int32
	d := newDialer()
	d.Resolver = &net.Resolver{
		PreferGo: true, // cgo 解析器无视 Resolver.Dial，必须 PreferGo
		Dial: func(context.Context, string, string) (net.Conn, error) {
			atomic.AddInt32(&hits, 1)
			return nil, errors.New("test-resolver-reached")
		},
	}
	c.SetDNSDialer(d)

	for name, cl := range map[string]*http.Client{"HTTP": c.HTTP, "ChatHTTP": c.ChatHTTP} {
		if _, ok := cl.Transport.(dynTransport); !ok {
			t.Fatalf("%s Transport 类型变了：%T", name, cl.Transport)
		}
	}
	tr := c.BaseTransport()
	if tr.DialContext == nil || tr.DialTLSContext == nil {
		t.Fatal("重建后的 Transport 缺 DialContext/DialTLSContext")
	}
	// 连接池参数不能被顺手改掉（挂 DNS 只该动拨号器）。
	if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost {
		t.Fatalf("连接池参数被改动：MaxIdleConnsPerHost=%d want %d", tr.MaxIdleConnsPerHost, maxIdleConnsPerHost)
	}
	// IdleConnTimeout 现在跟随运行时快照（New 的默认 300s），不再是硬编码 30s。
	if tr.IdleConnTimeout != defaultRuntimeIdleTimeout {
		t.Fatalf("IdleConnTimeout=%v want %v", tr.IdleConnTimeout, defaultRuntimeIdleTimeout)
	}
	// 真拨一次（域名必然解析失败）：必须经过上面那个假 resolver。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := tr.DialContext(ctx, "tcp", "dns-probe.invalid:443"); err == nil {
		t.Fatal("不存在的域名居然拨通了")
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("拨号没有经过自定义 resolver（DialContext 没接线）")
	}
}

// nil 拨号器不得panic、不得改动 Transport（配置解析失败路径会传 nil）。
func TestSetDNSDialerIgnoresNil(t *testing.T) {
	c := New()
	c.SetDNSDialer(nil)
	if c.BaseTransport().DialContext == nil {
		t.Fatal("nil 拨号器不应清掉 DialContext")
	}
}
