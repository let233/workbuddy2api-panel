package panel

// 分组模板的「编辑 / 删除」曾经点了没反应：行按钮写的是 data-gktpl-id，
// handler 却读 dataset.gkTplId —— dataset 的驼峰化规则是 data-gktpl-id →
// gktplId，大小写差一个字母就取到 undefined，于是 gkEditTpl(undefined) 静默 return。
// 这类错在 Go 侧完全看不见（app.js 不执行、node --check 只查语法），
// 所以这里用 node 跑真实 handler：渲染出真行、按 dataset 名字派发点击。

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayTemplateRowActionsDispatch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; template action dispatch test skipped")
	}
	script := gatewayTemplateDispatchHarness
	f := filepath.Join(t.TempDir(), "gk-tpl-dispatch.cjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, f, "app.js")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行失败: %v\n%s", err, out)
	}
	for _, want := range []string{
		`"editName":"试用 7 天"`,
		`"editMode":"append"`,
		`"editDays":"7"`,
		`"deletePath":"apikey_templates/delete"`,
		`"deleteBody":"gt_1"`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("期望 %s，实际输出：%s", want, out)
		}
	}
}

const gatewayTemplateDispatchHarness = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ── 客户端 API Key（多 Key + 分组模板）');
if (start < 0) throw new Error('gateway key section not found');

// 最小 DOM 桩：按 id 返回固定元素，记录 addEventListener 以便派发点击。
const els = new Map();
function el(id) {
  if (!els.has(id)) els.set(id, {
    id, value: '', textContent: '', innerHTML: '', hidden: false, disabled: false, checked: true,
    listeners: {},
    addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); },
    classList: { add() {}, remove() {} },
  });
  return els.get(id);
}
const html = new Set(['gkTplBody', 'gkTplName', 'gkTplDays', 'gkTplLimit', 'gkTplMode', 'gkTplModels', 'gkTplPrompt', 'gkTplErr', 'gkTplNote', 'btnGkTplSave', 'btnGkTplClear', 'btnGkTplClose', 'gkTplPick']);
const calls = [];
const ctx = {
  esc: s => String(s == null ? '' : s),
  $: id => (html.has(id) ? el(id) : null),
  setHTML: (node, v) => { if (node) node.innerHTML = v; },
  toast: () => {},
  confirm: () => true,
  api: async (path, opt) => { calls.push({ path, opt }); return {}; },
  loadGatewayKeys: async () => {},
  // 模型多选在切片之前定义，这里只关心模板表单有没有被填上。
  mpReset: () => {},
  mpBind: () => {},
  document: { addEventListener() {} },
  setTimeout, clearTimeout, console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, isNaN, parseInt, parseFloat,
};
ctx.window = ctx; ctx.globalThis = ctx;

vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(start) + '\nthis.__gk = { renderGatewayTemplates, gkData };', ctx);

// 模块里的 gkData 是切片内自己声明的，桩对象会被遮蔽：拿到真引用再灌数据。
ctx.__gk.gkData.templates = [{
  id: 'gt_1', name: '试用 7 天', models: ['cn:glm-5.2'], credit_limit: 100,
  expiry_days: 7, prompt_mode: 'append', prompt_text: 'hi',
}];
ctx.__gk.renderGatewayTemplates();
const body = el('gkTplBody');
if (!/data-gktpl-id="gt_1"/.test(body.innerHTML)) throw new Error('模板行没有 data-gktpl-id：' + body.innerHTML);

// dataset 名字必须与 HTML 属性对齐：data-gktpl-id -> dataset.gktplId
const btn = { dataset: { gktplAct: 'edit', gktplId: 'gt_1' }, closest: () => btn };
body.listeners.click[0]({ target: btn });
const editName = el('gkTplName').value;

const delBtn = { dataset: { gktplAct: 'del', gktplId: 'gt_1' }, closest: () => delBtn };
body.listeners.click[0]({ target: delBtn });
const delCall = calls.filter(c => c.path === 'apikey_templates/delete').pop();

