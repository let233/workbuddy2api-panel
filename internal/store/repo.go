package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 通用整表读写：面板的多数数据集都是「小表、整体重写」形态（池状态、窗口表、
// 补跑标记），整体替换比逐行 diff 简单且不会漏删——与旧的整文件覆写语义一致。

// LoadAll 读取表内全部记录。s 为 nil（内存模式）时返回空。
func LoadAll[T any](s *Store) ([]T, error) {
	var rows []T
	if s == nil || s.db == nil {
		return rows, nil
	}
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// ReplaceAll 在一个事务里用 rows 整体替换整张表（先清空再批量插入）。
// s 为 nil 时静默成功。
func ReplaceAll[T any](s *Store, rows []T) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var zero T
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&zero).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		const batch = 100
		return tx.CreateInBatches(rows, batch).Error
	})
}

// Count 返回表内记录数（面板/测试校验用）。
func Count[T any](s *Store) (int64, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	var zero T
	var n int64
	if err := s.db.Model(&zero).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// Insert 单条/多条插入（日志型数据：只追加，不重写）。
func Insert[T any](s *Store, rows ...T) error {
	if s == nil || s.db == nil || len(rows) == 0 {
		return nil
	}
	return s.db.CreateInBatches(rows, 200).Error
}

// DeleteAll 清空整表，返回删掉的行数。
//
// 语义等于旧的「整个文件重写为空」：删掉的是这张表的全部行。gorm 默认拒绝
// 无条件的 DELETE（防手滑清库），这里显式开 AllowGlobalUpdate，把「清空」的
// 意图写在调用点之外。
func DeleteAll[T any](s *Store) (int64, error) {
	if s == nil {
		return 0, nil
	}
	var zero T
	tx := s.db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&zero)
	if tx.Error != nil {
		return 0, tx.Error
	}
	return tx.RowsAffected, nil
}

// Upsert 批量写入，主键冲突时更新 update 列出的列（增量数据用：只写变更过的行）。
func Upsert[T any](s *Store, rows []T, pk, update []string) error {
	if s == nil || s.db == nil || len(rows) == 0 {
		return nil
	}
	cols := make([]clause.Column, 0, len(pk))
	for _, c := range pk {
		cols = append(cols, clause.Column{Name: c})
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   cols,
		DoUpdates: clause.AssignmentColumns(update),
	}).CreateInBatches(rows, 200).Error
}

// DeleteIn 按某列的 IN 列表批量删除（折叠掉的小时桶、过期的历史行等）。
func DeleteIn[T any](s *Store, column string, values []string) error {
	if s == nil || s.db == nil || len(values) == 0 {
		return nil
	}
	var zero T
	const chunk = 200
	for i := 0; i < len(values); i += chunk {
		j := i + chunk
		if j > len(values) {
			j = len(values)
		}
		if err := s.db.Where(column+" IN ?", values[i:j]).Delete(&zero).Error; err != nil {
			return err
		}
	}
	return nil
}

// DeleteComposite 按多列组合键批量删除行（例如 (uid, ts)）。
// keys 里每个元素的长度必须与 cols 一致；s 为 nil 时静默成功。
func DeleteComposite[T any](s *Store, cols []string, keys [][]any) error {
	if s == nil || s.db == nil || len(keys) == 0 {
		return nil
	}
	if len(cols) == 0 {
		return errors.New("store: DeleteComposite 需要至少一列")
	}
	var zero T
	const chunk = 100
	for i := 0; i < len(keys); i += chunk {
		j := i + chunk
		if j > len(keys) {
			j = len(keys)
		}
		parts := make([]string, 0, j-i)
		args := make([]any, 0, (j-i)*len(cols))
		for _, key := range keys[i:j] {
			if len(key) != len(cols) {
				return fmt.Errorf("store: 组合键列数不匹配（期望 %d，实际 %d）", len(cols), len(key))
			}
			conds := make([]string, 0, len(cols))
			for _, c := range cols {
				conds = append(conds, c+" = ?")
			}
			parts = append(parts, "("+strings.Join(conds, " AND ")+")")
			args = append(args, key...)
		}
		if err := s.db.Where(strings.Join(parts, " OR "), args...).Delete(&zero).Error; err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 键值表：补跑标记、池状态落盘时刻这类零散小状态。
// ---------------------------------------------------------------------------

// KV 通用键值表（app_kv）。
type KV struct {
	Key       string    `gorm:"column:kv_key;primaryKey;size:191" json:"key"`
	Value     string    `gorm:"column:kv_value;type:text" json:"value"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime" json:"updated_at"`
}

// TableName 固定表名（避免 gorm 复数化规则带来的歧义）。
func (KV) TableName() string { return "app_kv" }

func init() { RegisterModels(&KV{}) }

// KVGet 读一个键；不存在返回 ok=false。
func KVGet(s *Store, key string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, nil
	}
	var row KV
	err := s.db.Where("kv_key = ?", key).Take(&row).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return row.Value, true, nil
}

// KVSet 写一个键（存在则覆盖）。
func KVSet(s *Store, key, value string) error {
	if s == nil || s.db == nil {
		return nil
	}
	row := KV{Key: key, Value: value}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "kv_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"kv_value", "updated_at"}),
	}).Create(&row).Error
}

// KVDel 删除一个键。
func KVDel(s *Store, key string) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Where("kv_key = ?", key).Delete(&KV{}).Error
}

// KVList 列出某前缀下的全部键值（按键排序）。
func KVList(s *Store, prefix string) ([]KV, error) {
	var rows []KV
	if s == nil || s.db == nil {
		return rows, nil
	}
	q := s.db.Order("kv_key")
	if prefix != "" {
		q = q.Where("kv_key LIKE ? ESCAPE '\\'", escapeLike(prefix)+"%")
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// KVDelPrefix 删除某前缀下的全部键（补跑标记按天整体刷新时用）。
func KVDelPrefix(s *Store, prefix string) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Where("kv_key LIKE ? ESCAPE '\\'", escapeLike(prefix)+"%").Delete(&KV{}).Error
}

// escapeLike 转义 LIKE 通配符（配 ESCAPE '\' 使用：SQLite 的 LIKE 默认没有转义符，
// 不显式声明时 "\_" 会被当成字面反斜杠，前缀里带 _ 的键（task_run:）就永远匹配不上）。
func escapeLike(v string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(v)
}
