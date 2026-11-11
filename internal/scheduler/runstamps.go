package scheduler

// 补跑标记：把「今天已经有哪几类任务补跑过」记下来，让重启不再重复打上游。
//
// 排程只活在内存里时，进程重启就会被 missedToday 判成「今天的时点全过了、还没跑」，
// 于是每次启动都把签到 / 旅行 / 活跃上报对每个账号各打一遍；一天重开三次就是三倍
// 无意义请求（账号越多、被上游风控盯上的概率越高）。同一思路见 agent2api 的
// task_state.rs：「重启不重置节奏，停机期间只补一次」。
//
// 只记日期不记时刻：精度到「天」就够（missedToday 的判据本来也是按天），键最多六行，
// 旧日期写过就丢，不需要清理策略。

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/misakano7545/workbuddy2api-panel/internal/cst"
	"github.com/misakano7545/workbuddy2api-panel/internal/store"
)

// runStampKeyPrefix 键前缀：app_kv 里 task_run:<kind> → 自然日（CST）。
const runStampKeyPrefix = "task_run:"

// ranToday 报告 kind 今天是否已经补跑过。未配置 DB 时恒 false（旧行为）。
func (s *Scheduler) ranToday(kind taskKind, now time.Time) bool {
	if s.cfg.DB == nil {
		return false
	}
	s.stampMu.Lock()
	defer s.stampMu.Unlock()
	return s.loadStamps()[kind.String()] == cst.Day(now)
}

// markRan 把 kinds 记成「今天已补跑」。落库失败只记日志：下次重启多补一轮，
// 比让签到状态机因为写库错误而停摆好。
func (s *Scheduler) markRan(kinds []taskKind, now time.Time) {
	if s.cfg.DB == nil || len(kinds) == 0 {
		return
	}
	s.stampMu.Lock()
	defer s.stampMu.Unlock()
	day := cst.Day(now)
	st := s.loadStamps()
	for k, v := range st {
		if v != day {
			// 只留当天，键不会越积越多
			if err := store.KVDel(s.cfg.DB, runStampKeyPrefix+k); err != nil {
				log.Printf("scheduler: 清理过期补跑标记失败: %v", err)
			}
		}
	}
	for _, k := range kinds {
		if err := store.KVSet(s.cfg.DB, runStampKeyPrefix+k.String(), day); err != nil {
			log.Printf("scheduler: 补跑标记落库失败（下次重启会再补一轮）: %v", err)
		}
	}
}

// loadStamps 读标记；库不可用/读失败都当空表（宁可多补一轮，不因此不跑）。
func (s *Scheduler) loadStamps() map[string]string {
	st := map[string]string{}
	if s.cfg.DB == nil {
		return st
	}
	rows, err := store.KVList(s.cfg.DB, runStampKeyPrefix)
	if err != nil {
		log.Printf("scheduler: 读取补跑标记失败（当作未补跑）: %v", err)
		return st
	}
	for _, kv := range rows {
		st[strings.TrimPrefix(kv.Key, runStampKeyPrefix)] = kv.Value
	}
	return st
}

// importLegacyRunStamps 把旧 data/task-runs.json（kind → 自然日）搬进 app_kv。
func importLegacyRunStamps(st *store.Store, dir string) error {
	raw, ok := store.LegacyFile(dir, "task-runs.json")
	if !ok {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	for k, v := range m {
		if k == "" || v == "" {
			continue
		}
		if err := store.KVSet(st, runStampKeyPrefix+k, v); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	store.RegisterLegacy(store.LegacyImport{Key: "taskruns", Name: "task-runs.json", Fn: importLegacyRunStamps})
}
