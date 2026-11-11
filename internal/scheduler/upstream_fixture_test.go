package scheduler

import (
	"net/http"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// testUpClient 测试夹具：把 base/开关填进上游的 Runtime 快照。
//
// 为什么需要它：internal/upstream 把 ChatBaseCN / BillingBaseCN / WebBaseCN /
// GlobalEnabled 等装配期明文字段收进了 Runtime 值快照（运行期可整份热换，见
// internal/upstream/runtime.go），Client 的字段字面量里不再有这些键。测试要做的
// 仍然只是「把 base 指到 httptest 服务器」，这里给出一条最短路径。
func testUpClient(rt upstream.Runtime, httpClient *http.Client) *upstream.Client {
	c := &upstream.Client{HTTP: httpClient}
	c.SetRuntime(rt)
	return c
}
