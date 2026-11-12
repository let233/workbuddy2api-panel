// ui.tsx 通用展示组件：只做结构与可访问性，样式全部来自 styles/components.css 的类名。
import {
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
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

// ---------- 下拉选择 ----------

export type SelectOption = { value: string; label: ReactNode; disabled?: boolean }

// 同一时刻只允许一个自绘下拉/菜单展开：两个菜单叠着开既互相遮挡，Esc 又只关掉其中一个。
// 用模块级自定义事件互斥，省得为每个调用点包一层 Provider。
const DD_OPEN_EVENT = 'wb2api:ddopen'

// Select 是自绘下拉，替代 <select>：原生下拉在两处不能接受——
// ① 弹层里的 <select> 在移动端会弹系统滚轮，盖住页面且无法跟随主题；
// ② 原生 <option> 不能放结构与样式（计数、状态点、分组标题都做不了）。
// 交互按 WAI-ARIA combobox 走：焦点始终留在按钮上，用 aria-activedescendant 指当前项；
// 菜单 portal 到 body 用 fixed 定位，避免被 .box / .tbl-wrap 的 overflow 裁掉。
export function Select({
  value,
  options,
  onChange,
  ariaLabel,
  id,
  className = '',
  disabled = false,
  placeholder = '请选择',
  title,
  menuMaxHeight = 300,
}: {
  value: string
  options: SelectOption[]
  onChange: (v: string) => void
  ariaLabel?: string
  id?: string
  className?: string
  disabled?: boolean
  placeholder?: string
  title?: string
  menuMaxHeight?: number
}) {
  const [open, setOpen] = useState(false)
  const [active, setActive] = useState(0)
  const [rect, setRect] = useState<{ left: number; top: number; width: number; up: boolean; maxH: number } | null>(null)
  const btnRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement>(null)
  const autoId = useId().replace(/:/g, '')
  const listId = 'dd-list-' + autoId
  const curIdx = options.findIndex((o) => o.value === value)
  const current = curIdx >= 0 ? options[curIdx] : undefined

  const place = useCallback(() => {
    const b = btnRef.current
    if (!b) return
    const r = b.getBoundingClientRect()
    const gap = 4
    const below = window.innerHeight - r.bottom - gap - 8
    const above = r.top - gap - 8
    const up = below < Math.min(menuMaxHeight, 180) && above > below
    const maxH = Math.max(120, Math.min(menuMaxHeight, up ? above : below))
    const width = Math.max(r.width, 180)
    const left = Math.max(8, Math.min(r.left, window.innerWidth - width - 8))
    // 上翻时把菜单底边贴在按钮上方（靠 translateY(-100%) 上移），否则从按钮下方展开
    setRect({ left, top: up ? r.top - gap : r.bottom + gap, width, up, maxH })
  }, [menuMaxHeight])

  useEffect(() => {
    if (!open) return
    place()
    setActive(curIdx >= 0 ? curIdx : 0)
    const onDocDown = (e: MouseEvent) => {
      const t = e.target as Node
      if (btnRef.current?.contains(t) || menuRef.current?.contains(t)) return
      setOpen(false)
    }
    // 滚动容器里也要跟着按钮走，否则 fixed 菜单会和按钮脱开
    const onReposition = () => place()
    document.addEventListener('mousedown', onDocDown)
    window.addEventListener('resize', onReposition)
    window.addEventListener('scroll', onReposition, true)
    // 捕获阶段拦 Esc：弹层里的下拉只该关菜单，不该把整层 Modal 关掉
    const onEsc = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      e.preventDefault()
      setOpen(false)
      btnRef.current?.focus()
    }
    document.addEventListener('keydown', onEsc, true)
    // 打开时广播一次，其它实例（监听常驻）收到就收起
    document.dispatchEvent(new CustomEvent<string>(DD_OPEN_EVENT, { detail: autoId }))
    return () => {
      document.removeEventListener('mousedown', onDocDown)
      window.removeEventListener('resize', onReposition)
      window.removeEventListener('scroll', onReposition, true)
      document.removeEventListener('keydown', onEsc, true)
    }
    // curIdx 只在打开那一刻用来定初始高亮，故意不放进依赖
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, place])

  // 互斥监听必须常驻——关闭状态下不注册就收不到「别人打开了」的广播
  useEffect(() => {
    const onOtherOpen = (e: Event) => {
      if ((e as CustomEvent<string>).detail !== autoId) setOpen(false)
    }
    document.addEventListener(DD_OPEN_EVENT, onOtherOpen)
    return () => document.removeEventListener(DD_OPEN_EVENT, onOtherOpen)
  }, [autoId])

  // 打开后把高亮项滚进可视区
  useEffect(() => {
    if (!open) return
    const el = menuRef.current?.querySelector<HTMLElement>('[data-idx="' + active + '"]')
    el?.scrollIntoView({ block: 'nearest' })
  }, [open, active])

  const step = (delta: number) => {
    if (!options.length) return
    let i = active
    for (let n = 0; n < options.length; n++) {
      i = (i + delta + options.length) % options.length
      if (!options[i].disabled) break
    }
    setActive(i)
  }

  const commit = (idx: number) => {
    const o = options[idx]
    if (!o || o.disabled) return
    onChange(o.value)
    setOpen(false)
    btnRef.current?.focus()
  }

  const onKeyDown = (e: ReactKeyboardEvent<HTMLButtonElement>) => {
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowUp':
        e.preventDefault()
        if (!open) setOpen(true)
        else step(e.key === 'ArrowDown' ? 1 : -1)
        return
      case 'Home':
        if (open) {
          e.preventDefault()
          setActive(0)
        }
        return
      case 'End':
        if (open) {
          e.preventDefault()
          setActive(options.length - 1)
        }
        return
      case 'Enter':
      case ' ':
        e.preventDefault()
        if (!open) setOpen(true)
        else commit(active)
        return
      case 'Tab':
        if (open) setOpen(false)
        return
      default:
        // 首字母跳转：长列表（几百个账号/模型）靠键盘找比滚动快
        if (open && e.key.length === 1) {
          const ch = e.key.toLowerCase()
          const at = options.findIndex(
            (o, i) => i > active && !o.disabled && String(o.value).toLowerCase().startsWith(ch),
          )
          if (at >= 0) setActive(at)
        }
    }
  }

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        id={id}
        className={'select dd' + (open ? ' open' : '') + (className ? ' ' + className : '')}
        role="combobox"
        aria-label={ariaLabel}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-activedescendant={open ? listId + '-' + active : undefined}
        aria-disabled={disabled || undefined}
        disabled={disabled}
        title={title}
        onClick={() => !disabled && setOpen((v) => !v)}
        onKeyDown={onKeyDown}
      >
        <span className="dd-lbl">{current ? current.label : placeholder}</span>
        <svg className="dd-arrow" viewBox="0 0 10 6" aria-hidden="true">
          <path d="M1 1l4 4 4-4" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>
      {open && rect
        ? createPortal(
            <div
              ref={menuRef}
              className={'dd-menu' + (rect.up ? ' up' : '')}
              role="listbox"
              id={listId}
              aria-label={ariaLabel}
              style={{
                left: rect.left,
                top: rect.top,
                width: rect.width,
                maxHeight: rect.maxH,
                transform: rect.up ? 'translateY(-100%)' : undefined,
              }}
            >
              {options.map((o, i) => (
                <div
                  key={o.value + '#' + i}
                  data-idx={i}
                  id={listId + '-' + i}
                  role="option"
                  aria-selected={o.value === value}
                  aria-disabled={o.disabled || undefined}
                  className={
                    'dd-opt' + (o.value === value ? ' on' : '') + (i === active ? ' act' : '') + (o.disabled ? ' dis' : '')
                  }
                  onMouseEnter={() => !o.disabled && setActive(i)}
                  onMouseDown={(e) => e.preventDefault()}
                  onClick={() => commit(i)}
                >
                  <span className="dd-lbl">{o.label}</span>
                  {o.value === value && (
                    <svg className="dd-check" viewBox="0 0 12 10" aria-hidden="true">
                      <path d="M1 5l3.5 3.5L11 1.5" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
                    </svg>
                  )}
                </div>
              ))}
            </div>,
            document.body,
          )
        : null}
    </>
  )
}

