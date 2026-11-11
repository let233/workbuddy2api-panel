package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/misakano7545/workbuddy2api-panel/internal/store"
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

// TestConfigExampleDatabaseSamples 守卫 config.example.json 里的 "_数据库示例"：
// 每条样例都得能被 store 认出驱动的别名、且 dsn 非空，三种驱动都要有。
//
// 为什么要管这个：换库是很容易踩坑的一件事（SQLite 共享盘、MySQL 漏 parseTime、
// 把驱动名拼进 dsn）。样例写错等于把人往坑里带，而 JSON 本身合法、Config 也能解析，
// 上面那个测试一个字都发现不了。
func TestConfigExampleDatabaseSamples(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到 %s: %v", path, err)
	}
	var root struct {
		Samples map[string]json.RawMessage `json:"_数据库示例"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("%s 无法解析: %v", path, err)
	}
	// 三个驱动各自一组，另加一个环境变量覆盖的说明块。
	groups := map[string][]string{"sqlite": nil, "mysql": nil, "postgres": nil}
	for group, body := range root.Samples {
		if strings.HasPrefix(group, "_") || group == "环境变量覆盖" {
			continue
		}
		if _, ok := groups[group]; !ok {
			t.Errorf("_数据库示例 里出现未知分组 %q（只应有 sqlite / mysql / postgres / 环境变量覆盖）", group)
			continue
		}
		var variants map[string]json.RawMessage
		if err := json.Unmarshal(body, &variants); err != nil {
			t.Fatalf("分组 %s 不是对象: %v", group, err)
		}
		for name, v := range variants {
			if strings.HasPrefix(name, "_") {
				continue // _说明 / _注意 这类注释字段
			}
			var one struct {
				Driver string `json:"driver"`
				DSN    string `json:"dsn"`
			}
			if err := json.Unmarshal(v, &one); err != nil {
				t.Errorf("%s / %s 不是 {driver,dsn}: %v", group, name, err)
				continue
			}
			if !store.ValidDriver(one.Driver) {
				t.Errorf("%s / %s 的 driver %q 不被 store 接受", group, name, one.Driver)
			}
			if strings.TrimSpace(one.DSN) == "" {
				t.Errorf("%s / %s 的 dsn 是空的", group, name)
			}
			// 把驱动名写进 dsn 是最常见的误用：sqlite 会拿它当文件名，MySQL 会直接连不上。
			// PostgreSQL 例外——postgres://user:pass@host/db 是 pgx 正式支持的 URL 形态，
			// 所以只拦「postgres:」后面不跟 // 的那种半吊子写法。
			lower := strings.ToLower(strings.TrimSpace(one.DSN))
			for _, alias := range []string{"sqlite:", "mysql:"} {
				if strings.HasPrefix(lower, alias) {
					t.Errorf("%s / %s 的 dsn 不要带驱动前缀（%q）", group, name, alias)
				}
			}
			if (strings.HasPrefix(lower, "postgres:") || strings.HasPrefix(lower, "postgresql:")) &&
				!strings.HasPrefix(lower, "postgres://") && !strings.HasPrefix(lower, "postgresql://") {
				t.Errorf("%s / %s 的 dsn 写成 postgres:… 了，URL 形态要 postgres://…", group, name)
			}
			groups[group] = append(groups[group], name)
		}
	}
	for group, names := range groups {
		if len(names) == 0 {
			t.Errorf("_数据库示例 缺少 %s 的连接样例", group)
		}
	}
	// 环境变量覆盖块：两个变量名要写对，值是能直接用的样例。
	var env struct {
		Driver string `json:"WB2A_DB_DRIVER"`
		DSN    string `json:"WB2A_DB_DSN"`
	}
	if body, ok := root.Samples["环境变量覆盖"]; ok {
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("环境变量覆盖块无法解析: %v", err)
		}
		if !store.ValidDriver(env.Driver) {
			t.Errorf("WB2A_DB_DRIVER 样例 %q 不被 store 接受", env.Driver)
		}
		if strings.TrimSpace(env.DSN) == "" {
			t.Error("WB2A_DB_DSN 样例为空")
		}
	}
}
