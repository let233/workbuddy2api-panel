package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLoginPollGoneForUnknownState 未知/过期 state 必须回 410（终态）：前端据此停止轮询
// 并提示重新发起；若与网络抖动共用一个状态码，一次抖动就会让用户放弃等待。
func TestLoginPollGoneForUnknownState(t *testing.T) {
	p := New(Config{})
	rec := httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=nope", nil))
	if rec.Code != http.StatusGone {
		t.Fatalf("code=%d want 410 body=%s", rec.Code, rec.Body)
	}
}

// TestLoginPollProcessingGate 成功分支的副作用（落盘/签到/余额）可能耗时数秒，而前端每
// 3s 轮询一次：并发 poll 必须被闸门挡住，否则整段流程会重跑一遍（重复落盘 + 重复签到）。
func TestLoginPollProcessingGate(t *testing.T) {
	p := New(Config{})
	p.logins = map[string]loginSession{"st": {created: time.Now(), realm: "cn", processing: true}}
	rec := httptest.NewRecorder()
	p.loginPoll(rec, httptest.NewRequest("GET", "/panel/api/login/poll?state=st", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v (%s)", err, rec.Body)
	}
	if got["done"] != false {
		t.Fatalf("并发 poll 不应宣判完成：%v", got)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "正在写入凭据") {
		t.Fatalf("message=%q want 正在写入凭据", msg)
	}
}
