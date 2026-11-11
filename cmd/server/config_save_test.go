package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/livecfg"
	"github.com/misakano7545/workbuddy2api-panel/internal/pool"
	"github.com/misakano7545/workbuddy2api-panel/internal/scheduler"
	"github.com/misakano7545/workbuddy2api-panel/internal/session"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/upstream"
)

// saveRig 一套「库 + 配置快照 + 热应用器」，与生产的装配同形（只接本测试用到的组件）。
//
// 用真库而不是 mock：saveConfig 的核心契约是「入库成功才算保存成功」，库这一环是
// 被测对象本身（键名、JSON 形状、派生字段重算都靠它兜）。
type saveRig struct {
	hub  *settingsHub
	ap   *hotApplier
	db   *store.Store
	path string // 引导参数文件路径
	live *livecfg.Holder
	up   *upstream.Client
	p    *pool.Pool
	sch  *scheduler.Scheduler
	sess *session.Router
}

func newSaveRig(t *testing.T) *saveRig {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(store.Config{Driver: "sqlite", DSN: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := Default()
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}

	p := pool.New(nil)
	t.Cleanup(p.Close)
	up := upstream.New()
	up.SetRuntime(upstreamRuntime(cfg))
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})
	live := livecfg.New(liveSnapshotOf(cfg))
	sess := session.New(session.Config{})

	path := filepath.Join(dir, "config.json")
	hub := newSettingsHub(db, path)
	hub.Store(cfg)
	return &saveRig{
		hub:  hub,
		db:   db,
		path: path,
		live: live,
		up:   up,
		p:    p,
		sch:  sch,
		sess: sess,
		ap:   &hotApplier{db: db, up: up, pool: p, sch: sch, live: live, sess: sess},
	}
}

func (r *saveRig) save(t *testing.T, body string) []string {
	t.Helper()
	fields, err := saveConfig([]byte(body), r.hub, r.ap)
	if err != nil {
		t.Fatalf("save %s: %v", body, err)
	}
	return fields
}

