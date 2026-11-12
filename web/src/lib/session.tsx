// session.tsx 登录态：一个会话一个 Provider，401 全局回落成未登录。
// 面板用 HttpOnly Cookie 会话，前端不存 token，只维护「现在是不是已登录」这一份状态。
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import { api, onUnauthorized } from './api'

export type SessionInfo = {
  authenticated: boolean
  username: string
  isAdmin: boolean
  mustChange: boolean
  defaultCredentials: boolean
  loginRequired: boolean
}

type SessionApi = SessionInfo & {
  /** 首次会话探测还没回来 */
  probing: boolean
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
  changePassword: (oldPassword: string, newPassword: string) => Promise<void>
  refresh: () => Promise<void>
}

type RawSession = {
  authenticated?: boolean
  username?: string
  is_admin?: boolean
  must_change_password?: boolean
  default_credentials?: boolean
  login_required?: boolean
}

function pick(raw: RawSession): SessionInfo {
  return {
    authenticated: Boolean(raw.authenticated),
    username: raw.username || '',
    isAdmin: Boolean(raw.is_admin),
    mustChange: Boolean(raw.must_change_password),
    defaultCredentials: Boolean(raw.default_credentials),
    loginRequired: raw.login_required !== false,
  }
}

const EMPTY: SessionInfo = {
  authenticated: false,
  username: '',
  isAdmin: false,
  mustChange: false,
  defaultCredentials: false,
  loginRequired: true,
}

const Ctx = createContext<SessionApi | null>(null)

export function SessionProvider({ children }: { children: ReactNode }) {
  const [info, setInfo] = useState<SessionInfo>(EMPTY)
  const [probing, setProbing] = useState(true)

  const refresh = useCallback(async () => {
    try {
      const raw = await api.get<RawSession>('/auth/session')
      setInfo(pick(raw))
    } catch {
      setInfo(EMPTY)
    } finally {
      setProbing(false)
    }
  }, [])

  useEffect(() => {
    void refresh()
  }, [refresh])

  // 任何接口 401 都说明会话没了：直接翻到登录层，不要各自弹错
  useEffect(
    () =>
      onUnauthorized(() => {
        setInfo((prev) => ({ ...prev, authenticated: false }))
        setProbing(false)
      }),
    [],
  )

  const login = useCallback(async (username: string, password: string) => {
    const raw = await api.post<RawSession>('/auth/login', { username, password })
    setInfo(pick(raw))
    setProbing(false)
  }, [])

  const logout = useCallback(async () => {
    try {
      await api.post('/auth/logout')
    } finally {
      setInfo((prev) => ({ ...prev, authenticated: false }))
    }
  }, [])

  const changePassword = useCallback(async (oldPassword: string, newPassword: string) => {
    const raw = await api.post<RawSession>('/auth/password', {
      old_password: oldPassword,
      new_password: newPassword,
    })
    setInfo((prev) => ({ ...prev, ...pick(raw), mustChange: false }))
  }, [])

  const value = useMemo<SessionApi>(
    () => ({ ...info, probing, login, logout, changePassword, refresh }),
    [info, probing, login, logout, changePassword, refresh],
  )

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useSession(): SessionApi {
  const v = useContext(Ctx)
  if (!v) throw new Error('useSession 必须在 SessionProvider 内使用')
  return v
}
