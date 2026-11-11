// upstream_helper_test.go 测试用上游客户端夹具。
//
// 为什么要这个夹具：upstream.Client 的出站配置（域名/UA/设备 token/超时/TLS 指纹）
// 已经不在结构体上留明文导出字段，而是收在 runtime 快照里（见 internal/upstream/runtime.go）——
// 这样面板保存配置才能在运行期整份替换、并发读零锁。测试里原来的
// `&upstream.Client{HTTP: hc, ChatBaseCN: "..."}` 因此不再成立。
//
// 这里刻意**不**用 upstream.New()：New 会把 SanitizeFingerprints 置 true 并填生产
// 缺省，而测试要的是「零值 Client + 明确指定的 base」——行为与旧字面量一致。
package panel

import (
	"net/http"

	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// testUpstream 构造测试用 Client：hc 只覆盖 HTTP（ChatHTTP 留空 → chat 回落到 HTTP，
// 与旧字面量一致）；mod 里改的全是 runtime 快照字段。
func testUpstream(hc *http.Client, mod func(*upstream.Runtime)) *upstream.Client {
	c := &upstream.Client{HTTP: hc}
	var rt upstream.Runtime
	if mod != nil {
		mod(&rt)
	}
	c.SetRuntime(rt)
	return c
}
