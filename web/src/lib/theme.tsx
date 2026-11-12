// theme.tsx 主题：auto 跟随系统 + light/dark 手选，选择存在 localStorage['wb2api.theme']。
// 判定口径与原 theme.js 逐字一致：只有存了 light/dark 才照办，其余（含脏值）按系统偏好，
// 系统不是明确浅色就算深色——否则老用户刷新会看到「先深后浅」的二次闪烁。
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'

export type ThemeMode = 'auto' | 'light' | 'dark'
export type Resolved = 'light' | 'dark'

const STORAGE_KEY = 'wb2api.theme'

function systemResolved(): Resolved {
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark'
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
}

export function resolveTheme(mode: ThemeMode): Resolved {
  if (mode === 'auto') return systemResolved()
  return mode
}

function apply(resolved: Resolved) {
  document.documentElement.setAttribute('data-theme', resolved)
  document.documentElement.style.colorScheme = resolved
}

function readMode(): ThemeMode {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw === 'light' || raw === 'dark') return raw
  } catch {
    /* 隐私模式：按 auto */
  }
  return 'auto'
}

/** 在 React 挂载之前调用一次，避免首帧之后才翻主题。 */
export function bootstrapTheme(): ThemeMode {
  const mode = readMode()
  apply(resolveTheme(mode))
  return mode
}

type ThemeCtx = {
  mode: ThemeMode
  resolved: Resolved
  setMode: (m: ThemeMode) => void
  cycle: () => void
}

const Ctx = createContext<ThemeCtx | null>(null)

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(() => readMode())
  const [resolved, setResolved] = useState<Resolved>(() => resolveTheme(readMode()))

  const setMode = useCallback((m: ThemeMode) => {
    setModeState(m)
    try {
      localStorage.setItem(STORAGE_KEY, m)
    } catch {
      /* 存不了就只在本次会话生效 */
    }
  }, [])

  // auto 模式下系统切深浅色要跟着变
  useEffect(() => {
    const next = resolveTheme(mode)
    setResolved(next)
    apply(next)
    if (mode !== 'auto' || !window.matchMedia) return
    const mq = window.matchMedia('(prefers-color-scheme: light)')
    const onChange = () => {
      const r = systemResolved()
      setResolved(r)
      apply(r)
    }
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [mode])

  const cycle = useCallback(() => {
    setMode(mode === 'dark' ? 'light' : mode === 'light' ? 'auto' : 'dark')
  }, [mode, setMode])

  const value = useMemo(() => ({ mode, resolved, setMode, cycle }), [mode, resolved, setMode, cycle])
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useTheme(): ThemeCtx {
  const v = useContext(Ctx)
  if (!v) throw new Error('useTheme 必须在 ThemeProvider 内使用')
  return v
}
