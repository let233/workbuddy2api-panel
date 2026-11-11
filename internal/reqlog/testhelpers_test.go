package reqlog

import (
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// testArchive 造一个「只用于具体断言」的归档器：不走 New（不起写循环），只把
// 热改字段的原子值填好，读/清理路径因此可以确定性断言。
//
// 为什么需要它：archiveStore 的 db/enabled/retention/maxBytes 现在都是 atomic
// （运行期可热改，见 SetConfig），结构体字面量不再能直接赋值。
func testArchive(db *store.Store, enabled bool, retention time.Duration, maxBytes int64) *archiveStore {
	w := &archiveStore{}
	if db != nil {
		w.db.Store(db)
	}
	w.enabled.Store(enabled)
	w.retention.Store(int64(retention))
	w.maxBytes.Store(maxBytes)
	return w
}
