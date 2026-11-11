// sweep.go 补领清扫排程：分钟级周期，只 accept 待接受的、只 claim 已达标未领的。
//
// 执行体在面板（panel/tasksweep.go）：main 装配期用 SetSweepRunner 注入
// （panel 依赖本包，反向 import 会成环）。独立于 Run 的小时制排程——补领是分钟级
// 观测量，且不跑行为链路，代价只是每账号 2 次列表请求。
package scheduler

import (
	"context"
	"log"
	"time"
)

// RunTaskSweepNow 立即执行一轮补领清扫。未注入执行器（nil）时静默返回：
// 测试与未装配面板的部署不必额外判断。
func (s *Scheduler) RunTaskSweepNow() {
	s.schedMu.Lock()
	fn := s.cfg.SweepRunner
	s.schedMu.Unlock()
	if fn == nil {
		return
	}
	_, msg := fn()
	log.Printf("补领清扫：%s", msg)
}

// StartTaskSweep 启动补领清扫循环（ctx 取消即停）。
//
// 语义（配置全热生效）：循环**常驻**，是否真的跑由 sweepEnabled 原子开关决定。
// interval<=0 不再表示"不启动"，而是"先关着"——面板打开
// schedule.task_sweep_enabled 就能立刻开始清扫，不必重启进程。main 只需在装配末尾
// 无条件调一次。
func (s *Scheduler) StartTaskSweep(ctx context.Context, interval time.Duration) {
	s.SetTaskSweep(interval > 0, interval, time.Duration(s.sweepJitterDur.Load()))
	s.startSweepLoop(ctx)
}

// SetTaskSweep 热改补领清扫（面板保存配置后调用）。
//
// enabled=false 时循环空转等待（只等 ctx 结束或下一次 rearm，不 busy loop）；重新
// 启用时 rearm 立刻唤醒。jitter 语义同 SetBalanceRefresh：总抖动宽度，±jitter/2。
func (s *Scheduler) SetTaskSweep(enabled bool, interval, jitter time.Duration) {
	if interval < 0 {
		interval = 0
	}
	if jitter < 0 {
		jitter = 0
	}
	if interval <= 0 {
		enabled = false
	}
	s.sweepInterval.Store(int64(interval))
	s.sweepJitterDur.Store(int64(jitter))
	s.sweepEnabled.Store(enabled)
	poke(s.rearmSweep)
}

// startSweepLoop 起常驻循环（幂等）。
//
// 与余额刷新不同，这里启动时**不**立刻跑一轮：补领清扫要遍历任务中心（每账号 2 次
// 列表请求），进程刚起来就齐射一轮与旧行为不一致，也没必要（旧版就是等第一个 interval）。
func (s *Scheduler) startSweepLoop(ctx context.Context) {
	if !s.sweepLoopOn.CompareAndSwap(false, true) {
		return
	}
	go func() {
		logged := time.Duration(-1)
		for {
			cur := time.Duration(s.sweepInterval.Load())
			if !s.sweepEnabled.Load() || cur <= 0 {
				// 关闭：等唤醒（面板打开开关）或退出。不 sleep 轮询 = 不 busy loop。
				select {
				case <-ctx.Done():
					return
				case <-s.rearmSweep:
					continue
				}
			}
			if cur != logged {
				log.Printf("scheduler: 补领清扫每 %s（关闭时显示 0s）", cur)
				logged = cur
			}
			delay := cur + periodicJitter(cur, s.periodicJitterFor(true), "sweep") +
				absoluteJitter(time.Duration(s.sweepJitterDur.Load()), "sweepabs")
			if delay < 0 {
				delay = 0
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmSweep:
				timer.Stop() // 间隔/开关/抖动已变：立刻按新值重算
			case <-timer.C:
				s.RunTaskSweepNow()
			}
		}
	}()
}

// SetTaskSweepInterval 热改补领清扫间隔（旧入口，保留兼容）：<=0 表示关闭循环。
func (s *Scheduler) SetTaskSweepInterval(d time.Duration) {
	s.SetTaskSweep(d > 0, d, time.Duration(s.sweepJitterDur.Load()))
}

// TaskSweepEnabled 补领清扫是否启用（面板/观测用）。
func (s *Scheduler) TaskSweepEnabled() bool { return s.sweepEnabled.Load() }
