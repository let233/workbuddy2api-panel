package store

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 旧 JSON 数据的一次性导入。
//
// 面板此前的运行数据都摊在数据目录下的 JSON 文件里（state.json / usage.json /
// credit-history.json / request-logs/*.jsonl …）。换成数据库后，升级上来的部署不能
// 把历史直接归零：各业务包在 init 里登记一个导入器，Open 之后由 ImportLegacy 统一
// 执行一次，成功后写 app_kv 标记（并把旧文件改名成 *.imported 留档），重启不再重跑。
//
// 用登记制而不是集中式 switch：store 不反向依赖业务包（否则循环 import），
// 新数据集的导入逻辑和它的表结构放在一起，删掉业务包时导入逻辑一起消失。

// LegacyImport 一个「旧文件 → 表」的一次性导入器。
type LegacyImport struct {
	// Key 幂等键（app_kv 里 legacy:imported:<Key>）。
	Key string
	// Name 旧数据文件名（相对数据目录，仅用于日志与改名留档；目录型数据填目录名）。
	Name string
	// Fn 实际导入逻辑。旧文件不存在时返回 nil（视为无事可做）。
	Fn func(s *Store, dir string) error
}

var (
	legacyMu  sync.Mutex
	legacyFns []LegacyImport
)

// RegisterLegacy 登记一个导入器（业务包 init 里调用）。
func RegisterLegacy(imp LegacyImport) {
	if imp.Key == "" || imp.Fn == nil {
		return
	}
	legacyMu.Lock()
	defer legacyMu.Unlock()
	legacyFns = append(legacyFns, imp)
}

// legacyKVKey 导入完成标记在 app_kv 里的键前缀。
const legacyKVKey = "legacy:imported:"

// ImportLegacy 执行全部已登记导入器（每个最多成功一次）。dir 是数据目录
// （旧版 state.json 所在目录，即 config.json 的 state_file 所在目录）。
func ImportLegacy(s *Store, dir string) {
	if s == nil || dir == "" {
		return
	}
	legacyMu.Lock()
	list := make([]LegacyImport, len(legacyFns))
	copy(list, legacyFns)
	legacyMu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })

	for _, imp := range list {
		key := legacyKVKey + imp.Key
		if v, ok, err := KVGet(s, key); err == nil && ok && v != "" {
			continue
		}
		if err := runLegacy(s, dir, imp); err != nil {
			log.Printf("store: 导入旧数据 %s 失败（已跳过，下次启动重试）: %v", imp.Name, err)
			continue
		}
		if err := KVSet(s, key, time.Now().Format(time.RFC3339)); err != nil {
			log.Printf("store: 记录旧数据导入标记 %s 失败: %v", imp.Key, err)
		}
	}
}

func runLegacy(s *Store, dir string, imp LegacyImport) error {
	path, exists := LegacyPath(dir, imp.Name)
	if !exists {
		// 没有旧文件：全新部署，直接标记完成，免得每次启动都来找。
		return imp.Fn(s, dir)
	}
	if err := imp.Fn(s, dir); err != nil {
		return err
	}
	// 改名留档：数据已经在库里，旧文件留着占地方还容易被误读成"生效中"。
	if err := os.Rename(path, path+".imported"); err == nil {
		log.Printf("store: 旧数据 %s 已导入数据库（原文件改名为 %s.imported）", imp.Name, imp.Name)
	} else {
		log.Printf("store: 旧数据 %s 已导入数据库（改名留档失败: %v）", imp.Name, err)
	}
	return nil
}

// LegacyFile 读取数据目录下的旧文件；不存在返回 ok=false。
func LegacyFile(dir, name string) ([]byte, bool) {
	if dir == "" || name == "" {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// LegacyPath 返回数据目录下旧文件/旧目录的路径；不存在返回 ok=false。
func LegacyPath(dir, name string) (string, bool) {
	if dir == "" || name == "" {
		return "", false
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}
