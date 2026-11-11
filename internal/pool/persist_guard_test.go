package pool

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/auth"
)

// TestCorruptDBLogsInsteadOfSilentLoss 库不可读（表被删 / 文件损坏）此前被静默丢弃：
// 池子以 0 账号启动、credits/冷却/熔断全丢，日志里却一个字都没有（运维只看到"账号全没了"）。
// 必须留下痕迹。
func TestCorruptDBLogsInsteadOfSilentLoss(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	p := New(db)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 7, 0)
	p.Flush()
	p.Close()
	// 模拟"库结构损坏/被外部清理"：账号表没了。
	if err := db.DB().Exec("DROP TABLE pool_accounts").Error; err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	p2 := New(db)
	defer p2.Close()

	if _, ok := p2.Status("u1"); ok {
		t.Fatal("库不可读时不应凭空恢复出账号状态")
	}
	if !strings.Contains(buf.String(), "读取账号状态失败") {
		t.Fatalf("库读取失败被静默丢弃，日志=%q", buf.String())
	}
}

// TestFlushRetriesAfterFailedSave 落盘失败后 dirty 必须置回：此前 Swap(false) 在写盘前
// 执行，一败即永久搁置（库恢复也不补写），这段内存变更要等到下次状态变更才碰巧落盘。
func TestFlushRetriesAfterFailedSave(t *testing.T) {
	db := openTestDB(t, t.TempDir())
	p := New(db)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	// 删掉窗口表 → ReplaceAll(pool_windows) 必失败（库结构损坏的等价场景）。
	if err := db.DB().Exec("DROP TABLE pool_windows").Error; err != nil {
		t.Fatal(err)
	}
	p.Flush()
	if !p.dirty.Load() {
		t.Fatal("落盘失败后 dirty 应置回，否则库恢复后也不会补写")
	}

	// 库结构恢复（AutoMigrate 重建）后必须能补写成功。
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	p.Flush()
	raw := dbStateJSON(t, db)
	if !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("库恢复后应补写成功: %s", raw)
	}
	if p.dirty.Load() {
		t.Fatal("成功落盘后 dirty 应清")
	}
}

// TestStateOverviewDropsExpiredCooldownFields 账号级软冷却按墙钟到期、reason/until/cool_kind
// 字段本身不清理（NoteSuccess 不碰），落盘时必须惰性清洗——否则过期 reason 永久留在
// 库里并被重启原样拾回（issue #207-4；显示层已由 statusOf 门控挡住）。
func TestStateOverviewDropsExpiredCooldownFields(t *testing.T) {
	p := New(nil)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Cooldown("u1", CoolSoft, -time.Minute, "429 rate limit") // 已到期，字段残留
	p.Cooldown("u2", CoolSoft, time.Hour, "429 rate limit")    // 仍在冷却

	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	if a := sf.Accounts["u1"]; a.Reason != "" || !a.Until.IsZero() || a.CoolKind != 0 {
		t.Fatalf("过期冷却不应落盘: %+v", a)
	}
	if a := sf.Accounts["u2"]; a.Reason != "429 rate limit" || a.Until.IsZero() || a.CoolKind != CoolSoft {
		t.Fatalf("冷却中的字段必须照常落盘: %+v", a)
	}

	// 禁用原因走同一个字段，不能被清洗掉（否则重启后看不到为什么死）。
	p.Disable("u1", "12153 session dead")
	p.mu.Lock()
	sf = p.stateOverviewLocked()
	p.mu.Unlock()
	if a := sf.Accounts["u1"]; !a.Disabled || a.Reason != "12153 session dead" {
		t.Fatalf("禁用原因必须落盘: %+v", a)
	}
}
