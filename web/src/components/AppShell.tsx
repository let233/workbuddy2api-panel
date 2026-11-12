// AppShell.tsx 整体外壳：左侧导航（窄屏变抽屉）、顶部栏（标题/刷新/自动刷新/主题/账号）、内容区。
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { NAV, NAV_GROUPS, navItem, DEFAULT_ROUTE } from './nav'
import type { NavIcon } from './nav'
import { useRoute } from '../lib/router'
import { useRefresh } from '../lib/refresh'
import { useSession } from '../lib/session'
import { useTheme, type ThemeMode } from '../lib/theme'
import { useQuery } from '../lib/hooks'
import { api } from '../lib/api'
import { Field, Modal, Select } from './ui'

// ---------- 图标 ----------

const ICONS: Record<NavIcon, ReactNode> = {
  gauge: (
    <>
      <path d="M2.6 11.6a6 6 0 1 1 10.8 0" />
      <path d="M8 8.8 10.9 6.6" />
      <circle cx="8" cy="9.4" r="0.9" fill="currentColor" stroke="none" />
    </>
  ),
  users: (
    <>
      <circle cx="8" cy="5.6" r="2.6" />
      <path d="M2.8 13.4c.6-2.6 2.7-3.9 5.2-3.9s4.6 1.3 5.2 3.9" />
    </>
  ),
  clock: (
    <>
      <circle cx="8" cy="8" r="6" />
      <path d="M8 4.6V8l2.4 1.6" />
    </>
  ),
  chart: (
    <>
      <path d="M2 13.5h12" />
      <path d="M4.6 13.5V8.4M8 13.5V3.6M11.4 13.5v-3.2" />
    </>
  ),
  key: (
    <>
      <circle cx="5.6" cy="8" r="2.9" />
      <path d="M8.5 8h5M11.3 8v2M13.1 8v1.4" />
    </>
  ),
  box: (
    <>
      <path d="M2.3 5.5 8 2.6l5.7 2.9v5L8 13.4l-5.7-2.9z" />
      <path d="M2.3 5.5 8 8.4l5.7-2.9M8 8.4v5" />
    </>
  ),
  trend: (
    <>
      <path d="M2 12.2 5.8 7.6l2.7 2.2 4.4-4.6" />
      <path d="M10.6 5.2h2.6v2.6" />
    </>
  ),
  tasks: (
    <>
      <path d="M3 2.6h10v9H8l-3 2.8V11.6H3z" />
      <path d="M6 6.4h4M6 8.8h2.6" />
    </>
  ),
  cpu: (
    <>
      <rect x="4.4" y="4.4" width="7.2" height="7.2" rx="1.4" />
      <path d="M6.6 2v2M9.4 2v2M6.6 12v2M9.4 12v2M2 6.6h2M2 9.4h2M12 6.6h2M12 9.4h2" />
    </>
  ),
  net: (
    <>
      <rect x="5.8" y="2" width="4.4" height="4.2" rx="1" />
      <rect x="1.6" y="10" width="4.4" height="4.2" rx="1" />
      <rect x="10" y="10" width="4.4" height="4.2" rx="1" />
      <path d="M8 6.2v2.2M3.8 10V8.4h8.4V10" />
    </>
  ),
  settings: (
    <>
      <circle cx="8" cy="8" r="2.2" />
      <path d="M8 1.8v1.8M8 12.4v1.8M1.8 8h1.8M12.4 8h1.8M3.6 3.6l1.3 1.3M11.1 11.1l1.3 1.3M12.4 3.6l-1.3 1.3M4.9 11.1l-1.3 1.3" />
    </>
  ),
  feed: (
    <>
      <rect x="2.2" y="2.2" width="11.6" height="11.6" rx="2.4" />
      <path d="M5.2 5.6h5.6M5.2 8h5.6M5.2 10.4h3.2" />
    </>
  ),
  list: (
    <>
      <path d="M2.6 4h10.8M2.6 8h10.8M2.6 12h6.6" />
    </>
  ),
  info: (
    <>
      <circle cx="8" cy="8" r="6" />
      <path d="M8 7.2v4M8 5.1h0.01" />
    </>
  ),
}

export function Icon({ name, size = 16 }: { name: NavIcon; size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {ICONS[name]}
    </svg>
  )
}

// ---------- 顶部栏小件 ----------

const THEME_LABEL: Record<ThemeMode, string> = { auto: '跟随系统', light: '浅色', dark: '深色' }
const THEME_GLYPH: Record<ThemeMode, string> = { auto: 'A', light: '☀', dark: '☾' }