process.stdout.write(JSON.stringify({
  editName,
  editMode: el('gkTplMode').value,
  editDays: el('gkTplDays').value,
  deletePath: delCall ? delCall.path : '',
  deleteBody: delCall ? JSON.parse(delCall.opt.body).id : '',
}));
`

// 可用模型改成下拉多选（m01368）。这里钉两件事：一是 HTML 里两个模型输入确定变成了
// 隐藏字段 + 选择器容器（不再是让人自己敲 ID 的 textarea），二是选择/过滤/去重这三段
// 纯函数在 node 下的真实行为——它们出错时页面上只是「勾了没生效」，没有报错可看。
func TestGatewayModelsPickerMultiSelect(t *testing.T) {
	htmlSrc, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(htmlSrc)
	for _, want := range []string{
		`id="gkModelsPick"`,
		`id="gkModelsChips"`,
		`id="gkModelsMenu"`,
		`id="gkTplModelsPick"`,
		`id="gkTplModelsMenu"`,
		`type="hidden" id="gkModels"`,
		`type="hidden" id="gkTplModels"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("index.html 缺少 %s", want)
		}
	}
	for _, banned := range []string{`<textarea id="gkModels"`, `<textarea id="gkTplModels"`} {
		if strings.Contains(page, banned) {
			t.Fatalf("index.html 仍有多行文本框：%s", banned)
		}
	}
	jsSrc, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsSrc)
	for _, want := range []string{
		"function mpSplit(",
		"function mpJoin(",
		"function mpToggle(",
		"function mpAdd(",
		"function mpFilter(",
		"function mpBind(",
		"mpBind('gkModels')",
		"mpBind('gkTplModels')",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js 缺少 %s", want)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; models picker helper test skipped")
	}
	script := gatewayModelsPickerHarness
	f := filepath.Join(t.TempDir(), "gk-models-picker.cjs")
	if err := os.WriteFile(f, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, f, "app.js")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行失败: %v\n%s", err, out)
	}
	for _, want := range []string{
		`"filterEmpty":[{"id":"cn:a","on":true},{"id":"cn:b","on":false}]`,
		`"filterQuery":["cn:b"]`,
		`"toggleOff":[]`,
		`"toggleOn":["cn:a","cn:b"]`,
		`"addDup":["cn:a"]`,
		`"addNew":["x:y"]`,
		`"split":["a","b"]`,
		`"chipsNone":true`,
		`"chipsOne":true`,
		`"menuCustomRow":true`,
		`"menuCustomNote":true`,
		`"menuCustomNoPlainNoMatch":true`,
		`"menuErrNote":true`,
		`"menuErrNoPlainNoMatch":true`,
		`"menuPlainNoNote":true`,
		`"menuQueryNote":true`,
		`"bindSeq":"cn:a|kimi-k3-1"`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("期望 %s，实际输出：%s", want, out)
		}
	}
}

const gatewayModelsPickerHarness = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ── 模型多选');
if (start < 0) throw new Error('models picker section not found');
const end = src.indexOf('/* ── 客户端 API Key');
if (end <= start) throw new Error('models picker section end not found');
// 最小 DOM 桩：切片里只有 mpRenderChips / mpRenderMenu / mpBind 碰 DOM，都只认 name + 后缀这五个 id。
const mkEl = () => ({
  innerHTML: '', value: '', hidden: true, handlers: {},
  classList: { add() {}, remove() {}, toggle() {} },
  addEventListener(t, h) { this.handlers[t] = h; },
  contains() { return true; },
  focus() {},
});
const els = { m: mkEl(), mChips: mkEl(), mQ: mkEl(), mMenu: mkEl(), mPick: mkEl() };
const ctx = {
  String, Array, Number, RegExp, console,
  $: id => els[id] || null,
  document: { addEventListener() {} },
  setHTML: (el, html) => { el.innerHTML = html; },
  esc: s => String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/"/g, '&quot;'),
};
vm.createContext(ctx);
vm.runInContext("'use strict';\n" + src.slice(start, end) +
  '\nthis.mp = { mpSplit, mpJoin, mpToggle, mpAdd, mpFilter, mpRenderMenu, mpRenderChips, mpBind, setCatalog: v => { mpCatalog = v; }, setErr: v => { mpCatalogErr = v; } };', ctx);
