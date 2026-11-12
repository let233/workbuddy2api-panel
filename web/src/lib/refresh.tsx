// refresh.tsx 全局刷新信号：顶栏的刷新按钮 + 自动刷新开关，
// 各视图把自己的刷新计数塞进 useQuery 的 deps，就能一起响应。
import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'

type RefreshApi = {
  /** 每次点刷新 +1 */
  tick: number
  refresh: () => void
  /** 自动刷新间隔（毫秒），0 = 关闭 */
  interval: number
  setInterval: (ms: number) => void
}

const Ctx = createContext<RefreshApi | null>(null)

export function RefreshProvider({ children }: { children: ReactNode }) {
  const [tick, setTick] = useState(0)
  const [interval, setIntervalMs] = useState(0)

  const refresh = useCallback(() => setTick((t) => t + 1), [])
  const value = useMemo(
    () => ({ tick, refresh, interval, setInterval: setIntervalMs }),
    [tick, refresh, interval],
  )
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useRefresh(): RefreshApi {
  const v = useContext(Ctx)
  if (!v) throw new Error('useRefresh 必须在 RefreshProvider 内使用')
  return v
}

/** 视图里常用的组合：刷新按钮/自动刷新都能触发重新取数。 */
export function useAutoRefresh(pollMs = 0) {
  const { tick, interval } = useRefresh()
  const ms = interval > 0 ? interval : pollMs
  return { tick, poll: ms }
}
