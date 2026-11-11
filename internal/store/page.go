package store

import (
	"strings"

	"gorm.io/gorm"
)

// 分页/条件查询：整表读（LoadAll）在账号池上会随规模线性变差——面板列表要
// 「第 N 页」时不该把全表拉进内存再切片（那是前端分页）。这里只提供最小的一层：
// 等值过滤 + 原生条件 + 排序 + LIMIT/OFFSET，计数与取页在同一次调用里完成。

// PageOpts 一页查询的入参。
type PageOpts struct {
	// Filter 等值条件（列名 → 值）。走 map 形式让 gorm 按方言给列名加引号：
	// group 是 SQL 保留字，裸写 "group = ?" 在 MySQL/Postgres 上直接语法错误。
	Filter map[string]any
	// Where 额外的原生条件（可以用 OR），与 Filter 之间是 AND。
	Where string
	Args  []any
	// Order 排序子句（如 "uid ASC"）；空则不排序（顺序由库决定，分页会不稳）。
	Order string
	// Limit 每页条数（<=0 表示不限）；Offset 起始偏移。
	Limit  int
	Offset int
}

// Page 返回一页记录与总数（总数不受 Limit/Offset 影响）。
// s 为 nil（内存模式）时返回空页与 0，调用方负责回落。
func Page[T any](s *Store, opts PageOpts) ([]T, int64, error) {
	var rows []T
	if s == nil || s.db == nil {
		return rows, 0, nil
	}
	var zero T
	scope := func() *gorm.DB {
		q := s.db.Model(&zero)
		if len(opts.Filter) > 0 {
			q = q.Where(opts.Filter)
		}
		if strings.TrimSpace(opts.Where) != "" {
			// 原生条件单独加一层括号：与 Filter 组合时 gorm 会再包一层，OR 不会被
			// 等值条件抢先吞掉。
			q = q.Where("("+opts.Where+")", opts.Args...)
		}
		return q
	}
	var total int64
	if err := scope().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	q := scope()
	if opts.Order != "" {
		q = q.Order(opts.Order)
	}
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}
	if opts.Offset > 0 {
		q = q.Offset(opts.Offset)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}
