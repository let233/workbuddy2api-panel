package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestTaskFrontendCapabilitiesAndResults(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf('function checkinResultNote('),end=src.indexOf('// 可自动完成的任务');
assert(start>=0&&end>start);
const ctx={AUTO_TASKS:{chat_5:'chat'},GROWTH_TITLES:{}};
vm.createContext(ctx);vm.runInContext(src.slice(start,end),ctx);
const global=ctx.taskRowPresentation({task_code:'first_chat',code:'first_chat',task_id:1,status:'available',progress_known:false,reward_known:false},{accept:false,claim:false,automate:false});
assert.equal(global.code,'first_chat');assert.equal(global.progress,'未提供');assert.equal(global.reward,'未提供');assert.equal(global.action,'');
const cn=ctx.taskRowPresentation({task_code:'chat_5',current:0,target:5,accept_status:'accepted'},{accept:true,claim:true,automate:true});
assert.equal(cn.progress,'0 / 5');assert.equal(cn.action,'auto');
assert.equal(ctx.taskRowPresentation({task_code:'',target:5},{accept:true,claim:true,automate:true}).action,'');
assert.equal(ctx.checkinResultNote({checkin_done:false,checkin_error:'活动未开启',credits:350}).severity,'err');
assert.notEqual(ctx.checkinResultNote({skipped:true,skip_reason:'国际区不适用'}).severity,'ok');
assert.equal(ctx.checkinResultNote({checkin_done:true,credits:350}).severity,'ok');
const summary=ctx.taskResultSummary([{status:'done'},{status:'awaiting_progress'},{status:'done',claim_error:'denied'},{status:'skipped'},{status:'error'}]);
assert(summary.message.includes('已完成 1 项'));assert(summary.message.includes('等待计分 1 项'));assert(summary.message.includes('领奖待重试 1 项'));assert.equal(summary.severity,'err');
const gs=src.indexOf('function groupItems(d)'),ge=src.indexOf('const ST_WORDS');
vm.runInContext(src.slice(gs,ge),ctx);
const groups=ctx.groupItems({accounts:[{uid:'global',skipped:true,skip_reason:'国际区只读'},{uid:'bad',growth_error:'upstream unavailable'}]});
assert.equal(groups.length,2);assert.equal(groups[0].rows[0].status,'skipped');assert.equal(groups[1].rows[0].status,'error');
assert(!src.includes('全部账号的成长任务与开学季活动都已完成'));
console.log('task frontend regression checks passed');`
	path := filepath.Join(t.TempDir(), "task-ui.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestConfigSavePostsCollectedBody 钉住配置保存的路径：提交 → api('config', POST, JSON(collectConfig()))
// → 重新拉取配置 + 刷新概览 + 回显环境托管字段。
//
// 同时钉住反面：保存配置不再碰任何「本机密钥」——凭据只在 HttpOnly 会话 Cookie 里，
// 前端既读不到也写不了（旧实现会把 api_key 存进 localStorage，等于给 XSS 留了一份副本）。
func TestConfigSavePostsCollectedBody(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf("$('cfgForm').onsubmit ="),end=src.indexOf('/* ── 添加账号');
assert(start>=0&&end>start);
(async()=>{
  const hosts={cfgForm:{},btnCfgSave:{disabled:false,textContent:''},cfgNote:{textContent:''}};
  let posted=null,loaded=0,overview=0,keyWrites=0;
  const ctx={
    $:id=>hosts[id],
    DURATION_FIELDS:[],durationBad:()=>false,markDurationFields(){},
    collectConfig:()=>({upstream:{user_agent:'x'}}),
    FALLBACK_FIELDS:[],fallbackBad:()=>false,FALLBACK_TIP:'',
    api:async(path,opts)=>{
      assert.equal(path,'config');assert.equal(opts.method,'POST');
      posted=JSON.parse(opts.body);
      return {restart_required:[],environment_managed_fields:['upstream.user_agent']};
    },
    // 凭据在 HttpOnly Cookie 里：保存配置既不该写本地密钥，也不该读它。
    localStorage:{setItem(){keyWrites++},removeItem(){keyWrites++},getItem(){keyWrites++;return null}},
    toast(){},loadOverview(){overview++},loadConfig:async()=>{loaded++},JSON};
  vm.createContext(ctx);vm.runInContext(src.slice(start,end),ctx);
  assert(ctx.$('cfgForm').onsubmit,'cfgForm 未绑定 onsubmit');
  await ctx.$('cfgForm').onsubmit({preventDefault(){}});
  assert.deepEqual(posted,{upstream:{user_agent:'x'}},'应提交 collectConfig 的整份结果');
  assert.equal(loaded,1,'保存后应重新拉取配置');
  assert.equal(overview,1,'保存后应刷新概览');
  assert.equal(keyWrites,0,'保存配置不应触碰 localStoarge 里的凭据');
  assert(hosts.cfgNote.textContent.includes('upstream.user_agent'),'应回显环境托管字段');
  assert.equal(hosts.btnCfgSave.disabled,false,'保存完按钮要恢复');
  console.log('config save regression passed');
})().catch(e=>{console.error(e);process.exit(1)});`
	path := filepath.Join(t.TempDir(), "config-ui.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestAutoActionsSyncWithFrontend 钉住「后端 autoActions」与「前端 AUTO_TASKS」的键集合一致。
//
// 为什么要这条：任务中心是否把某任务渲染成「一键完成」，由前端 AUTO_TASKS[code] 决定
// （app.js 的 taskRowPresentation），而点了之后能不能跑由后端 autoActions 决定。
// 只在一边登记就是静默故障，两边都看不出来：
//   - 只在后端：按钮不出现，任务看着"不支持自动化"；
//   - 只在前端：按钮出现了，点了后端返回无动作。
//
// 本测试纯静态比对：不打上游、不碰账号、不依赖 node。
func TestAutoActionsSyncWithFrontend(t *testing.T) {
	// 后端：autoActions 表的真实任务码。("(批量接受)" 之类是进度事件标签，不在表里)
	back := map[string]bool{}
	for _, a := range autoActions {
		back[a.TaskCode] = true
	}
	if len(back) == 0 {
		t.Fatal("autoActions 为空，测试失去意义")
	}

	// 前端：从 app.js 切出 `const AUTO_TASKS = { ... };` 块，抠出 '键': 形式的键名。
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	const head = "const AUTO_TASKS = {"
	i := strings.Index(text, head)
	if i < 0 {
		t.Fatalf("app.js 里找不到 %q（表格被改名或移走了？）", head)
	}
	body := text[i+len(head):]
	// 必须找到闭合的 "};" 才切；找不到就立刻失败——否则 body 会退化成"表头到文件末尾"
	// 的全部代码，正则可能抓到后面其他表/对象里的 'key': 造成假报错。
	j := strings.Index(body, "\n};")
	if j < 0 {
		t.Fatal("AUTO_TASKS 块没有正常的 }; 结尾，无法定位表格边界（格式被改动？）")
	}
	body = body[:j]
	keyRE := regexp.MustCompile(`(?m)^\s*'([^']+)':`)
	front := map[string]bool{}
	for _, m := range keyRE.FindAllStringSubmatch(body, -1) {
		front[m[1]] = true
	}
	// 防假绿：正则一旦失效（比如表格改用双引号、或一键写成一行），提取结果会变成空集，
	// 那时"两边都没差异"是假象。用下限兜住。
	if len(front) < 20 {
		t.Fatalf("AUTO_TASKS 只解析出 %d 个键（正则失效或表格换写法了？）", len(front))
	}

	var missing, extra []string
	for code := range back {
		if !front[code] {
			missing = append(missing, code)
		}
	}
	for code := range front {
		if !back[code] {
			extra = append(extra, code)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("后端 autoActions 有、前端 AUTO_TASKS 缺 %v —— 任务中心不会渲染「一键完成」按钮", missing)
	}
	if len(extra) > 0 {
		t.Errorf("前端 AUTO_TASKS 有、后端 autoActions 缺 %v —— 按钮能点但后端会返回无动作", extra)
	}
}
