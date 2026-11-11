// testclient_test.go 测试夹具：出站配置改成 runtime 快照后，测试不能再用结构体
// 字面量直接写 UserAgent / ChatBaseCN / GlobalEnabled 这些键（字段已不存在，见
// runtime.go）。这里提供两个一站式帮手，把旧写法原样搬过来：
//
//	rtClient(srv.Client(), nil, Runtime{ChatBaseCN: srv.URL})
//	rtPatch(c, func(rt *Runtime) { rt.GlobalEnabled = true })
//
// 语义与旧代码逐字一致：没写的键就是零值（GlobalEnabled=false、base 为空、
// TLSFingerprint="" → node），所以现存用例的期望值都不用改。
package upstream

import "net/http"

// testClient 构造测试用 Client：hc / chat 是注入的 HTTP client（nil 表示不设，
// chat 为 nil 时 chatHTTP() 回落到 hc，与旧行为一致），rt 一次性写进运行期快照。
//
// 直接写快照而不走 SetRuntime：测试夹具不打真实网络，没必要顺手建一条按快照参数
// 构造的 Transport（那会给每个用例多开一套连接池参数与拨号闭包）。
func rtClient(hc, chat *http.Client, rt Runtime) *Client {
	c := &Client{HTTP: hc, ChatHTTP: chat}
	c.runtime.Store(&rt)
	return c
}

// rtPatch 就地改测试 Client 的运行期配置：等价旧版对明文字段的直接赋值。
// 走 UpdateRuntime 而不是直接写快照，是为了顺带覆盖「改配置会重建 Transport」
// 这条生产路径（否则热改相关断言只能靠人工检查）。
func rtPatch(c *Client, fn func(rt *Runtime)) *Client {
	c.UpdateRuntime(fn)
	return c
}
