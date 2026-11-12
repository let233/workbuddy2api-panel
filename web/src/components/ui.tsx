// ui.tsx 通用展示组件：只做结构与可访问性，样式全部来自 styles/components.css 的类名。
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { createPortal } from 'react-dom'

export type Tone = 'ok' | 'warn' | 'bad' | 'accent' | 'muted'

// ---------- 容器 ----------

export function Box({
  title,
  sub,
  actions,
  children,
  footer,
  className = '',
  bodyClass = '',
  flush = false,
}: {
  title?: ReactNode
  sub?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  footer?: ReactNode
  className?: string
  bodyClass?: string
  /** 不给 body 加内边距（表格自己贴边） */
  flush?: boolean
}) {
  const hasHead = title !== undefined || sub !== undefined || actions !== undefined
  return (
    <section className={'box ' + className}>
      {hasHead && (
        <header>
          <div className="grow">
            {title !== undefined && <h2 className="h">{title}</h2>}
            {sub !== undefined && <div className="sub">{sub}</div>}
          </div>
          {actions && <div className="row flex-shrink0">{actions}</div>}
        </header>
      )}
      <div className={(flush ? 'body flush ' : 'body ') + bodyClass}>{children}</div>
      {footer && <footer>{footer}</footer>}
    </section>
  )
}

export function Stat({
  label,
  value,
  sub,
  tone,
  icon,
}: {
  label: ReactNode
  value: ReactNode
  sub?: ReactNode
  tone?: Tone
  icon?: ReactNode
}) {
  return (
    <div className={'stat ' + (tone || '')}>
      <div className="k">
        {icon && <span className="ic">{icon}</span>}
        {label}
      </div>
      <div className="v">{value}</div>
      {sub !== undefined && <div className="sub">{sub}</div>}
    </div>
  )
}

export function StatsGrid({ children, cols }: { children: ReactNode; cols?: number }) {
  const style = cols ? { gridTemplateColumns: 'repeat(auto-fit, minmax(' + (168 * 1) + 'px, 1fr))' } : undefined
  void cols
  return (
    <div className="stats" style={style}>
      {children}
    </div>
  )
}

export function Badge({ tone = 'muted', children }: { tone?: Tone; children: ReactNode }) {
  return <span className={'badge ' + tone}>{children}</span>
}

export function Dot({ tone = 'muted', pulse = false }: { tone?: Tone; pulse?: boolean }) {
  return <span className={'dot ' + tone + (pulse ? ' pulse' : '')} aria-hidden="true" />
}

export function Bar({ pct, tone = 'accent' }: { pct: number; tone?: Tone }) {
  const w = Math.max(0, Math.min(100, isFinite(pct) ? pct : 0))
  return (
    <div className="bar" role="progressbar" aria-valuenow={Math.round(w)} aria-valuemin={0} aria-valuemax={100}>
      <i className={tone} style={{ width: w + '%' }} />
    </div>
  )
}

export function Empty({
  title = '暂无数据',
  hint,
  action,
}: {
  title?: ReactNode
  hint?: ReactNode
  action?: ReactNode
}) {
  return (
    <div className="empty">
      <div className="t">{title}</div>
      {hint && <div className="h2">{hint}</div>}
      {action && <div className="a">{action}</div>}
    </div>
  )
}

export function Skeleton({ rows = 3 }: { rows?: number }) {
  return (
    <div className="skeleton-wrap" aria-hidden="true">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="skeleton" />
      ))}
    </div>
  )
}

export function KV({ items }: { items: { k: ReactNode; v: ReactNode; mono?: boolean }[] }) {
  return (
    <dl className="kv">
      {items.map((it, i) => (
        <div key={i} className="kv-row">
          <dt>{it.k}</dt>
          <dd className={it.mono ? 'mono' : undefined}>{it.v}</dd>
        </div>
      ))}
    </dl>
  )
}

// ---------- 表单控件 ----------

export function Field({
  label,
  hint,
  error,
  children,
  inline = false,
  htmlFor,
}: {
  label?: ReactNode
  hint?: ReactNode
  error?: ReactNode
  children: ReactNode
  inline?: boolean
  htmlFor?: string
}) {
  return (
    <div className={'field' + (inline ? ' inline' : '')}>
      {label !== undefined && (
        <label htmlFor={htmlFor} className="lbl">
          {label}
        </label>
      )}
      <div className="ctl">{children}</div>
      {error ? <div className="err">{error}</div> : hint ? <div className="hint">{hint}</div> : null}
    </div>
  )
}

export function Switch({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label?: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      className={'switch' + (checked ? ' on' : '')}
      onClick={() => onChange(!checked)}
    />
  )
}

