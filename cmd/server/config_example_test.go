package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigExampleIsValid 守卫仓库根目录的 config.example.json：合法 JSON、能被 Config
// 解析，且**只含引导参数**。
//
// 来由有二：
//   - PR #55 加 features.sanitize_brand_scope 时漏了逗号，文件变成非法 JSON 而 CI 全绿；
//   - 本次改造后运行期配置全部入库，示例里再留一堆键会让人以为「改文件能生效」，
//     那正是这次要根除的困惑。所以这里反过来断言「除了引导参数什么都不能有」。
func TestConfigExampleIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	if !json.Valid(raw) {
		t.Fatalf("%s 不是合法 JSON", path)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s 无法解析为 Config: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s 无法解析为对象: %v", path, err)
	}
	for _, k := range bootstrapKeys {
		if _, ok := doc[k]; !ok {
			t.Errorf("示例缺少引导参数 %q（首次部署的用户只会看这个文件）", k)
		}
	}
	for k := range doc {
		if strings.HasPrefix(k, "_") || isBootstrapKey(k) {
			continue
		}
		t.Errorf("示例里不该有 %q：运行期配置已入库，写进文件不会生效", k)
	}
	if c.Database.Driver == "" || c.Database.DSN == "" {
		t.Errorf("database 示例不完整：%+v", c.Database)
	}
	if c.StateFile == "" {
		t.Error("state_file 应有示例值")
	}
}
