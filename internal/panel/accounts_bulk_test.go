package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSBulkActions 多选批量的「作用域」「导出路径」「时间戳」三件事。
//
// 为什么值得一测：批量是写操作（一次能把整批号撤出流量池、或导出整池凭据）。
// 谓词放宽成「全表」、导出路径漏了 realm/group 变成导全池，界面看起来照样正常；
// Go 侧全绿、node --check 也发现不了。这里既跑真实 JS，也钉住 HTML 接线的关键点。
func TestAppJSBulkActions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; bulk action test skipped")
	}
	script := `const fs = require("fs"), vm = require("vm"), assert = require("assert");
const src = fs.readFileSync(process.argv[2], "utf8");
const html = fs.readFileSync(process.argv[3], "utf8");

// ── 1) BULK_ACT 谓词：不该动的账号一个都不许进目标集 ──
const bs = src.indexOf("const BULK_ACT = {"), be = src.indexOf("};", bs) + 2;
assert.ok(bs >= 0 && be > bs, "BULK_ACT not found");
const bctx = {};
vm.createContext(bctx);
vm.runInContext(src.slice(bs, be) + "\nthis.BULK_ACT = BULK_ACT;", bctx);
const B = bctx.BULK_ACT;
assert.strictEqual(B.checkin.ok({}), true);
assert.strictEqual(B.checkin.ok({ enterprise: true }), false, "企业版不该批量签到");
assert.strictEqual(B.checkin.ok({ api_key: true }), false, "静态 Key 不该批量签到");
assert.strictEqual(B.checkin.ok({ disabled: true }), false, "禁用号不该批量签到");
assert.strictEqual(B.pause.ok({ paused: true }), false, "已暂停的号不该再暂停");
assert.strictEqual(B.pause.ok({ disabled: true }), false, "禁用号不该被暂停");
assert.strictEqual(B.resume.ok({ paused: false }), false, "没暂停的号没什么可恢复");
assert.strictEqual(B.disable.ok({ disabled: true }), false, "已禁用的号不该再禁用");
assert.strictEqual(B.revive.ok({ disabled: false }), false, "只对禁用号解冻");
assert.strictEqual(B.revive.ok({ disabled: true }), true);
assert.strictEqual(B.remove.ok({}), true);
assert.strictEqual(B.group.ok({}), true);

// ── 2) accExportPath：带 uid 只导所选；不带则必须带上当前域/分组 ──
const ps = src.indexOf("function accExportPath("), pe = src.indexOf("\n}", ps) + 2;
assert.ok(ps >= 0 && pe > ps, "accExportPath not found");
const pctx = { Array, encodeURIComponent, accRealmFilter: "all", accGroupFilter: "A" };
vm.createContext(pctx);
vm.runInContext(src.slice(ps, pe) + "\nthis.accExportPath = accExportPath;", pctx);
assert.strictEqual(pctx.accExportPath([]), "accounts/export?realm=all&group=A");
assert.strictEqual(pctx.accExportPath(["u1", "u 2"]), "accounts/export?uid=u1&uid=u%202");
pctx.accRealmFilter = "global";
pctx.accGroupFilter = "";
assert.strictEqual(pctx.accExportPath(null), "accounts/export?realm=global&group=",
  "整池导出必须带 realm/group，否则「导出这个分组」会变成导出全池");

// ── 3) accStamp 是本地时间 YYYYMMDD-HHmmss（不能用 toISOString 的 UTC） ──
const ss = src.indexOf("function accStamp("), se = src.indexOf("\n}", ss) + 2;
assert.ok(ss >= 0 && se > ss, "accStamp not found");
const sctx = { Date, String };
vm.createContext(sctx);
vm.runInContext(src.slice(ss, se) + "\nthis.accStamp = accStamp;", sctx);
assert.ok(/^\d{8}-\d{6}$/.test(sctx.accStamp()), "accStamp 形状不对: " + sctx.accStamp());

// ── 4) 接线：勾选框/全选/批量条/回填，缺一个都是「点了没反应」 ──
assert.ok(html.includes('id="accSelAll"'), "表头缺全选框");
assert.ok(html.includes('id="accBulkBar"'), "缺批量操作条");
assert.ok(html.includes('id="btnExportAccounts"'), "缺整池导出按钮");
for (const k of ["checkin", "balance", "pause", "resume", "disable", "revive", "remove", "group", "export", "clear"]) {
  assert.ok(html.includes('data-bulk="' + k + '"'), "批量条缺按钮: " + k);
}
assert.ok(src.includes("function applyAccSel("), "缺 applyAccSel");
assert.ok(/applyAccSel\(\);/.test(src), "loadAccounts 渲染后必须回填勾选态");
assert.ok(src.includes("ev.target.matches('.acc-sel')"), "change 监听没有接住多选框");
assert.ok(src.includes('class="acc-sel" data-sel='), "行模板缺多选格");

// 空态 colspan 必须跟着多一列走，否则空表会少一格、右边出现空洞。
const rs = src.indexOf("function renderAccounts("), re = src.indexOf("// renderModelLocks");
assert.ok(rs >= 0 && re > rs, "renderAccounts block not found");
const rb = src.slice(rs, re);
assert.ok(!rb.includes('colspan="9"'), "renderAccounts 里还有 colspan=9 没改成 10（会少一格）");
const n10 = (rb.match(/colspan="10"/g) || []).length;
assert.ok(n10 >= 4, "空态 colspan=10 只有 " + n10 + " 处，want 4");
console.log("ok");
`
	appJS, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatalf("读 app.js 失败: %v", err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读 index.html 失败: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "app.js")
	if err := os.WriteFile(path, appJS, 0o644); err != nil {
		t.Fatalf("写临时 app.js 失败: %v", err)
	}
	htmlPath := filepath.Join(dir, "index.html")
	if err := os.WriteFile(htmlPath, html, 0o644); err != nil {
		t.Fatalf("写临时 index.html 失败: %v", err)
	}
	scriptPath := filepath.Join(dir, "bulk_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("写测试脚本失败: %v", err)
	}
	out, err := exec.Command(node, scriptPath, path, htmlPath).CombinedOutput()
	if err != nil {
		t.Fatalf("批量操作用例失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("脚本没跑到结尾: %s", out)
	}
}
