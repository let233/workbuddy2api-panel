package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppJSImportQueueAndProgress 导入的「多选」与「实时进展」两件事。
//
// 多选过去只是「一次能勾几个」：onchange 把当前 FileList 当整批，再选一次是
// 替换——选错一个只能全部重来。现在改成队列（追加 + 去重 + 单独移除）。
// 确认导入过去是同步 POST：上百个号要跑十几分钟，界面只有一个禁用的按钮。
// 现在要拿 job_id 轮询进度条。
//
// 变异判据：把 onchange 改回「替换」（去掉 push/去重），或把 watchImportJob 的
// 轮询删掉，本用例都会红。
func TestAppJSImportQueueAndProgress(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; import queue test skipped")
	}
	script := `const fs = require("fs"), vm = require("vm"), assert = require("assert");
const src = fs.readFileSync(process.argv[2], "utf8");
const html = fs.readFileSync(process.argv[3], "utf8");

// ── 1) 页面接线：队列区、进度区、多选、确认按钮 ──
for (const id of ["importFile", "importQueue", "importProg", "importPreview", "btnImportCommit"]) {
  assert.ok(html.includes('id="' + id + '"'), "index.html 缺 #" + id);
}
const fi = html.slice(html.indexOf('id="importFile"'));
assert.ok(fi.slice(0, fi.indexOf(">")).includes("multiple"), "importFile 必须带 multiple");

// ── 2) onchange 是「追加」而不是「替换」 ──
const on = src.indexOf("$('importFile').onchange"), commit = src.indexOf("$('btnImportCommit').onclick");
assert.ok(on >= 0 && commit > on, "找不到 importFile.onchange / btnImportCommit.onclick");
const body = src.slice(on, commit);
assert.ok(body.includes("importQueue.push(f)"), "选择的文件必须追加进队列（否则再选一次就把上次的顶掉了）");
assert.ok(body.includes("importFileKey"), "追加必须去重（同名同大小同 mtime）");
assert.ok(body.includes("$('importFile').value = ''"), "选完必须清空 input.value，否则重选同一文件不触发 change");
assert.ok(body.includes("previewImportQueue()"), "队列变了要重算预览");
assert.ok(!body.includes("postImport(false)"), "选择文件时不得直接写盘");

// ── 3) 确认导入拿 job_id 轮询，而不是干等一个同步响应 ──
assert.ok(src.includes("fd.append('async', '1')"), "确认导入必须走 async=1 的后台任务");
assert.ok(src.includes("function watchImportJob("), "缺 watchImportJob");
assert.ok(src.includes("import/cockpit/jobs/"), "进度轮询路径不对");
assert.ok(body.indexOf("watchImportJob") < 0, "watchImportJob 不该在选择文件时就跑");

// ── 4) 真跑 renderImportQueue：空队列藏起来，有文件逐条列出并可移除 ──
const qs = src.indexOf("function importFileKey("), qe = src.indexOf("async function previewImportQueue(");
assert.ok(qs >= 0 && qe > qs, "队列渲染 block 找不到");
const fakes = {};
const mk = id => (fakes[id] = fakes[id] || { hidden: true, innerHTML: "", addEventListener() {} });
const ctx = {
  importQueue: [],
  $: id => mk(id),
  esc: s => String(s).replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])),
  Number, String, Math,
};
vm.createContext(ctx);
vm.runInContext(src.slice(qs, qe) + "\nthis.renderImportQueue = renderImportQueue; this.importFileKey = importFileKey;", ctx);
ctx.renderImportQueue();
assert.strictEqual(mk("importQueue").hidden, true, "空队列应藏起队列区");
ctx.importQueue = [{ name: "a.json", size: 2048, lastModified: 1 }, { name: "b<c>.json", size: 1024, lastModified: 2 }];
ctx.renderImportQueue();
const box = mk("importQueue");
assert.strictEqual(box.hidden, false, "有文件时队列区必须显示");
assert.ok(box.innerHTML.includes("已选 2 个文件"), "应报出已选文件数: " + box.innerHTML);
assert.ok(box.innerHTML.includes("a.json"), "缺 a.json 这一行");
assert.ok(box.innerHTML.includes("b&lt;c&gt;.json"), "文件名必须转义");
assert.ok(box.innerHTML.includes("2.0 KB"), "文件大小应可读（2.0 KB）: " + box.innerHTML);
assert.strictEqual((box.innerHTML.match(/data-iq="/g) || []).length, 2, "每个文件都要有移除按钮");
assert.strictEqual(ctx.importFileKey({ name: "a", size: 1, lastModified: 2 }), "a:1:2");

// ── 5) renderImportProgress：进度条宽度跟着 processed/total 走，并显示当前账号 ──
const ps = src.indexOf("function renderImportProgress("), pe = src.indexOf("function renderImportDone(");
assert.ok(ps >= 0 && pe > ps, "renderImportProgress 找不到");
const pctx = { $: id => mk(id), esc: s => String(s), Math };
vm.createContext(pctx);
vm.runInContext(src.slice(ps, pe) + "\nthis.renderImportProgress = renderImportProgress;", pctx);
pctx.renderImportProgress({ state: "running", total: 8, processed: 2, creates: 1, overwrites: 1, skipped: 0, current: "甲", elapsed_ms: 1500 }, "2 个文件");
let out = mk("importProg").innerHTML;
assert.ok(out.includes("width:25%"), "进度条宽度应为 25%: " + out);
assert.ok(out.includes("已处理 2/8"), "缺已处理计数: " + out);
assert.ok(out.includes("当前：甲"), "缺当前账号: " + out);
assert.ok(out.includes("用时 1.5s"), "缺用时: " + out);
assert.ok(out.includes("正在导入"), "running 状态文案不对");

// total=0 不能除出 NaN。
pctx.renderImportProgress({ state: "done", total: 0, processed: 0 }, "x");
assert.ok(!mk("importProg").innerHTML.includes("NaN"), "total=0 时进度算出了 NaN");

// ── 6) watchImportJob：拿到 done 就停，连续 3 次取不到才放弃 ──
const ws = src.indexOf("function watchImportJob("), we = src.indexOf("// 进度区：");
assert.ok(ws >= 0 && we > ws, "watchImportJob 找不到");
(async () => {
  const calls = [];
  const wctx = {
    $: id => mk(id),
    esc: s => String(s),
    Math, Promise, setTimeout, Date,
    api: async p => {
      calls.push(p);
      return { state: calls.length < 3 ? "running" : "done", total: 4, processed: calls.length, creates: calls.length };
    },
  };
  vm.createContext(wctx);
  vm.runInContext(src.slice(ps, pe) + src.slice(ws, we) + "\nthis.watchImportJob = watchImportJob;", wctx);
  const done = await wctx.watchImportJob("abc", "f.json");
  assert.strictEqual(done.state, "done", "任务 done 后应返回最终快照");
  assert.strictEqual(calls.length, 3, "轮询次数应为 3（running→running→done）: " + calls.length);
  assert.ok(calls.every(p => p === "import/cockpit/jobs/abc"), "轮询路径不对: " + calls[0]);

  // 任务丢了（面板重启/过期）：3 次失败后必须 resolve，别让用户对着不动的进度条等。
  const mctx = {
    $: id => mk(id),
    esc: s => String(s),
    Math, Promise, setTimeout, Date,
    api: async () => { throw new Error("任务不存在或已过期"); },
  };
  vm.createContext(mctx);
  vm.runInContext(src.slice(ps, pe) + src.slice(ws, we) + "\nthis.watchImportJob = watchImportJob;", mctx);
  const fallback = await mctx.watchImportJob("gone", "f.json");
  assert.strictEqual(fallback.creates, 0, "取不到任务时应返回中性快照");
  assert.ok(mk("importErr").textContent.includes("进度查询中断"), "应提示进度中断: " + mk("importErr").textContent);
})().then(() => console.log("ok"), e => { console.error(e && e.stack || e); process.exit(1); });
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
	scriptPath := filepath.Join(dir, "queue_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("写测试脚本失败: %v", err)
	}
	out, err := exec.Command(node, scriptPath, path, htmlPath).CombinedOutput()
	if err != nil {
		t.Fatalf("导入队列用例失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Fatalf("脚本没跑到结尾: %s", out)
	}
}
