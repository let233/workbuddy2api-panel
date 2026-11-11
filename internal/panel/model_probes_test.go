package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// model_probes 端点三态：库里有数据 → 透传 + exists:true + updated_at；
// 纯内存模式 / 无数据 → 空集（面板退化为无标注）。
// 契约字段（claimed/measured/verdict…）由前端消费，网关只透传不解析。
func TestModelProbesEndpoint(t *testing.T) {
	db := storetest.Open(t)
	body := `{"version":1,"probes":{"cn:glm-5.2":{"claimed":131072,"measured":48000,` +
		`"verdict":"clamped","note":"钳制","tested_at":"2026-09-15 18:30:00","source":"probe_max_tokens.py"}}}`

	get := func(cfg Config) *httptest.ResponseRecorder {
		p := New(cfg)
		req := httptest.NewRequest("GET", "/panel/api/model_probes", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	post := func(cfg Config, payload string) *httptest.ResponseRecorder {
		p := New(cfg)
		req := httptest.NewRequest("POST", "/panel/api/model_probes", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 0) 纯内存模式：推送返回 503（数据必须落库），读仍然 200 空集
	if rec := post(Config{Version: "test", APIKey: "test-key"}, body); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("memory-only ingest: code=%d want 503", rec.Code)
	}

	// 1) 推送成功 → 读回：透传 + exists + updated_at
	if rec := post(Config{Version: "test", APIKey: "test-key", DB: db}, body); rec.Code != http.StatusOK {
		t.Fatalf("ingest: code=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := get(Config{Version: "test", APIKey: "test-key", DB: db})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Probes    map[string]json.RawMessage `json:"probes"`
		Exists    bool                       `json:"exists"`
		UpdatedAt string                     `json:"updated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Exists || len(got.Probes) != 1 || got.UpdatedAt == "" {
		t.Fatalf("exists=%v probes=%d updated_at=%q", got.Exists, len(got.Probes), got.UpdatedAt)
	}
	var rec1 struct {
		Verdict  string `json:"verdict"`
		Measured int64  `json:"measured"`
	}
	if err := json.Unmarshal(got.Probes["cn:glm-5.2"], &rec1); err != nil {
		t.Fatal(err)
	}
	if rec1.Verdict != "clamped" || rec1.Measured != 48000 {
		t.Fatalf("probe passthrough = %+v", rec1)
	}

	// 2) 库里没有数据：空集 200（不是错误——无数据 = 无标注）。
	//    注意每个 case 用全新结构体：向已非 nil 的 map 再次 Unmarshal 是合并不是替换。
	empty := storetest.Open(t)
	rec = get(Config{Version: "test", APIKey: "test-key", DB: empty})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty table: code=%d want 200", rec.Code)
	}
	var gotEmpty struct {
		Probes map[string]json.RawMessage `json:"probes"`
		Exists bool                       `json:"exists"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gotEmpty); err != nil {
		t.Fatal(err)
	}
	if gotEmpty.Exists || len(gotEmpty.Probes) != 0 {
		t.Fatalf("empty table: exists=%v probes=%d, want false/0", gotEmpty.Exists, len(gotEmpty.Probes))
	}

	// 3) 未接线（无数据库）：同库里没数据
	rec = get(Config{Version: "test", APIKey: "test-key"})
	if rec.Code != http.StatusOK {
		t.Fatalf("unwired: code=%d want 200", rec.Code)
	}

	// 4) 推送体损坏：400（让工具知道没存进去，而不是静默失败）
	if rec := post(Config{Version: "test", APIKey: "test-key", DB: db}, `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("corrupt push: code=%d want 400", rec.Code)
	}
	// 推送是整表替换：空 probes 清空历史结论
	if rec := post(Config{Version: "test", APIKey: "test-key", DB: db}, `{"version":1,"probes":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("empty push: code=%d", rec.Code)
	}
	if n, err := store.Count[modelProbeRow](db); err != nil || n != 0 {
		t.Fatalf("整表替换后 n=%d err=%v", n, err)
	}
}

// 旧的 data/output_probes.json 首次启动一次性导入（升级路径）。
func TestModelProbesLegacyImport(t *testing.T) {
	dir := t.TempDir()
	body := `{"version":1,"probes":{"global:claude-opus":{"claimed":32000,"measured":32000}}}`
	if err := os.WriteFile(filepath.Join(dir, legacyProbesFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	db := storetest.OpenDir(t, dir)
	store.ImportLegacy(db, dir)
	probes, updated, err := readProbes(db)
	if err != nil || len(probes) != 1 || updated.IsZero() {
		t.Fatalf("probes=%d updated=%v err=%v", len(probes), updated, err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyProbesFile)); err == nil {
		t.Fatal("旧文件应改名归档")
	}
}