const AUTO_OPTIONS = [
  { ms: 0, label: '不自动刷新' },
  { ms: 10000, label: '每 10 秒' },
  { ms: 30000, label: '每 30 秒' },
  { ms: 60000, label: '每 1 分钟' },
]

type OverviewPeek = {
  version?: string
  redis_mode?: string
  healthy?: number
  total?: number
  cooling?: number
  update?: { available?: boolean; latest?: string } | null
}

export function AppShell({ children }: { children: ReactNode }) {
  const route = useRoute()
  const item = navItem(route.name)
  const { tick, refresh, interval, setInterval } = useRefresh()
  const { mode, setMode, cycle } = useTheme()
  const session = useSession()
  const [drawer, setDrawer] = useState(false)
  const [menu, setMenu] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const [pwOpen, setPwOpen] = useState(false)
  // 初始密码提示只做“每次打开页面提示一次”：关掉后本次会话不再弹，
  // 刷新页面组件重新挂载 → 状态归零 → 再次提示。故意不写 localStorage。
  const [pwDeferred, setPwDeferred] = useState(false)

  // 路由变化时收起抽屉与菜单
  useEffect(() => {
    setDrawer(false)
    setMenu(false)
  }, [route.name])

  // 点击外部关闭账号菜单
  useEffect(() => {
    if (!menu) return
    const onDown = (e: MouseEvent) => {
      if (!menuRef.current?.contains(e.target as Node)) setMenu(false)
    }
    document.addEventListener('mousedown', onDown)
    return () => document.removeEventListener('mousedown', onDown)
  }, [menu])

  const { data: peek } = useQuery<OverviewPeek>((signal) => api.get('/overview', undefined, signal), [tick], {
    poll: 20000,
  })

  const active = NAV.some((n) => n.name === route.name) ? route.name : DEFAULT_ROUTE
  void active

  return (
    <div className="shell">
      <nav className={'sidebar' + (drawer ? ' drawer open' : '')} aria-label="主导航">
        <div className="brand">
          <div className="logo" aria-hidden="true">
            WB
          </div>
          <div className="grow">
            <div className="name">WorkBuddy2API</div>
            <div className="sub">
              {peek?.version || 'dev'}
              {peek?.update?.available ? <span className="badge accent ml1">有更新</span> : null}
            </div>
          </div>
          <button
            type="button"
            className="btn ghost icon only-sm"
            aria-label="关闭导航"
            onClick={() => setDrawer(false)}
          >
            ✕
          </button>
        </div>

        <div className="navscroll">
          {NAV_GROUPS.map((group) => (
            <div key={group} className="navgroup">
              <div className="navgroup-h">{group}</div>
              <ul className="navlist">
                {NAV.filter((n) => n.group === group).map((n) => (
                  <li key={n.name}>
                    <a
                      href={'#/' + n.name}
                      className={n.name === route.name ? 'on' : ''}
                      aria-current={n.name === route.name ? 'page' : undefined}
                      onClick={() => setDrawer(false)}
                    >
                      <Icon name={n.icon} />
                      <span>{n.label}</span>
                    </a>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>

        <div className="navfoot">
          <div className="row gap2 v-center">
            <span className={'dot ' + ((peek?.healthy ?? 0) > 0 ? 'ok' : 'bad') + ' pulse'} aria-hidden="true" />
            <span className="small">
              健康 {peek?.healthy ?? '-'} / {peek?.total ?? '-'}
              {peek?.cooling ? ' · 冷却 ' + peek.cooling : ''}
            </span>
          </div>
          <div className="row gap1 v-center tiny muted">
            <span>{peek?.redis_mode ? 'Redis ' + peek.redis_mode : '内存模式'}</span>
          </div>
        </div>
      </nav>

      {drawer && <div className="drawer-scrim show" onClick={() => setDrawer(false)} aria-hidden="true" />}

      <div className="main">
        <header className="topbar">
          <button
            type="button"
            className="btn ghost icon only-sm"
            aria-label="打开导航"
            aria-expanded={drawer}
            onClick={() => setDrawer(true)}
          >
            <svg width="18" height="18" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5">
              <path d="M2.5 4h11M2.5 8h11M2.5 12h11" strokeLinecap="round" />
            </svg>
          </button>

          <div className="titlebar">
            <h1>{item?.label || '概览'}</h1>
            <div className="desc hide-sm">{item?.desc}</div>
          </div>

          <div className="row gap1 v-center ml-auto">
            <Select
              className="xs hide-sm"
              ariaLabel="自动刷新频率"
              value={String(interval)}
              onChange={(v) => setInterval(Number(v))}
              menuMaxHeight={220}
              title="自动刷新频率"
              options={AUTO_OPTIONS.map((o) => ({ value: String(o.ms), label: o.label }))}
            />

            <button type="button" className="btn ghost icon" aria-label="刷新数据" title="刷新" onClick={refresh}>
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.4">
                <path d="M13 8a5 5 0 1 1-1.6-3.7" strokeLinecap="round" />
                <path d="M13 2.6v2.6h-2.6" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
            </button>

            <button
              type="button"
              className="btn ghost icon"
              aria-label={'主题：' + THEME_LABEL[mode]}
              title={'主题：' + THEME_LABEL[mode] + '（点击切换）'}
              onClick={cycle}
            >
              <span className="theme-glyph">{THEME_GLYPH[mode]}</span>
            </button>

            <div className="usermenu" ref={menuRef}>
              <button
                type="button"
                className="btn ghost sm userbtn"
                aria-haspopup="menu"
                aria-expanded={menu}
                onClick={() => setMenu((v) => !v)}
              >
                <span className="avatar" aria-hidden="true">
                  {(session.username || 'U').slice(0, 1).toUpperCase()}
                </span>
                <span className="hide-sm">{session.username || '未登录'}</span>
              </button>
              {menu && (
                <div className="menu" role="menu">
                  <button
                    type="button"
                    role="menuitem"
                    onClick={() => {
                      setMenu(false)
                      setPwOpen(true)
                    }}
                  >
                    修改密码
                  </button>
                  <button
                    type="button"
                    role="menuitem"
                    onClick={() => {
                      setMenu(false)
                      setMode(mode === 'auto' ? 'dark' : 'auto')
                    }}
                  >
                    主题：{THEME_LABEL[mode]}
                  </button>
                  <button
                    type="button"
                    role="menuitem"
                    className="danger"
                    onClick={() => {
                      setMenu(false)
                      void session.logout()
                    }}
                  >
                    退出登录
                  </button>
                </div>
              )}
            </div>
          </div>
        </header>

        <main className="content" id="content">
          {children}
        </main>
      </div>

      <PasswordModal
        open={pwOpen || (session.mustChange && !pwDeferred)}
        forced={session.mustChange}
        onClose={() => {
          setPwOpen(false)
          if (session.mustChange) setPwDeferred(true)
        }}
      />
    </div>
  )
}

// 密码修改弹层：初始密码未改时自动弹出（forced），但允许关闭，
// 关闭后本次会话不再打扰，刷新页面会再次提示。账号菜单里也能随时重开。
function PasswordModal({ open, forced, onClose }: { open: boolean; forced: boolean; onClose: () => void }) {
  const { changePassword } = useSession()
  const [oldPw, setOldPw] = useState('')
  const [newPw, setNewPw] = useState('')
  const [again, setAgain] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (open) {
      setOldPw('')
      setNewPw('')
      setAgain('')
      setErr('')
    }
  }, [open])

  const submit = async () => {
    setErr('')
    if (newPw.length < 8) {
      setErr('新密码至少 8 位')
      return
    }
    if (newPw !== again) {
      setErr('两次输入的新密码不一致')
      return
    }
    setBusy(true)
    try {
      await changePassword(oldPw, newPw)
      onClose()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal
      open={open}
      title={forced ? '建议修改初始密码' : '修改密码'}
      onClose={onClose}
      narrow
      footer={
        <>
          <button type="button" className="btn ghost" onClick={onClose} disabled={busy}>
            {forced ? '稍后修改' : '取消'}
          </button>
          <button type="button" className="btn primary" onClick={submit} disabled={busy}>
            {busy ? '提交中…' : '确认修改'}
          </button>
        </>
      }
    >
      <div className="stack">
        {forced && (
          <p className="small warn-text">
            当前账号仍在使用初始密码，建议立即修改。可以稍后再改，刷新页面时会再次提示。
          </p>
        )}
        <Field label="当前密码" htmlFor="pw-old">
          <input id="pw-old" className="input" type="password" value={oldPw} autoComplete="current-password" onChange={(e) => setOldPw(e.target.value)} />
        </Field>
        <Field label="新密码" hint="至少 8 位" htmlFor="pw-new">
          <input id="pw-new" className="input" type="password" value={newPw} autoComplete="new-password" onChange={(e) => setNewPw(e.target.value)} />
        </Field>
        <Field label="确认新密码" htmlFor="pw-again">
          <input id="pw-again" className="input" type="password" value={again} autoComplete="new-password" onChange={(e) => setAgain(e.target.value)} />
        </Field>
        {err && <div className="formerr" role="alert">{err}</div>}
      </div>
    </Modal>
  )
}
