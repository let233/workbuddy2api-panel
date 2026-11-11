package panel

import (
	"encoding/json"
	"log"
	"sort"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// modelProbeRow 模型输出上限探测结果的一行（表 model_probes）。
//
// Payload 原样保存 scripts/probe_max_tokens.py 产出的契约对象（claimed/measured/
// verdict/note/tested_at/source…）：网关只透传不解析，上游改了限制后重跑工具、
// 重新推一次即可刷新，无需重启网关。
type modelProbeRow struct {
	ModelKey  string    `gorm:"column:model_key;primaryKey;size:191"`
	Payload   string    `gorm:"column:payload;type:text"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (modelProbeRow) TableName() string { return "model_probes" }

// legacyProbesFile 旧的契约文件名（scripts/probe_max_tokens.py --panel-out 写入）。
const legacyProbesFile = "output_probes.json"

// probeContract 探测结果契约（文件与推送接口共用）。
type probeContract struct {
	Version int                        `json:"version"`
	Probes  map[string]json.RawMessage `json:"probes"`
}

func init() {
	store.RegisterModels(&modelProbeRow{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "modelprobes",
		Name: legacyProbesFile,
		Fn:   importLegacyProbes,
	})
}

// importLegacyProbes 把旧的 output_probes.json 一次性导入 model_probes。
func importLegacyProbes(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, legacyProbesFile)
	if !ok {
		return nil
	}
	var f probeContract
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	rows := probeRows(f.Probes, time.Now())
	if err := store.ReplaceAll(s, rows); err != nil {
		return err
	}
	log.Printf("[panel] 已从旧文件 %s 导入 %d 条模型上限探测结果", legacyProbesFile, len(rows))
	return nil
}

// probeRows 把契约里的 probes 摊平成行（键名排序，便于测试与肉眼对比）。
func probeRows(probes map[string]json.RawMessage, now time.Time) []modelProbeRow {
	keys := make([]string, 0, len(probes))
	for k := range probes {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	rows := make([]modelProbeRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, modelProbeRow{ModelKey: k, Payload: string(probes[k]), UpdatedAt: now})
	}
	return rows
}

// readProbes 读取全部探测结果，并返回最近更新时间（无数据时为零值）。
func readProbes(db *store.Store) (map[string]json.RawMessage, time.Time, error) {
	out := map[string]json.RawMessage{}
	if db == nil {
		return out, time.Time{}, nil
	}
	rows, err := store.LoadAll[modelProbeRow](db)
	if err != nil {
		return nil, time.Time{}, err
	}
	var latest time.Time
	for _, row := range rows {
		if row.ModelKey == "" {
			continue
		}
		out[row.ModelKey] = json.RawMessage(row.Payload)
		if row.UpdatedAt.After(latest) {
			latest = row.UpdatedAt
		}
	}
	return out, latest, nil
}
