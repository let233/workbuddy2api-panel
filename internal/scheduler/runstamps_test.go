package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// 同一天重启多次只补跑一次：第一次启动补了哪几类，重启后就不该再补哪几类。
func TestStartupCatchUpRunsOncePerDay(t *testing.T) {
	db := storetest.Open(t)
	cfg := Config{CheckinHours: []int{9}, DB: db}
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, cst.Loc) // 签到的 9 点已过

	first := New(cfg)
	missed := first.missedToday(now)
	if !hasKind(missed, taskCheckin) {
		t.Fatalf("首轮应补签到，got %v", missed)
	}
	first.markRan(missed, now)

	// 模拟重启：新实例、同一个库。
	if got := New(cfg).missedToday(now); len(got) != 0 {
		t.Fatalf("重启后不该再补，got %v", got)
	}
	// 第二天照旧补。
	if got := New(cfg).missedToday(now.AddDate(0, 0, 1)); !hasKind(got, taskCheckin) {
		t.Fatalf("次日应重新补，got %v", got)
	}
}

// 未注入存储时不落库、行为与旧实现一致（每次启动都补）。
func TestStartupCatchUpWithoutStampFile(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}})
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, cst.Loc)
	s.markRan([]taskKind{taskCheckin}, now) // 无库 = 不写，不能 panic
	if got := s.missedToday(now); !hasKind(got, taskCheckin) {
		t.Fatalf("got %v", got)
	}
}

// 旧文件损坏：导入失败只跳过，宁可多补一轮，也不能因此不跑。
func TestStartupCatchUpToleratesCorruptStamp(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task-runs.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := storetest.OpenDir(t, dir)
	store.ImportLegacy(db, dir) // 损坏 = 跳过，不 panic
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, cst.Loc)
	if got := New(Config{CheckinHours: []int{9}, DB: db}).missedToday(now); !hasKind(got, taskCheckin) {
		t.Fatalf("got %v", got)
	}
}

// 旧 task-runs.json 会被导入：导入后同一天不再补跑。
func TestStartupCatchUpImportsLegacyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task-runs.json"), []byte("{\"checkin\":\"2026-10-20\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := storetest.OpenDir(t, dir)
	store.ImportLegacy(db, dir)
	now := time.Date(2026, 10, 20, 15, 0, 0, 0, cst.Loc)
	if got := New(Config{CheckinHours: []int{9}, DB: db}).missedToday(now); hasKind(got, taskCheckin) {
		t.Fatalf("导入后不该再补，got %v", got)
	}
}