// Menu 是行内「更多操作」菜单：同一套 portal + fixed 定位逻辑，避免表格
// .tbl-wrap 的 overflow 把下拉裁掉。用途是把详情弹层里的动作直接搬到列表行内。
export type MenuItem = {
  label: ReactNode
  onSelect: () => void
  disabled?: boolean
  danger?: boolean
  /** 在这一项之前画一条分隔线 */
  sep?: boolean
}

export function Menu({
  items,
  ariaLabel = '更多操作',
  title,
  className = '',
  align = 'right',
  trigger,
}: {
  items: MenuItem[]
  ariaLabel?: string
  title?: string
  className?: string
  align?: 'left' | 'right'
  trigger?: ReactNode
}) {
  const [open, setOpen] = useState(false)
  const [rect, setRect] = useState<{ left: number; top: number; up: boolean; maxH: number } | null>(null)
  const btnRef = useRef<HTMLButtonElement>(null)
  const menuRef = useRef<HTMLDivElement | null>(null)
  const autoId = 'menu' + useId().replace(/:/g, '')

  const place = useCallback(() => {
    const b = btnRef.current
    if (!b) return
    const r = b.getBoundingClientRect()
    const mw = menuRef.current?.offsetWidth ?? 176
    const gap = 4
    const below = window.innerHeight - r.bottom - gap - 8
    const above = r.top - gap - 8
    const up = below < 140 && above > below
    const left =
      align === 'right'
        ? Math.max(8, Math.min(r.right - mw, window.innerWidth - mw - 8))
        : Math.max(8, Math.min(r.left, window.innerWidth - mw - 8))
    setRect({ left, top: up ? r.top - gap : r.bottom + gap, up, maxH: Math.max(120, Math.min(280, up ? above : below)) })
  }, [align])

  useEffect(() => {
    if (!open) return
    place()
    const onDocDown = (e: MouseEvent) => {
      const t = e.target as Node
      if (btnRef.current?.contains(t) || menuRef.current?.contains(t)) return
      setOpen(false)
    }
    const onReposition = () => place()
    const onEsc = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return
      e.stopPropagation()
      e.preventDefault()
      setOpen(false)
      btnRef.current?.focus()
    }
    document.addEventListener('mousedown', onDocDown)
    window.addEventListener('resize', onReposition)
    window.addEventListener('scroll', onReposition, true)
    document.addEventListener('keydown', onEsc, true)
    // 与 Select 共用一套互斥：一次只展开一个菜单
    document.dispatchEvent(new CustomEvent<string>(DD_OPEN_EVENT, { detail: autoId }))
    return () => {
      document.removeEventListener('mousedown', onDocDown)
      window.removeEventListener('resize', onReposition)
      window.removeEventListener('scroll', onReposition, true)
      document.removeEventListener('keydown', onEsc, true)
    }
  }, [open, place])

  useEffect(() => {
    const onOtherOpen = (e: Event) => {
      if ((e as CustomEvent<string>).detail !== autoId) setOpen(false)
    }
    document.addEventListener(DD_OPEN_EVENT, onOtherOpen)
    return () => document.removeEventListener(DD_OPEN_EVENT, onOtherOpen)
  }, [autoId])

  // 首次 place 时菜单还没挂载，只能按兜底宽度估；挂载后拿到真实宽度立刻重摆，
  // 否则右对齐会按 176px 估宽、菜单右侧溢出视口几个像素。
  const setMenuEl = useCallback(
    (el: HTMLDivElement | null) => {
      menuRef.current = el
      if (el) place()
    },
    [place],
  )

  const pick = (it: MenuItem) => {
    if (it.disabled) return
    setOpen(false)
    it.onSelect()
  }

  const onKeyDown = (e: ReactKeyboardEvent<HTMLButtonElement>) => {
    if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      setOpen(true)
    }
  }

  return (
    <>
      <button
        ref={btnRef}
        type="button"
        className={'btn sm ghost' + (className ? ' ' + className : '')}
        aria-label={ariaLabel}
        aria-haspopup="menu"
        aria-expanded={open}
        title={title || ariaLabel}
        onClick={() => setOpen((v) => !v)}
        onKeyDown={onKeyDown}
        disabled={items.length === 0}
      >
        {trigger ?? (
          <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true" fill="currentColor">
            <circle cx="3" cy="8" r="1.4" />
            <circle cx="8" cy="8" r="1.4" />
            <circle cx="13" cy="8" r="1.4" />
          </svg>
        )}
      </button>
      {open && rect
        ? createPortal(
            <div
              ref={setMenuEl}
              className="dd-menu menu"
              role="menu"
              aria-label={ariaLabel}
              style={{
                left: rect.left,
                top: rect.top,
                maxHeight: rect.maxH,
                transform: rect.up ? 'translateY(-100%)' : undefined,
              }}
            >
              {items.map((it, i) => (
                <div key={i} role="none">
                  {it.sep ? <div className="dd-sep" role="separator" /> : null}
                  <div
                    role="menuitem"
                    tabIndex={-1}
                    aria-disabled={it.disabled || undefined}
                    className={'dd-opt' + (it.disabled ? ' dis' : '') + (it.danger ? ' danger' : '')}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => pick(it)}
                  >
                    <span className="dd-lbl">{it.label}</span>
                  </div>
                </div>
              ))}
            </div>,
            document.body,
          )
        : null}
    </>
  )
}