export function Segmented<T extends string | number>({
  value,
  options,
  onChange,
  ariaLabel,
}: {
  value: T
  options: { value: T; label: ReactNode }[]
  onChange: (v: T) => void
  ariaLabel?: string
}) {
  return (
    <div className="segmented" role="group" aria-label={ariaLabel}>
      {options.map((o) => (
        <button
          key={String(o.value)}
          type="button"
          className={o.value === value ? 'on' : ''}
          aria-pressed={o.value === value}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

export function Tabs<T extends string>({
  value,
  options,
  onChange,
}: {
  value: T
  options: { value: T; label: ReactNode; badge?: ReactNode }[]
  onChange: (v: T) => void
}) {
  return (
    <div className="tabs" role="tablist">
      {options.map((o) => (
        <button
          key={String(o.value)}
          type="button"
          role="tab"
          aria-selected={o.value === value}
          className={o.value === value ? 'on' : ''}
          onClick={() => onChange(o.value)}
        >
          {o.label}
          {o.badge !== undefined && <span className="b">{o.badge}</span>}
        </button>
      ))}
    </div>
  )
}

// ---------- 弹层 ----------

export function Modal({
  open,
  title,
  onClose,
  children,
  footer,
  wide = false,
  narrow = false,
  labelId = 'modal-title',
}: {
  open: boolean
  title?: ReactNode
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  wide?: boolean
  narrow?: boolean
  labelId?: string
}) {
  const panelRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation()
        onClose()
      }
    }
    document.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    const t = window.setTimeout(() => {
      const el = panelRef.current?.querySelector<HTMLElement>('input, select, textarea, button')
      el?.focus()
    }, 30)
    return () => {
      document.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
      window.clearTimeout(t)
    }
  }, [open, onClose])

  if (!open) return null
  return createPortal(
    <div
      className="overlay"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        ref={panelRef}
        className={'modal' + (wide ? ' wide' : '') + (narrow ? ' narrow' : '')}
        role="dialog"
        aria-modal="true"
        aria-labelledby={title ? labelId : undefined}
      >
        {title !== undefined && (
          <header>
            <h2 id={labelId} className="h">
              {title}
            </h2>
            <button type="button" className="btn ghost icon" onClick={onClose} aria-label="关闭">
              ✕
            </button>
          </header>
        )}
        <div className="body">{children}</div>
        {footer && <footer>{footer}</footer>}
      </div>
    </div>,
    document.body,
  )
}

export function ConfirmDialog({
  open,
  title,
  message,
  confirmText = '确认',
  danger = false,
  busy = false,
  onConfirm,
  onClose,
}: {
  open: boolean
  title: ReactNode
  message: ReactNode
  confirmText?: string
  danger?: boolean
  busy?: boolean
  onConfirm: () => void
  onClose: () => void
}) {
  return (
    <Modal
      open={open}
      title={title}
      onClose={onClose}
      narrow
      footer={
        <>
          <button type="button" className="btn ghost" onClick={onClose} disabled={busy}>
            取消
          </button>
          <button
            type="button"
            className={'btn ' + (danger ? 'danger' : 'primary')}
            onClick={onConfirm}
            disabled={busy}
          >
            {busy ? '处理中…' : confirmText}
          </button>
        </>
      }
    >
      <div className="confirm-msg">{message}</div>
    </Modal>
  )
}

// ---------- 交互小件 ----------

export function CopyText({ value, label = '复制', className = '' }: { value: string; label?: string; className?: string }) {
  const [done, setDone] = useState(false)
  const timer = useRef<number | undefined>(undefined)

  const copy = useCallback(async () => {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(value)
      } else {
        // 非安全上下文（http 内网访问）没有 clipboard API，退回 textarea 选中
        const ta = document.createElement('textarea')
        ta.value = value
        ta.style.position = 'fixed'
        ta.style.opacity = '0'
        document.body.appendChild(ta)
        ta.select()
        document.execCommand('copy')
        document.body.removeChild(ta)
      }
      setDone(true)
      window.clearTimeout(timer.current)
      timer.current = window.setTimeout(() => setDone(false), 1600)
    } catch {
      setDone(false)
    }
  }, [value])

  useEffect(() => () => window.clearTimeout(timer.current), [])

  return (
    <button type="button" className={'btn ghost sm ' + className} onClick={copy} title={value}>
      {done ? '已复制' : label}
    </button>
  )
}

export function Pager({
  page,
  pageSize,
  total,
  onPage,
  onPageSize,
}: {
  page: number
  pageSize: number
  total: number
  onPage: (p: number) => void
  onPageSize?: (n: number) => void
}) {
  const pages = Math.max(1, Math.ceil(total / Math.max(1, pageSize)))
  const nums = useMemo(() => {
    const out: number[] = []
    const push = (n: number) => {
      if (n >= 1 && n <= pages && !out.includes(n)) out.push(n)
    }
    push(1)
    push(page - 1)
    push(page)
    push(page + 1)
    push(pages)
    out.sort((a, b) => a - b)
    return out
  }, [page, pages])

  if (total <= 0) return null

  return (
    <div className="pager">
      <div className="row gap2 v-center">
        <span className="muted small">
          共 {total} 条 · 第 {page}/{pages} 页
        </span>
        {onPageSize && (
          <select
            className="select xs"
            aria-label="每页条数"
            value={pageSize}
            onChange={(e) => onPageSize(Number(e.target.value))}
          >
            {[10, 20, 50, 100].map((n) => (
              <option key={n} value={n}>
                {n} / 页
              </option>
            ))}
          </select>
        )}
      </div>
      <div className="pages">
        <button type="button" onClick={() => onPage(Math.max(1, page - 1))} disabled={page <= 1} aria-label="上一页">
          ‹
        </button>
        {nums.map((n, i) => {
          const prev = nums[i - 1]
          const gap = prev !== undefined && n - prev > 1
          return (
            <span key={n} className="row gap1 v-center">
              {gap && <span className="muted tiny">…</span>}
              <button type="button" className={n === page ? 'on' : ''} onClick={() => onPage(n)} aria-current={n === page}>
                {n}
              </button>
            </span>
          )
        })}
        <button
          type="button"
          onClick={() => onPage(Math.min(pages, page + 1))}
          disabled={page >= pages}
          aria-label="下一页"
        >
          ›
        </button>
      </div>
    </div>
  )
}

/** 只读的 JSON 预览（配置/详情用），超长截断。 */
export function Pre({ children, maxHeight = 320 }: { children: ReactNode; maxHeight?: number }) {
  return (
    <pre className="pre" style={{ maxHeight }}>
      {children}
    </pre>
  )
}
