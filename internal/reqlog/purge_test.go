package reqlog

import (
	"testing"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
	"github.com/misakano7545/workbuddy2api-panel/internal/store/storetest"
)

// TestPurgeClearsArchiveAndKeepsWriting 手动清空归档：删掉归档表全部行，之后照常写入。
//
// 关键在「之后照常写入」：清空是清表，写循环仍在跑，下一条事件必须还能读出来
// （旧的删文件实现里忘了清 w.day/path，新事件会写进已删除的 inode，日志静默丢失）。
func TestPurgeClearsArchiveAndKeepsWriting(t *testing.T) {
	db := storetest.Open(t)
	r := New(Config{DB: db, Enabled: true, MaxBytes: 1 << 20, RetentionDays: 7})
	base := time.Now().Add(-time.Minute)
	for i, id := range []string{"a", "b"} {
		r.Record(Event{Time: base.Add(time.Duration(i) * time.Second), RequestID: id, Status: 200, OK: true})
	}

	res, err := r.Purge()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.Rows != 2 || res.Bytes == 0 {
		t.Fatalf("应删掉两条归档行: %+v", res)
	}
	if got, err := store.Count[reqLogRow](db); err != nil || got != 0 {
		t.Fatalf("清空后归档表应为空: %d 行 (err=%v)", got, err)
	}

	// 清空后写的新事件必须照常落库并可读。
	r.Record(Event{Time: base.Add(3 * time.Second), RequestID: "after", Status: 200, OK: true})
	r.Close()
	rows, err := r.ReadArchive(10, Filter{})
	if err != nil {
		t.Fatalf("ReadArchive: %v", err)
	}
	if len(rows) != 1 || rows[0].RequestID != "after" {
		t.Fatalf("清空后应只读到新事件，实际 %+v", rows)
	}
}

// TestPurgeDisabledArchiveIsNoop 归档关闭时清空是空操作、不报错（面板据此 200 + 0）。
func TestPurgeDisabledArchiveIsNoop(t *testing.T) {
	r := New(Config{})
	res, err := r.Purge()
	if err != nil || res.Rows != 0 || res.Bytes != 0 {
		t.Fatalf("归档关闭时清空应为空操作: %+v %v", res, err)
	}
}

// TestPurgeAfterCloseReturns 归档写循环停了以后清空必须立即返回错误，不能卡住调用方
// （面板的 HTTP handler）。purgeCh 带缓冲时，写循环退出后「发进通道」和「done 已关」
// 两个分支同时就绪，select 随机选；选中发送就会永远等一个没人回的结果。
func TestPurgeAfterCloseReturns(t *testing.T) {
	r := New(Config{DB: storetest.Open(t), Enabled: true, MaxBytes: 1 << 20, RetentionDays: 7})
	r.Close()
	for i := 0; i < 50; i++ {
		errc := make(chan error, 1)
		go func() {
			_, err := r.Purge()
			errc <- err
		}()
		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("归档已停止时清空应返回错误")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次清空卡住：写循环已退出，请求没人处理", i+1)
		}
	}
}
