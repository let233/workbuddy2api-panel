// baseprovider_test.go 基座热换：上游重建 Transport（TLS 指纹 / DNS / 超时热改）后，
// 代理池按节点缓存的 *http.Transport 必须跟着换基底。
//
// 为什么值得一条回归：节点 Transport 是 base.Clone() 出来的，克隆那一刻的
// DialTLSContext 就是那一刻的指纹档位。上游热改后若不重建，经代理的流量会一直用
// 旧指纹——直连看起来已经切了，经代理的还露着上一个形态，这种半生效最难排查。
package proxypool

import (
	"net/http"
	"testing"
)

func TestSetBaseProviderFollowsBaseChange(t *testing.T) {
	baseA := http.DefaultTransport.(*http.Transport).Clone()
	baseA.MaxIdleConnsPerHost = 3
	baseB := http.DefaultTransport.(*http.Transport).Clone()
	baseB.MaxIdleConnsPerHost = 7

	// 构造期基座是 A；随后注入运行期 provider（生产里是 upstream.BaseTransport）。
	m, err := New(nil, baseA, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	cur := baseA
	m.SetBaseProvider(func() *http.Transport { return cur })

	n := node{ID: "n1", URL: "http://127.0.0.1:1"}
	tr1, err := m.transportLocked(n)
	if err != nil {
		t.Fatal(err)
	}
	if tr1.MaxIdleConnsPerHost != 3 {
		t.Fatalf("首建应克隆当前基底 A：MaxIdleConnsPerHost=%d want 3", tr1.MaxIdleConnsPerHost)
	}

	// 基底没变 → 复用同一条节点 Transport（别每次请求都重建连接池）。
	tr2, err := m.transportLocked(n)
	if err != nil {
		t.Fatal(err)
	}
	if tr2 != tr1 {
		t.Fatal("基底未变却重建了节点 Transport")
	}

	// 模拟上游热改：基底换成 B。
	cur = baseB
	tr3, err := m.transportLocked(n)
	if err != nil {
		t.Fatal(err)
	}
	if tr3 == tr1 {
		t.Fatal("基底换了，节点 Transport 仍是旧实例（经代理的流量会继续用旧指纹）")
	}
	if tr3.MaxIdleConnsPerHost != 7 {
		t.Fatalf("重建后应克隆新基底 B：MaxIdleConnsPerHost=%d want 7", tr3.MaxIdleConnsPerHost)
	}
}

// SetBaseProvider 注入时立即丢弃已缓存的节点 Transport（它们都是按旧基底克隆的）。
func TestSetBaseProviderDropsCachedTransports(t *testing.T) {
	m, err := New(nil, http.DefaultTransport.(*http.Transport), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	n := node{ID: "n1", URL: "http://127.0.0.1:1"}
	tr1, err := m.transportLocked(n)
	if err != nil {
		t.Fatal(err)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	m.SetBaseProvider(func() *http.Transport { return base })
	m.mu.Lock()
	_, cached := m.transports[n.ID]
	m.mu.Unlock()
	if cached {
		t.Fatal("注入 provider 后仍留着旧基底克隆出的节点 Transport")
	}
	tr2, err := m.transportLocked(n)
	if err != nil {
		t.Fatal(err)
	}
	if tr2 == tr1 {
		t.Fatal("应重建出新的节点 Transport")
	}
}
