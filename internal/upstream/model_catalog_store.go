package upstream

import (
	"encoding/json"
	"log"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// capRow 模型上限缓存的一行（表 model_catalog）。
//
// 旧实现在数据目录维护 model.json（用户可手编）；改为一张表后手编入口变成直接
// 改库（或删行让查找链重新拉取），写入口仍是 models.dev 按需拉取。
type capRow struct {
	Model           string `gorm:"column:model;primaryKey;size:191"`
	ContextLength   int64  `gorm:"column:context_length"`
	MaxOutputTokens int64  `gorm:"column:max_output_tokens"`
	FetchedAt       string `gorm:"column:fetched_at;size:64"`
	Source          string `gorm:"column:source;size:32"`
}

func (capRow) TableName() string { return "model_catalog" }

func rowOfCap(model string, e ModelCapEntry) capRow {
	return capRow{
		Model:           model,
		ContextLength:   e.ContextLength,
		MaxOutputTokens: e.MaxOutputTokens,
		FetchedAt:       e.FetchedAt,
		Source:          e.Source,
	}
}

func (row capRow) entry() ModelCapEntry {
	return ModelCapEntry{
		ContextLength:   row.ContextLength,
		MaxOutputTokens: row.MaxOutputTokens,
		FetchedAt:       row.FetchedAt,
		Source:          row.Source,
	}
}

// legacyCatalogFile 旧的缓存文件名（与 state 文件同目录）。
const legacyCatalogFile = "model.json"

func init() {
	store.RegisterModels(&capRow{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "modelcatalog",
		Name: legacyCatalogFile,
		Fn:   importLegacyCatalog,
	})
}

// importLegacyCatalog 把旧的 data/model.json（map[model]ModelCapEntry）导入 model_catalog。
func importLegacyCatalog(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, legacyCatalogFile)
	if !ok {
		return nil
	}
	var file map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &file); err != nil {
		return err
	}
	rows := make([]capRow, 0, len(file))
	for id, e := range file {
		if id == "" || !validCapEntry(e) {
			continue
		}
		rows = append(rows, rowOfCap(id, e))
	}
	if err := store.ReplaceAll(s, rows); err != nil {
		return err
	}
	log.Printf("[upstream] 已从旧文件 %s 导入 %d 条模型上限缓存", legacyCatalogFile, len(rows))
	return nil
}
