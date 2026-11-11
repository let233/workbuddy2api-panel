package panelauth

import (
	"encoding/json"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// 表结构沿用仓库里「一行一 JSON」的约定：payload 存完整对象，
// 少数需要排序/清理的列单独成列，避免每次读全表都解一遍 JSON。
type userRow struct {
	ID        string `gorm:"primaryKey;size:64"`
	Payload   string `gorm:"type:text"`
	UpdatedAt time.Time
}

func (userRow) TableName() string { return "panel_users" }

type sessionRow struct {
	TokenHash string `gorm:"primaryKey;size:64"`
	Payload   string `gorm:"type:text"`
	ExpiresAt int64  `gorm:"index"`
	CreatedAt time.Time
}

func (sessionRow) TableName() string { return "panel_sessions" }

func init() {
	store.RegisterModels(&userRow{}, &sessionRow{})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func decodeJSON(s string, v any) bool {
	return json.Unmarshal([]byte(s), v) == nil
}