const mp = ctx.mp;
const catalog = [{ id: 'cn:a', name: 'A' }, { id: 'cn:b', name: 'B' }];
function menu(value, kw, cat, err) {
  els.m.value = value;
  els.mQ.value = kw;
  els.mMenu.innerHTML = '';
  mp.setCatalog(cat);
  mp.setErr(err || '');
  mp.mpRenderMenu('m');
  return els.mMenu.innerHTML;
}
function chips(value) {
  els.m.value = value;
  els.mChips.innerHTML = '';
  mp.mpRenderChips('m');
  return els.mChips.innerHTML;
}
// 关键字只命中自定义项、目录里一条都不中：这时候 rows 为空但下面还有自定义行。
const customHtml = menu('cn:a\nzz:custom', 'zz', [{ id: 'cn:a', name: 'A' }], '');
const errHtml = menu('', '', [], '上游没有可用账号');
const plainHtml = menu('', '', [{ id: 'cn:a', name: 'A' }], '');
const queryHtml = menu('', 'zzz', [{ id: 'cn:a', name: 'A' }], '');
const has = (h, s) => h.indexOf(s) >= 0;
// 连续勾选两项：change 走的是「只改 chip + 行状态」，不重绘整块菜单（重绘会把用户
// 刚点的复选框换成新节点，菜单滚动位置也会跳回顶部）。这里只验证值链路仍然正确。
mp.mpBind('m');
const rowStub = { classList: { toggle() {} } };
const fire = (id, on) => {
  const cb = { value: id, checked: on };
  cb.closest = sel => (sel === '.mpick-item' ? rowStub : cb);
  els.mMenu.handlers.change({ target: cb });
};
fire('cn:a', true);
fire('kimi-k3-1', true);
const bindSeq = els.m.value.split('\n').join('|');
process.stdout.write(JSON.stringify({
  filterEmpty: mp.mpFilter(catalog, '', ['cn:a']).map(m => ({ id: m.id, on: m.on })),
  filterQuery: mp.mpFilter(catalog, 'B').map(m => m.id),
  toggleOff: mp.mpToggle(['cn:a'], 'cn:a'),
  toggleOn: mp.mpToggle(['cn:a'], 'cn:b'),
  addDup: mp.mpAdd(['cn:a'], 'cn:a'),
  addNew: mp.mpAdd([], 'x:y'),
  split: mp.mpSplit(' a \n\n b '),
  chipsNone: has(chips(''), '全部模型（不限）'),
  chipsOne: has(chips('cn:a'), 'data-mp-del="cn:a"'),
  menuCustomRow: has(customHtml, 'value="zz:custom"'),
  // 目录里没命中但下面要列自定义模型：不能同时说「没有匹配的模型」，自相矛盾。
  menuCustomNote: has(customHtml, '目录里没有匹配项'),
  menuCustomNoPlainNoMatch: !has(customHtml, '没有匹配的模型'),
  // 目录读取失败要说原因，而不是谎报「没有匹配」。
  menuErrNote: has(errHtml, '模型目录读取失败：上游没有可用账号'),
  menuErrNoPlainNoMatch: !has(errHtml, '没有匹配的模型'),
  menuPlainNoNote: !has(plainHtml, 'mpick-note'),
  menuQueryNote: has(queryHtml, '回车把「zzz」'),
  bindSeq,
}));
`
