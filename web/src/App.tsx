// App.tsx 应用入口：先探会话，未登录给登录层，登录后套外壳再按 hash 选视图。
import { lazy, Suspense } from 'react'
import type { ComponentType } from 'react'
import { AppShell } from './components/AppShell'
import { Login } from './views/Login'
import { Skeleton } from './components/ui'
import { useRoute, navigate } from './lib/router'
import { useSession } from './lib/session'
import { DEFAULT_ROUTE } from './components/nav'
import './views/views.css'

// 各视图按需加载：首屏只下载概览那一块，13 个后台页各自成 chunk。
const Overview = lazy(() => import('./views/Overview'))
const Accounts = lazy(() => import('./views/Accounts'))
const Expiry = lazy(() => import('./views/Expiry'))
const Usage = lazy(() => import('./views/Usage'))
const BalHist = lazy(() => import('./views/BalHist'))
const Packages = lazy(() => import('./views/Packages'))
const ApiKeys = lazy(() => import('./views/ApiKeys'))
const Models = lazy(() => import('./views/Models'))
const Proxies = lazy(() => import('./views/Proxies'))
const TasksCenter = lazy(() => import('./views/TasksCenter'))
const Events = lazy(() => import('./views/Events'))
const Logs = lazy(() => import('./views/Logs'))
const Config = lazy(() => import('./views/Config'))
const About = lazy(() => import('./views/About'))

// 视图全是 lazy 组件：用 ComponentType 收口，避免 LazyExoticComponent 与函数签名对不上。
const VIEWS: Record<string, ComponentType> = {
  overview: Overview,
  accounts: Accounts,
  expiry: Expiry,
  usage: Usage,
  balhist: BalHist,
  packages: Packages,
  apikeys: ApiKeys,
  models: Models,
  proxies: Proxies,
  taskscenter: TasksCenter,
  events: Events,
  logs: Logs,
  config: Config,
  about: About,
}

function View() {
  const route = useRoute()
  const name = VIEWS[route.name] ? route.name : DEFAULT_ROUTE
  const Cmp = VIEWS[name]
  return (
    <Suspense fallback={<div className="stack"><Skeleton rows={5} /></div>}>
      <Cmp />
    </Suspense>
  )
}

function NotFound() {
  return (
    <div className="box">
      <div className="body">
        <div className="empty">
          <div className="t">页面不存在</div>
          <button type="button" className="btn primary" onClick={() => navigate(DEFAULT_ROUTE)}>
            回到概览
          </button>
        </div>
      </div>
    </div>
  )
}

export function App() {
  const { authenticated, probing } = useSession()
  const route = useRoute()

  if (probing) {
    return (
      <div className="boot">
        <div className="bootspin" aria-label="加载中" />
      </div>
    )
  }

  if (!authenticated) return <Login />

  const known = Boolean(VIEWS[route.name])
  return <AppShell>{known ? <View /> : <NotFound />}</AppShell>
}
