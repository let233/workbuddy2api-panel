// hooks.ts 通用钩子：取数、轮询、防抖、媒体查询、本地存储、选中集合。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

export type QueryState<T> = {
  data: T | undefined
  error: unknown
  loading: boolean
  /** 首次加载还没有任何数据 */
  first: boolean
  reload: () => void
  setData: (v: T | undefined) => void
}

/**
 * useQuery 统一取数：
 * - deps 变化 / reload() 重新取，取数中保留旧数据（不闪烁）
 * - poll>0 时定时静默刷新，页面不可见时暂停，重新可见立刻补一次
 * - 卸载或依赖变化时 abort 掉上一个请求
 */
export function useQuery<T>(
  fn: (signal: AbortSignal) => Promise<T>,
  deps: unknown[],
  opts: { poll?: number; enabled?: boolean } = {},
): QueryState<T> {
  const { poll = 0, enabled = true } = opts
  const fnRef = useRef(fn)
  fnRef.current = fn

  const [data, setData] = useState<T | undefined>(undefined)
  const [error, setError] = useState<unknown>(null)
  const [loading, setLoading] = useState<boolean>(enabled)
  const [tick, setTick] = useState(0)
  const key = JSON.stringify(deps)

  const reload = useCallback(() => setTick((t) => t + 1), [])

  useEffect(() => {
    if (!enabled) {
      setLoading(false)
      return
    }
    let cancelled = false
    const ac = new AbortController()
    let timer: number | undefined

    const run = async (silent: boolean) => {
      if (!silent) setLoading(true)
      try {
        const r = await fnRef.current(ac.signal)
        if (cancelled) return
        setData(r)
        setError(null)
      } catch (e) {
        if (cancelled || (e as Error)?.name === 'AbortError') return
        setError(e)
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    void run(false)

    if (poll > 0) {
      timer = window.setInterval(() => {
        if (document.visibilityState === 'visible') void run(true)
      }, poll)
    }
    const onVisible = () => {
      if (document.visibilityState === 'visible') void run(true)
    }
    document.addEventListener('visibilitychange', onVisible)

    return () => {
      cancelled = true
      ac.abort()
      if (timer) window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, tick, enabled, poll])

  return {
    data,
    error,
    loading,
    first: loading && data === undefined,
    reload,
    setData,
  }
}

/** 异步动作的忙碌标记：同一时刻只允许一个动作占位（按钮 loading）。 */
export function useBusy() {
  const [busy, setBusy] = useState<string>('')
  const run = useCallback(async (label: string, fn: () => Promise<unknown>) => {
    setBusy(label)
    try {
      return await fn()
    } finally {
      setBusy('')
    }
  }, [])
  return { busy, run }
}

export function useDebounced<T>(value: T, ms = 300): T {
  const [v, setV] = useState(value)
  useEffect(() => {
    const t = window.setTimeout(() => setV(value), ms)
    return () => window.clearTimeout(t)
  }, [value, ms])
  return v
}

export function useMediaQuery(query: string): boolean {
  const get = () => (typeof window === 'undefined' ? false : window.matchMedia(query).matches)
  const [matches, setMatches] = useState(get)
  useEffect(() => {
    const mq = window.matchMedia(query)
    const on = () => setMatches(mq.matches)
    on()
    mq.addEventListener('change', on)
    return () => mq.removeEventListener('change', on)
  }, [query])
  return matches
}

export function useLocalStorage<T>(key: string, initial: T) {
  const [value, setValue] = useState<T>(() => {
    try {
      const raw = localStorage.getItem(key)
      return raw === null ? initial : (JSON.parse(raw) as T)
    } catch {
      return initial
    }
  })
  useEffect(() => {
    try {
      localStorage.setItem(key, JSON.stringify(value))
    } catch {
      /* 隐私模式下写不进去，忽略 */
    }
  }, [key, value])
  return [value, setValue] as const
}

/** 每 ms 毫秒推进一次的时钟，用于「N 分钟前」这类相对时间。 */
export function useNow(ms = 30000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = window.setInterval(() => {
      if (document.visibilityState === 'visible') setNow(Date.now())
    }, ms)
    return () => window.clearInterval(t)
  }, [ms])
  return now
}

/** 跨分页的多选集合：key 用行的稳定标识。 */
export function useSelection<K extends string>() {
  const [set, setSet] = useState<Set<K>>(() => new Set())
  const has = useCallback((k: K) => set.has(k), [set])
  const toggle = useCallback((k: K) => {
    setSet((prev) => {
      const next = new Set(prev)
      if (next.has(k)) next.delete(k)
      else next.add(k)
      return next
    })
  }, [])
  const addMany = useCallback((ks: K[]) => {
    setSet((prev) => {
      const next = new Set(prev)
      for (const k of ks) next.add(k)
      return next
    })
  }, [])
  const clear = useCallback(() => setSet(new Set()), [])
  const list = useMemo(() => Array.from(set), [set])
  return { set, list, size: set.size, has, toggle, addMany, clear }
}
