package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// 改端口不重启：Listen 换 listener 后新地址可用、旧地址下线，进程不重建。
func TestServerManagerHotSwapPort(t *testing.T) {
	m := newServerManager(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	addr1, err := m.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := m.Addr(); got != addr1 {
		t.Fatalf("Addr=%q want %q", got, addr1)
	}
	// 同一地址重复设置必须幂等（面板保存时经常原样提交）。
	if err := m.Listen(addr1); err != nil {
		t.Fatalf("same addr: %v", err)
	}
	if got := m.Addr(); got != addr1 {
		t.Fatalf("同地址设置改掉了监听：%q", got)
	}
	if body := getBody(t, "http://"+addr1); body != "ok" {
		t.Fatalf("body=%q", body)
	}

	// 换端口：先听新的再关旧的，所以失败也不会把正在服务的进程搞下线。
	if err := m.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen new: %v", err)
	}
	addr2 := m.Addr()
	if addr2 == addr1 {
		t.Fatalf("端口没换：%q", addr2)
	}
	if body := getBody(t, "http://"+addr2); body != "ok" {
		t.Fatalf("新地址 body=%q", body)
	}
	if resp, err := newTestClient().Get("http://" + addr1); err == nil {
		resp.Body.Close()
		t.Fatal("旧地址仍在服务")
	}
}

// 读超时热生效：http.Server 恒为 ReadTimeout=0，真实值由 deadlineConn 在「Server 表示
// 不设读超时」那一刻补上，值来自原子变量 —— 所以改配置下一条请求就生效，不用重建 Server。
func TestDeadlineConnInjectsConfiguredReadTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var read atomic.Int64
	read.Store(int64(80 * time.Millisecond))
	dc := &deadlineConn{Conn: client, read: &read}

	// 零值 deadline = net/http 的「这条连接不设读超时」，必须换成本次配置的绝对时间。
	if err := dc.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	start := time.Now()
	if _, err := dc.Read(make([]byte, 1)); err == nil {
		t.Fatal("期望读超时")
	} else {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("err=%v 不是超时错误", err)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("超时来得太晚：%v", d)
	}

	// 配置为 0（不限制）：零值原样透传，读会一直阻塞（这里用 150ms 观察窗证明）。
	read.Store(0)
	if err := dc.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := dc.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("read_timeout=0 时不应超时：%v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := server.Write([]byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("有数据时读失败：%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("读没有返回")
	}
}

// newTestClient 关掉 keep-alive：默认 Transport 会复用上一条空闲连接，而「关掉
// listener」并不会断开已 accept 的连接——那样断言「旧地址下线」永远是假绿。
func newTestClient() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := newTestClient().Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
