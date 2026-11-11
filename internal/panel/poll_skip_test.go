package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSPollSkipsUnchangedRender 轮询载荷没变时不重建表。
//
// 面板每 5 秒轮询 overview，此前无条件重建账号表：重建 innerHTML 会打断行内焦点与
// 悬停（29 个号就是每 5 秒抖一次）。借鉴 workbuddy2api-hub 的「轮询指纹跳过」。
//
// 指纹口径已改：账号表走自己的分页端点（见下一个用例），overview 这份载荷里只剩
// 模型锁池需要「变了才重画」。
func TestAppJSPollSkipsUnchangedRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; poll skip test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf("let overviewSig = ''");
const e = src.indexOf("$('accBody').addEventListener", s);
assert.ok(s >= 0 && e > s, 'overviewSig slice anchors not found: s=' + s + ' e=' + e);
assert.ok(src.slice(s, e).includes('async function loadOverview('), 'slice missed loadOverview');

const dom = new Proxy(function () {}, { get: (t, k) => (k === Symbol.toPrimitive ? () => '' : dom), set: () => true, apply: () => dom });
const store = { payload: null };
const calls = { accounts: 0, locks: 0 };
const ctx = {
  $: () => dom, document: dom, window: dom, localStorage: dom, location: dom,
  String, Number, Math, Date, Object, Array, Set, JSON, RegExp, console, encodeURIComponent,
  api: async () => store.payload,
  esc: x => String(x == null ? '' : x),
  setHTML: () => {}, toast: () => {},
  renderUpdateStatus: () => {}, renderExpirySnapshot: () => {},
  renderAccounts: () => { calls.accounts++; },
  renderModelLocks: () => { calls.locks++; },
  startPolling: () => {},
  pollAutoRefresh: true, pollIntervalMs: 5000, updateData: null,
};
vm.createContext(ctx);
vm.runInContext(src.slice(s, e), ctx);

(async () => {
  const base = { total: 1, healthy: 1, cooling: 0, disabled: 0, paused: 0, sticky_sessions: 0,
    in_flight_full: 0, uptime_sec: 10, redis_mode: 'memory', version: 't',
    credits_sum: 5, credits_total_sum: 10, model_locks: [{ model: 'glm-5', state: 'locked' }] };
  store.payload = base;
  await ctx.loadOverview(true);
  await ctx.loadOverview(true);
  assert.strictEqual(calls.locks, 1, '同载荷重复轮询不该重建锁池表，实际 ' + calls.locks + ' 次');
  // 账号表已挪出 overview：轮询多少次都不该碰它（分页端点另有指纹）。
  assert.strictEqual(calls.accounts, 0, 'overview 不该再渲染账号表');
  store.payload = Object.assign({}, base, { model_locks: [{ model: 'glm-5', state: 'starved' }] });
  await ctx.loadOverview(true);
  assert.strictEqual(calls.locks, 2, '载荷变化后必须重渲染');
  console.log(JSON.stringify({ accounts: calls.accounts, locks: calls.locks }));
})().catch(err => { console.error(err); process.exit(1); });`

	f, err := os.CreateTemp(t.TempDir(), "pollskip-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"locks":2`) {
		t.Fatalf("unexpected output: %s", out)
	}
}

// TestAppJSAccountsPollSkipsUnchangedRender 账号表自己的轮询指纹（accSig）：
// 同一页载荷连轮两次只渲染一次，页内行变了（或页码变了）必须再渲染。
//
// 为什么单独一条：账号列表已改成服务端分页，指纹必须覆盖「这一页的行 + 分页元数据」，
// 否则翻页后行数一样、内容不同时会被误判成「没变」而不重画。
func TestAppJSAccountsPollSkipsUnchangedRender(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; accounts poll skip test skipped")
	}
	script := `const fs = require('fs'), vm = require('vm'), assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const s = src.indexOf('function accQuery()');
const e = src.indexOf("if ($('accRealmTabs')) {", s);
assert.ok(s >= 0 && e > s, 'accounts slice anchors not found: s=' + s + ' e=' + e);

const store = { payload: null };
const calls = { render: 0, pager: 0 };
const ctx = {
  String, Number, Math, Object, Array, JSON, console, encodeURIComponent, setTimeout,
  $: () => null,   // 分页条/按钮的接线在切片内，取值一律走 $：给 null 即整体跳过
  api: async () => store.payload,
  toast: () => {},
  renderAccounts: () => { calls.render++; },
  renderAccPager: () => { calls.pager++; },
  // 分页模块态（抽取范围外）：按真实初值注入。
  accPage: 1, accPageSize: 50, accRealmFilter: 'all', accGroupFilter: '',
  accPages: 1, accTotal: 0, accCounts: { all: 0, cn: 0, global: 0 }, accGroups: [], accSig: '',
};
vm.createContext(ctx);
vm.runInContext(src.slice(s, e), ctx);

(async () => {
  const page = (credits) => ({
    accounts: [{ uid: 'u1', credits }], total: 1, page: 1, size: 50, pages: 1,
    counts: { all: 1, cn: 1, global: 0 }, groups: [], source: 'db',
  });
  store.payload = page(5);
  await ctx.loadAccounts(true);
  await ctx.loadAccounts(true);
  assert.strictEqual(calls.render, 1, '同页同载荷不该重建账号表，实际 ' + calls.render + ' 次');
  store.payload = page(6);
  await ctx.loadAccounts(true);
  assert.strictEqual(calls.render, 2, '页内数据变化后必须重渲染');
  // 元数据（分组清单）变化也要重画：分组下拉依赖它。
  store.payload = Object.assign(page(6), { groups: [{ name: 'A', count: 1 }] });
  await ctx.loadAccounts(true);
  assert.strictEqual(calls.render, 3, '分组清单变化后必须重渲染');
  console.log(JSON.stringify({ render: calls.render, pager: calls.pager }));
})().catch(err => { console.error(err); process.exit(1); });`

	f, err := os.CreateTemp(t.TempDir(), "accskip-*.cjs")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatalf("write script: %v", err)
	}
	f.Close()

	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `"render":3`) {
		t.Fatalf("unexpected output: %s", out)
	}
}