// ---------- 弹层 ----------

// 弹层可以叠着开（例如初始密码提示压在页面弹层上），滚动锁定必须计数，
// 否则关掉其中一个就把 body 解锁，底下的页面会跟着滚。
let bodyLockCount = 0
let bodyLockOriginal = ''
function lockBodyScroll() {
  if (bodyLockCount === 0) {
    bodyLockOriginal = document.body.style.overflow
    document.body.style.overflow = 'hidden'
  }
  bodyLockCount += 1
}
function unlockBodyScroll() {
  if (bodyLockCount === 0) return
  bodyLockCount -= 1
  if (bodyLockCount === 0) document.body.style.overflow = bodyLockOriginal
}

export function Modal({
  open,
  title,
  onClose,
  children,
  footer,
  wide = false,
  narrow = false,
  titleId: titleIdProp,
}: {
  open: boolean
  title?: ReactNode
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  wide?: boolean
  narrow?: boolean
  titleId?: string
}) {
  const panelRef = useRef<HTMLDivElement>(null)
  const autoId = useId()
  const titleId = titleIdProp ?? 'modal-' + autoId.replace(/:/g, '')
  // onClose 是调用方每次渲染都会新建的函数，不能进下面的依赖数组：
  // 否则每敲一个字符（受控输入 setState → 重渲染 → onClose 换身份）就会重跑一次
  // 副作用，把焦点的“30ms 后自动聚焦”重新排一遍，于是焦点被抢到 ✕ 上。
  const closeRef = useRef(onClose)
  useEffect(() => {
    closeRef.current = onClose
  })

  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation()
        closeRef.current()
      }
    }
    document.addEventListener('keydown', onKey)
    lockBodyScroll()
    // 只在“打开”这一瞬间聚焦一次，且优先落在表单控件上；
    // 没有表单控件时聚焦面板本身（tabIndex=-1），绝不退化成聚焦关闭按钮。
    const t = window.setTimeout(() => {
      const panel = panelRef.current
      if (!panel || panel.contains(document.activeElement)) return
      const field =
        panel.querySelector<HTMLElement>(
          'input:not([type="hidden"]):not([type="checkbox"]):not([type="radio"]):not([disabled])',
        ) ??
        panel.querySelector<HTMLElement>('select:not([disabled]), textarea:not([disabled])')
      ;(field ?? panel).focus({ preventScroll: true })
    }, 30)
    return () => {
      document.removeEventListener('keydown', onKey)
      unlockBodyScroll()
      window.clearTimeout(t)
    }
  }, [open])

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
        aria-labelledby={title ? titleId : undefined}
        tabIndex={-1}
      >
        {title !== undefined && (
          <header>
            <h2 id={titleId} className="h">
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
          <Select
            className="xs"
            ariaLabel="每页条数"
            value={String(pageSize)}
            onChange={(v) => onPageSize(Number(v))}
            menuMaxHeight={220}
            options={[10, 20, 50, 100].map((n) => ({ value: String(n), label: n + ' / 页' }))}
          />
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