// 面板只提交它管理的键，未提交的兄弟键（含用户手写的降级表）必须原样保留。
func TestSaveConfigPreservesAndUpdatesModelFallback(t *testing.T) {
	rig := newSaveRig(t)
	if fields := rig.save(t, `{"pool":{"model_fallback":{"model-a":"model-b"}}}`); len(fields) != 0 {
		t.Fatalf("restart fields=%v", fields)
	}
	for _, tc := range []struct{ body, want string }{
		{`{"upstream":{"user_agent":"changed"}}`, "model-b"},
		{`{"pool":{"model_fallback":{"model-a":"model-c"}}}`, "model-c"},
		{`{"pool":{"model_fallback":null}}`, ""},
	} {
		rig.save(t, tc.body)
		if got := rig.live.Load().ModelFallback["model-a"]; got != tc.want {
			t.Errorf("save %s: fallback = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// TestSaveConfigHotAppliesWithoutRestart 本次改造的核心契约：面板上每一样「以前要重启」
// 的配置，保存后都必须立刻作用于运行中的组件，且不进「需重启」清单。
//
// 逐族抽查：出站身份与超时、global 闸、会话粘性、入站读超时、upstash。
func TestSaveConfigHotAppliesWithoutRestart(t *testing.T) {
	rig := newSaveRig(t)
	fields := rig.save(t, `{
		"global": {"enabled": false},
		"prompt": {"mode": "custom"},
		"upstream": {"user_agent": "new-agent", "timeout_seconds": 45},
		"session_sticky": {"enabled": false},
		"server": {"read_timeout": "120s"},
		"upstash": {"url": "https://example.upstash.io", "token": "sample"}
	}`)
	if len(fields) != 0 {
		t.Fatalf("这些字段现在都是热生效，不该要求重启：%v", fields)
	}
	if got := rig.up.UserAgent(); got != "new-agent" {
		t.Errorf("upstream UA = %q, want new-agent", got)
	}
	if got := rig.up.Timeout(); got != 45*time.Second {
		t.Errorf("upstream timeout = %v, want 45s", got)
	}
	if rig.up.GlobalEnabled() {
		t.Error("global.enabled=false 必须立刻关掉上游侧 global 路由")
	}
	if rig.sess.Enabled() {
		t.Error("session_sticky.enabled=false 必须立刻关掉会话粘性")
	}
	cur := rig.hub.Current()
	if cur.Prompt.Mode != "custom" {
		t.Errorf("prompt.mode = %q, want custom", cur.Prompt.Mode)
	}
	if cur.ServerReadTimeoutDur != 120*time.Second {
		t.Errorf("server.read_timeout = %v, want 120s", cur.ServerReadTimeoutDur)
	}
	if cur.Upstash.URL != "https://example.upstash.io" || cur.Upstash.Token != "sample" {
		t.Errorf("upstash = %+v", cur.Upstash)
	}
	// 入库后重启也不该丢：派生字段（*Dur）不落库，靠 Load 里的 normalize 重算。
	db2 := newSettingsHub(rig.db, rig.path)
	if _, err := db2.Load(Default(), false); err != nil {
		t.Fatal(err)
	}
	if got := db2.Current().ServerReadTimeoutDur; got != 120*time.Second {
		t.Errorf("重启后 server.read_timeout = %v, want 120s（派生字段没重算）", got)
	}
	if got := db2.Current().Upstream.UserAgent; got != "new-agent" {
		t.Errorf("重启后 upstream.user_agent = %q", got)
	}
}

// 监听地址与引导参数：listen 现在靠换 listener 热切，不再进重启清单；只有换库/换
// 数据目录这两件「建连接之前」的事必须重启。
func TestSaveConfigRestartFieldsOnlyBootstrap(t *testing.T) {
	rig := newSaveRig(t)
	if fields := rig.save(t, `{"listen": "127.0.0.1:19999"}`); len(fields) != 0 {
		t.Fatalf("listen 已能热切，不该要求重启：%v", fields)
	}
	fields := rig.save(t, `{"database": {"driver": "sqlite", "dsn": "other.db"}}`)
	if len(fields) != 1 || fields[0] != "database" {
		t.Fatalf("换库必须报 database，得到 %v", fields)
	}
	// 改回原 DSN 即从清单消失（无独立 pending 集合）。
	base := rig.hub.Current().Database
	if fields := rig.save(t, `{"database": {"driver": "`+base.Driver+`", "dsn": "`+base.DSN+`"}}`); len(fields) != 0 {
		t.Fatalf("改回原值后仍在重启清单里: %v", fields)
	}
}

// 引导参数文件写不出去（只读根文件系统 / 挂载点异常）不该让整次保存失败：设置已经
// 入库生效，受影响的只是「下次重启还记不记得端口」。旧实现把写文件当保存的一部分，
// 这类部署形态下面板会一直报错而配置其实早就生效了。
func TestSaveConfigSurvivesUnwritableBootstrapFile(t *testing.T) {
	rig := newSaveRig(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.hub.path = filepath.Join(blocker, "config.json") // 父路径是普通文件 → MkdirAll 必失败

	if _, err := saveConfig([]byte(`{"upstream":{"user_agent":"still-saved"}}`), rig.hub, rig.ap); err != nil {
		t.Fatalf("引导文件不可写不应让保存失败: %v", err)
	}
	if got := rig.up.UserAgent(); got != "still-saved" {
		t.Fatalf("upstream.user_agent 未热应用: %q", got)
	}
	if raw, ok, err := store.KVGet(rig.db, settingsKVKey); err != nil || !ok || raw == "" {
		t.Fatalf("设置没入库: ok=%v err=%v", ok, err)
	}
}

// WB2A_USER_AGENT 托管时：保存提交的值不得抢班（运行值恒为 env 值），且不产生重启项。
func TestSaveConfigRetainsEnvironmentManagedField(t *testing.T) {
	t.Setenv("WB2A_USER_AGENT", "environment-agent")
	rig := newSaveRig(t)
	envCfg := Default()
	envCfg.Upstream.UserAgent = "environment-agent"
	if err := envCfg.normalize(); err != nil {
		t.Fatal(err)
	}
	rig.hub.Store(envCfg)
	rig.ap.Apply(envCfg)

	fields := rig.save(t, `{"upstream":{"user_agent":"submitted-agent"}}`)
	if len(fields) != 0 {
		t.Fatalf("fields=%v", fields)
	}
	if got := rig.up.UserAgent(); got != "environment-agent" {
		t.Fatalf("env 托管的 UA 被提交值覆盖了: %q", got)
	}
	if got := rig.hub.Current().Upstream.UserAgent; got != "environment-agent" {
		t.Fatalf("库里存了提交值: %q", got)
	}
}

// 非法值必须在入库前被拒（面板报错），且不能把「合法但旧的」配置弄坏。
func TestSaveConfigRejectsInvalidValue(t *testing.T) {
	rig := newSaveRig(t)
	before := rig.hub.Current()
	if _, err := saveConfig([]byte(`{"server":{"read_timeout":"nonsense"}}`), rig.hub, rig.ap); err == nil {
		t.Fatal("非法 read_timeout 应报错")
	}
	if got := rig.hub.Current(); got != before {
		t.Fatalf("校验失败后内存快照被改写: %+v", got.Server.ReadTimeout)
	}
	if raw, ok, _ := store.KVGet(rig.db, settingsKVKey); ok && raw != "" {
		t.Fatal("校验失败却入库了")
	}
}

// TestEnvironmentManagedFieldsMatchesValidOverrides 托管清单只收「值合法」的 env：
// 非法值（WB2A_TIMEOUT_SECONDS=invalid）等同未设置，不进清单。
func TestEnvironmentManagedFieldsMatchesValidOverrides(t *testing.T) {
	t.Setenv("WB2A_USER_AGENT", "env-agent")
	t.Setenv("WB2A_SOFT_RATE", "30s")
	t.Setenv("WB2A_TIMEOUT_SECONDS", "invalid")
	fields := Default().EnvironmentManagedFields()
	has := func(want string) bool {
		for _, field := range fields {
			if field == want {
				return true
			}
		}
		return false
	}
	if !has("upstream.user_agent") || !has("cooldown.soft_rate") || has("upstream.timeout_seconds") {
		t.Fatalf("managed=%v", fields)
	}
}

// restartRequiredFields 只报引导参数：装配期的其余字段（身份/双域/提示词/粘性/
// upstash/读超时）改完即生效，报出来只会让用户以为白改了。
func TestRestartRequiredOnlyBootstrapFields(t *testing.T) {
	current, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := ParseConfig([]byte(`{"global":{"enabled":false},"prompt":{"mode":"custom"},"upstream":{"user_agent":"new-agent"},"session_sticky":{"enabled":false},"server":{"read_timeout":"120s"},"upstash":{"url":"https://example.upstash.io","token":"sample"},"listen":":9999","auth_dir":"/tmp/accounts"}`))
	if err != nil {
		t.Fatal(err)
	}
	if fields := restartRequiredFields(current, next); len(fields) != 0 {
		t.Fatalf("这些字段都是热生效，不该进重启清单：%v", fields)
	}

	next.StateFile = "/tmp/other-state.db"
	if fields := restartRequiredFields(current, next); len(fields) != 1 || fields[0] != "state_file" {
		t.Fatalf("fields=%v", fields)
	}
}

// 保存结果必须能原样被 Config 解析回来（面板回显走同一份 JSON）。
func TestSavedSettingsRoundTripJSON(t *testing.T) {
	rig := newSaveRig(t)
	rig.save(t, `{"prompt":{"mode":"passthrough"},"pool":{"max_in_flight":7}}`)
	raw, ok, err := store.KVGet(rig.db, settingsKVKey)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	var back Config
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("库里的设置解析不回 Config: %v", err)
	}
	if back.Pool.MaxInFlight != 7 {
		t.Fatalf("max_in_flight=%d", back.Pool.MaxInFlight)
	}
}

// TestPromptTextPersistsThroughSettingsDB 后台提示词正文的完整持久化链路：
// 面板提交 prompt.text → 原文进库 → 重启后 ParseConfig 解析出同一段正文。
//
// 钉住的是「提示词只是后台设置」这条契约：库里只有正文文本，没有 file 键，
// 部署侧不存在任何 .md 文件可丢，也就不存在「保存成功、重启后回落到内置默认」的错位。
func TestPromptTextPersistsThroughSettingsDB(t *testing.T) {
	rig := newSaveRig(t)
	text := "第一行\n\n- 规则一\n- 规则二"
	body, err := json.Marshal(map[string]any{
		"prompt": map[string]any{"mode": "custom", "text": text},
	})
	if err != nil {
		t.Fatal(err)
	}
	rig.save(t, string(body))

	raw, ok, err := store.KVGet(rig.db, settingsKVKey)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// 库里存原文，且 prompt 段不该再有遗留的 file 键（旧配置的路径值就此失联，
	// 用户需要把正文贴进面板一次——这是「不再用文件路径」的必然代价）。
	var probe struct {
		Prompt map[string]any `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		t.Fatalf("库里的设置解析不回来: %v", err)
	}
	if _, hasFile := probe.Prompt["file"]; hasFile {
		t.Fatalf("库里仍留着 prompt.file: %s", raw)
	}
	if probe.Prompt["text"] != text {
		t.Fatalf("库里的 prompt.text=%q want %q", probe.Prompt["text"], text)
	}

	back, err := ParseConfig([]byte(raw))
	if err != nil {
		t.Fatalf("重启后解析库里的设置: %v", err)
	}
	if back.Prompt.Mode != "custom" || back.PromptText != text {
		t.Fatalf("mode=%q PromptText=%q want custom / %q", back.Prompt.Mode, back.PromptText, text)
	}
}
