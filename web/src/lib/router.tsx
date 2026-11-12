// router.tsx 极简 hash 路由：面板是单页后台，用 hash 而不是 history 路由，
// 免掉服务端为每个子路径做 fallback（FileServer 只需要吐 index.html 一次）。
import { useCallback, useSyncExternalStore } from 'react'

type Listener = () => void
const listeners = new Set<Listener>()

function emit() {
  for (const l of listeners) l()
}

function subscribe(cb: Listener): () => void {
  listeners.add(cb)
  window.addEventListener('hashchange', emit)
  return () => {
    listeners.delete(cb)
    if (listeners.size === 0) window.removeEventListener('hashchange', emit)
  }
}

function readHash(): string {
  const raw = window.location.hash.replace(/^#\/?/, '')
  return raw
}

function snapshot(): string {
  return readHash()
}

export type Route = {
  /** 视图名，如 'accounts' */
  name: string
  /** hash 里的查询串（#usage?dim=account） */
  query: URLSearchParams
}

function parse(raw: string): Route {
  const q = raw.indexOf('?')
  const name = (q >= 0 ? raw.slice(0, q) : raw).trim() || 'overview'
  return { name, query: new URLSearchParams(q >= 0 ? raw.slice(q + 1) : '') }
}

export function useRoute(): Route {
  const raw = useSyncExternalStore(subscribe, snapshot, () => 'overview')
  return parse(raw)
}

export function navigate(name: string, query?: Record<string, string | number | boolean | undefined>) {
  let hash = '#' + name
  if (query) {
    const usp = new URLSearchParams()
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined || v === '') continue
      usp.set(k, String(v))
    }
    const s = usp.toString()
    if (s) hash += '?' + s
  }
  if (window.location.hash === hash) emit()
  else window.location.hash = hash
}

/** 替换当前 hash，不新增历史记录（用于把默认参数写回地址栏）。 */
export function replaceRoute(name: string) {
  const target = '#/' + name
  if (window.location.hash !== target) window.history.replaceState(null, '', target)
}

export function useNavigate() {
  return useCallback(navigate, [])
}
