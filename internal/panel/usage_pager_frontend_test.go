package panel

// 用量两张表的分页与「按 API Key」维度（node + vm 切片测试，做法同
// usage_overview_frontend_test.go）：默认每页 10 行、页码条文案、越界页码夹回
// 最后一页、切到「按 API Key」后首列表头与空态 colspan 对齐。

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const usPagerScript = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const start = src.indexOf('function fmtTok(n)');
const end = src.indexOf('/* renderUsageChart');
if ([escStart, escEnd, start, end].some(v => v < 0)) throw new Error('usage slices not found');
const els = {};
const $ = id => (els[id] || (els[id] = {
  innerHTML: '', textContent: '', title: '', dataset: {}, _h: {},
  addEventListener(t, f) { this._h[t] = f; }, onchange: null,
}));
const ctx = { Date, Number, String, Math, RegExp, isNaN, parseFloat, parseInt, JSON, Array, Object, $ };
vm.createContext(ctx);
vm.runInContext(
  'let usDim="account", usCreditDim="account", usSort="total", usageData=null;' +
  'function remarkName(u, h){ return h; }' +
  'function fmtHit(){ return "50%"; }' +
  'function fmtMs(n){ return String(n); }' +
  'function fmtRate(n){ return String(n); }' +
  'function fmtCredit(n){ return String(n); }' +
  'function fmtCreditRatio(a){ return String(a); }' +
  'function fmtModelRate(r){ return String(r || "-"); }' +
  'function cacheRateCell(){ return "50%"; }' +
  src.slice(escStart, escEnd) + src.slice(start, end) +
  '\nthis.renderUsageDim=renderUsageDim; this.renderCreditDim=renderCreditDim;' +
  '\nthis.setData=d=>{ usageData=d; }; this.setDim=k=>{ usDim=k; }; this.setCreditDim=k=>{ usCreditDim=k; };' +
  '\nthis.getPages=()=>({dim:usDimPage, credit:usCreditPage});', ctx);
const row = n => ({ key: 'k' + n, extra: 'Key ' + n, requests: 1, errors: 0, prompt_tokens: 10, completion_tokens: 5, total_tokens: 15, cache_hit_tokens: 5, cache_miss_tokens: 5, avg_latency_ms: 1, avg_tokens_per_second: 1 });
const rows = [];
for (let i = 1; i <= 24; i++) rows.push(row(i));
rows.push({ key: '', extra: '', requests: 2, errors: 0, prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 });
const crows = [];
for (let i = 1; i <= 12; i++) crows.push(Object.assign(row(i), { nickname: 'Key ' + i, credits: i, credit_tokens: 100, credit_samples: 1, credits_per_1m_tokens: 1000 }));
const out = {};
try {
  ctx.setData({ by_api_key: rows, credit_by_api_key: crows });
  ctx.setDim('apikey');
  ctx.renderUsageDim();
  out.tabs = els.usDimTabs.innerHTML;
  out.dimRows1 = (els.usDimBody.innerHTML.match(/<tr>/g) || []).length;
  out.dimPager1 = els.usDimPager.innerHTML;
  els.usDimPager._h.click({ target: { closest: () => ({ dataset: { uspage: '2' }, disabled: false }) } });
  out.dimRows2 = (els.usDimBody.innerHTML.match(/<tr>/g) || []).length;
  out.dimPager2 = els.usDimPager.innerHTML;
  out.page2HasK11 = els.usDimBody.innerHTML.indexOf('Key 11') >= 0;
  els.usDimPager._h.click({ target: { closest: () => ({ dataset: { uspage: '99' }, disabled: false }) } });
  out.dimPagerClamped = els.usDimPager.innerHTML;
  ctx.setCreditDim('apikey');
  ctx.renderCreditDim();
  out.creditRows = (els.usCreditBody.innerHTML.match(/<tr>/g) || []).length;
  out.creditPager = els.usCreditPager.innerHTML;
  out.creditHead = els.usCreditHead.innerHTML;
  out.ok = true;
} catch (e) {
  out.ok = false; out.err = String((e && e.stack) || e);
}
console.log(JSON.stringify(out));`

func TestUsagePagerAndAPIKeyDim(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage pager frontend test skipped")
	}
	f, err := os.CreateTemp(t.TempDir(), "us-pager-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(usPagerScript); err != nil {
		t.Fatal(err)
	}
	f.Close()
	raw, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage pager node test failed: %v\n%s", err, raw)
	}
	var out struct {
		Ok            bool   `json:"ok"`
		Err           string `json:"err"`
		Tabs          string `json:"tabs"`
		DimRows1      int    `json:"dimRows1"`
		DimPager1     string `json:"dimPager1"`
		DimRows2      int    `json:"dimRows2"`
		DimPager2     string `json:"dimPager2"`
		Page2HasK11   bool   `json:"page2HasK11"`
		DimPagerClamp string `json:"dimPagerClamped"`
		CreditRows    int    `json:"creditRows"`
		CreditPager   string `json:"creditPager"`
		CreditHead    string `json:"creditHead"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 node 输出失败: %v\n%s", err, raw)
	}
	if !out.Ok {
		t.Fatalf("渲染抛异常: %s", out.Err)
	}
	if !strings.Contains(out.Tabs, "按 API Key") {
		t.Fatalf("用量明细缺少「按 API Key」维度:\n%s", out.Tabs)
	}
	if out.DimRows1 != 10 {
		t.Fatalf("首页应渲染 10 行（25 行数据），实际 %d", out.DimRows1)
	}
	if !strings.Contains(out.DimPager1, "第 1 / 3 页 · 共 25 行") {
		t.Fatalf("页码条文案不对:\n%s", out.DimPager1)
	}
	if !strings.Contains(out.DimPager1, "data-uspage=\"2\"") {
		t.Fatalf("页码条缺少第 2 页按钮:\n%s", out.DimPager1)
	}
	if out.DimRows2 != 10 || !out.Page2HasK11 {
		t.Fatalf("切到第 2 页应渲染第 11~20 行，实际 rows=%d hasKey11=%v", out.DimRows2, out.Page2HasK11)
	}
	if !strings.Contains(out.DimPager2, "第 2 / 3 页") {
		t.Fatalf("第 2 页页码条文案不对:\n%s", out.DimPager2)
	}
	if !strings.Contains(out.DimPagerClamp, "第 3 / 3 页") {
		t.Fatalf("越界页码应夹回最后一页:\n%s", out.DimPagerClamp)
	}
	if out.CreditRows != 10 {
		t.Fatalf("积分扣除历史首页应渲染 10 行（12 行数据），实际 %d", out.CreditRows)
	}
	if !strings.Contains(out.CreditPager, "第 1 / 2 页 · 共 12 行") {
		t.Fatalf("积分扣除历史页码条文案不对:\n%s", out.CreditPager)
	}
	if !strings.Contains(out.CreditHead, "API Key") {
		t.Fatalf("积分扣除历史「按 API Key」表头首列应为 API Key:\n%s", out.CreditHead)
	}
}
