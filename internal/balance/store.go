package balance

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// 表：balance_points（余额采样点），主键 (uid, ts)。
// 点数上限 maxPoints（20 万）量级，故落库走「增量 upsert + 组合键删除」，
// 不整表重写（每次全量重写 20 万行代价过高）。
func (Point) TableName() string { return "balance_points" }

// pointUpdateCols 冲突时需要更新的列（主键 (uid, ts) 之外的业务列）。
var pointUpdateCols = []string{"credits", "day", "consumed", "gained", "first_level"}

func init() {
	store.RegisterModels(&Point{})
	store.RegisterLegacy(store.LegacyImport{
		Key:  "balance",
		Name: legacyFileName,
		Fn:   importLegacyBalance,
	})
}

const (
	// legacyFileName 旧的 JSON 存储文件名（与 state 文件同目录）。
	legacyFileName = "balance_history.json"
)

// importLegacyBalance 把旧的 balance_history.json 一次性导入数据库。
func importLegacyBalance(s *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, legacyFileName)
	if !ok {
		return nil
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	if f.Version != 1 {
		return fmt.Errorf("不支持的版本 %d", f.Version)
	}
	pts := make([]Point, 0, len(f.Points))
	for _, p := range f.Points {
		if p.UID == "" || p.TS <= 0 {
			continue
		}
		pts = append(pts, p)
	}
	if err := store.ReplaceAll(s, pts); err != nil {
		return err
	}
	log.Printf("[balance] 已从旧文件 %s 导入 %d 个余额采样点", legacyFileName, len(pts))
	return nil
}
