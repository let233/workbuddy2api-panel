package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// managedConfigStub 冒充 cmd/server 的 Config：只实现「被环境变量托管字段」这一可选能力。
// 面板不 import 具体配置类型，configManagedFields 靠接口断言取值，这里正好走同一条路径。
type managedConfigStub struct{}

func (managedConfigStub) EnvironmentManagedFields() []string {
	return []string{"upstream.user_agent"}
}

// TestConfigAPIReportsEnvironmentManagedFields 配置读写接口都要回传
// environment_managed_fields（前端据此把被 WB2A_* 托管的输入框置灰），
// 且不再有 api_key 相关的环境托管标记（全局密钥已删除）。
func TestConfigAPIReportsEnvironmentManagedFields(t *testing.T) {
	p := New(Config{
		LoadConfig: func() (any, error) { return managedConfigStub{}, nil },
		SaveConfig: func([]byte) ([]string, error) { return nil, nil },
	})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		body := ""
		if method == http.MethodPost {
			body = "{\"upstream\":{\"user_agent\":\"x\"}}"
		}
		req := httptest.NewRequest(method, "/panel/api/config", strings.NewReader(body))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("method=%s status=%d body=%s", method, rec.Code, rec.Body.String())
		}
		fields, _ := response["environment_managed_fields"].([]any)
		if len(fields) != 1 || fields[0] != "upstream.user_agent" {
			t.Fatalf("method=%s environment_managed_fields=%v", method, response["environment_managed_fields"])
		}
		if _, ok := response["api_key_env_managed"]; ok {
			t.Fatalf("method=%s 不应再有 api_key_env_managed：%v", method, response)
		}
	}
}
