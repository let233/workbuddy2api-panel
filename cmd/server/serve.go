// serve.go 监听器管理：改端口不重启进程，读超时也是热生效。
//
// 为什么不能「改完 http.Server 字段继续用」：net/http 明确要求 Serve 之后不要再改
// Server 上的字段，运行期写 srv.ReadTimeout 与在途连接的读并发，是实打实的数据竞争。
// 所以这里换两条路：
//   - 读超时：Server 恒为 ReadTimeout=0，改由监听层在「Server 表示这条连接不设
//     读超时」时补一个按原子值算出来的绝对 deadline（见 deadlineConn）；
//   - 端口：换 listener。新端口先听上再关旧 listener，旧连接自然读完退出。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// serverManager 长期持有唯一的 http.Server 与当前 listener。
type serverManager struct {
	handler http.Handler
	read    atomic.Int64 // 单请求读超时（纳秒），0 = 不限

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
}

func newServerManager(h http.Handler) *serverManager {
	return &serverManager{handler: h}
}

// SetReadTimeout 热改单请求读超时（下一条请求起生效）。
func (m *serverManager) SetReadTimeout(d time.Duration) {
	m.read.Store(int64(d))
}

// Start 首次监听并开始服务，返回实际监听地址（addr 带 :0 时能拿到真端口）。
func (m *serverManager) Start(addr string) (string, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.srv = m.newServer()
	m.ln = l
	go m.serveLocked(l)
	return l.Addr().String(), nil
}

// Addr 当前监听地址。
func (m *serverManager) Addr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ln == nil {
		return ""
	}
	return m.ln.Addr().String()
}

// Listen 热切监听地址（改端口/改绑定网卡）。
//
// 先听新地址、再关旧 listener：端口不同时这一步不会失败（不会被自己占），
// 所以「保存一个非法地址」不会把正在服务的进程搞下线。
// 只有「同一端口换绑定网卡」这种情况才会短暂无监听——此时先关旧的再听，
// 失败了就把旧地址听回来，仍然失败才把错误抛给面板。
func (m *serverManager) Listen(addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.srv == nil {
		return errors.New("server 未启动")
	}
	if m.ln != nil && samePort(m.ln.Addr().String(), addr) {
		if sameAddr(m.ln.Addr().String(), addr) {
			return nil // 地址没变，不动
		}
		old := m.ln
		if err := old.Close(); err != nil {
			log.Printf("[serve] 关闭旧监听 %s 失败：%v", old.Addr(), err)
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			// 回滚：把原地址听回来，宁可「新地址没生效」也不要「进程没有监听」
			if back, rerr := net.Listen("tcp", old.Addr().String()); rerr == nil {
				m.ln = back
				go m.serveLocked(back)
				log.Printf("[serve] 切换监听 %s 失败，已回滚到 %s：%v", addr, old.Addr(), err)
			} else {
				log.Printf("[serve] 切换监听 %s 失败且回滚失败：%v / %v", addr, err, rerr)
			}
			return fmt.Errorf("切换监听失败：%w", err)
		}
		m.ln = l
		go m.serveLocked(l)
		log.Printf("[serve] 监听已切换到 %s（无需重启）", l.Addr())
		return nil
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败：%w", addr, err)
	}
	old := m.ln
	m.ln = l
	go m.serveLocked(l)
	if old != nil {
		_ = old.Close() // 旧 listener 停止 accept；在途连接照常读完
		log.Printf("[serve] 监听已从 %s 切换到 %s（无需重启）", old.Addr(), l.Addr())
	}
	return nil
}

func (m *serverManager) newServer() *http.Server {
	return &http.Server{
		Handler:           m.handler,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 恒为 0：真实值由 deadlineConn 按原子值补，
		// 这样热改读超时不需要重建 Server（见文件头注释）。
		ReadTimeout: 0,
		IdleTimeout: 120 * time.Second,
	}
}

func (m *serverManager) serveLocked(l net.Listener) {
	srv := m.srv
	wrapped := &deadlineListener{Listener: l, read: &m.read}
	if err := srv.Serve(wrapped); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// 主动 Close 旧 listener 也会走到这里（"use of closed network connection"）：
		// 这是热切换的正常路径，不当错误喊。
		if !isClosedErr(err) {
			log.Printf("[serve] 监听 %s 退出：%v", l.Addr(), err)
		}
	}
}

// Shutdown 优雅退出，超时取当前配置（shutdown_timeout 热生效）。
func (m *serverManager) Shutdown(timeout time.Duration) error {
	m.mu.Lock()
	srv := m.srv
	m.mu.Unlock()
	if srv == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

func isClosedErr(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne *net.OpError
	return errors.As(err, &ne) && errors.Is(ne.Err, net.ErrClosed)
}

func sameAddr(a, b string) bool {
	ha, pa, err1 := net.SplitHostPort(a)
	hb, pb, err2 := net.SplitHostPort(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return normalizeHost(ha) == normalizeHost(hb) && pa == pb
}

func samePort(a, b string) bool {
	_, pa, err1 := net.SplitHostPort(a)
	_, pb, err2 := net.SplitHostPort(b)
	return err1 == nil && err2 == nil && pa == pb
}

func normalizeHost(h string) string {
	switch h {
	case "", "0.0.0.0", "::", "[::]":
		return "0.0.0.0"
	}
	return h
}

// deadlineListener 给每条连接的读加一个「按当前配置算出来的」超时。
type deadlineListener struct {
	net.Listener
	read *atomic.Int64
}

func (l *deadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &deadlineConn{Conn: c, read: l.read}, nil
}

// deadlineConn 兜住 net/http 那句「这条连接不设读超时」。
//
// net/http 在 ReadTimeout=0 时会显式用零值时间清掉读 deadline（"不设超时"），
// 我们把它换成本次配置对应的绝对时间：等价于 ReadTimeout 的语义（一个请求从
// 开始读到读完最多这么多秒），但值来自原子变量，改配置下一条请求就生效。
// Server 自己设的其它 deadline（ReadHeaderTimeout、IdleTimeout）是非零值，
// 原样透传，不干扰。
type deadlineConn struct {
	net.Conn
	read *atomic.Int64
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		if d := time.Duration(c.read.Load()); d > 0 {
			t = time.Now().Add(d)
		}
	}
	return c.Conn.SetReadDeadline(t)
}

// CloseWrite 透传给底层连接：hijack 出去的连接（若有）仍能半关闭。
func (c *deadlineConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
