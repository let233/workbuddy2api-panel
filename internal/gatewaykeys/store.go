package gatewaykeys

import (
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// keyRow 一行一个 Key：配置整体塞进 payload（加字段不用改表结构）。
type keyRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	Payload   string    `gorm:"column:payload;type:text"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 指定表名（避免 gorm 复数化规则带来的迁移歧义）。
func (keyRow) TableName() string { return "gateway_keys" }

// templateRow 一行一个分组模板。
type templateRow struct {
	ID        string    `gorm:"column:id;primaryKey;size:64"`
	Payload   string    `gorm:"column:payload;type:text"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName 指定表名。
func (templateRow) TableName() string { return "gateway_key_templates" }

func init() {
	store.RegisterModels(&keyRow{}, &templateRow{})
}
