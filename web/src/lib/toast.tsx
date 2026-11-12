// toast.tsx 轻量提示：右上角（移动端顶部）堆叠，4 秒自动消失，最多同时 3 条。
// 面板的写操作都靠它给反馈，所以不进 alert/confirm——浏览器原生弹窗在移动端不可用。
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'

export type ToastKind = 'ok' | 'err' | 'warn' | 'info'

type ToastItem = { id: number; kind: ToastKind; text: string; at: number }

type ToastApi = {
  push: (kind: ToastKind, text: string) => void
  ok: (text: string) => void
  err: (text: string) => void
  warn: (text: string) => void
  info: (text: string) => void
}

const Ctx = createContext<ToastApi | null>(null)

const MAX = 3
const TTL = 4000

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])
  const seq = useRef(0)

  const push = useCallback((kind: ToastKind, text: string) => {
    seq.current += 1
    const id = seq.current
    setItems((prev) => {
      const next = [...prev, { id, kind, text, at: Date.now() }]
      return next.length > MAX ? next.slice(next.length - MAX) : next
    })
  }, [])

  const api = useMemo<ToastApi>(
    () => ({
      push,
      ok: (t: string) => push('ok', t),
      err: (t: string) => push('err', t),
      warn: (t: string) => push('warn', t),
      info: (t: string) => push('info', t),
    }),
    [push],
  )

  // 单一定时器扫过期项，避免每条 toast 各自挂 timer
  useEffect(() => {
    if (items.length === 0) return
    const timer = window.setTimeout(() => {
      const cutoff = Date.now() - TTL
      setItems((prev) => prev.filter((t) => t.at > cutoff))
    }, 500)
    return () => window.clearTimeout(timer)
  }, [items])

  return (
    <Ctx.Provider value={api}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={'toast ' + t.kind} role={t.kind === 'err' ? 'alert' : undefined}>
            <span className="t">{t.text}</span>
            <button
              type="button"
              className="x"
              aria-label="关闭提示"
              onClick={() => setItems((prev) => prev.filter((p) => p.id !== t.id))}
            >
              ✕
            </button>
          </div>
        ))}
      </div>
    </Ctx.Provider>
  )
}

export function useToast(): ToastApi {
  const v = useContext(Ctx)
  if (!v) throw new Error('useToast 必须在 ToastProvider 内使用')
  return v
}
