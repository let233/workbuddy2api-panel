// Login.tsx 登录层：只有未认证时才渲染，登录成功后整个应用切到主界面。
import { useState, type FormEvent } from 'react'
import { useSession } from '../lib/session'
import { ApiError } from '../lib/api'

export function Login() {
  const { login, defaultCredentials } = useSession()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    if (busy) return
    setErr('')
    setBusy(true)
    try {
      await login(username.trim(), password)
    } catch (e2) {
      if (e2 instanceof ApiError) {
        setErr(
          e2.status === 429
            ? '尝试过于频繁，请一分钟后再试'
            : e2.code === 'invalid_credentials'
              ? '用户名或密码错误'
              : e2.code || '登录失败',
        )
      } else {
        setErr(e2 instanceof Error ? e2.message : '登录失败')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="loginpage">
      <form className="logincard" onSubmit={submit}>
        <div className="loginlogo" aria-hidden="true">
          WB
        </div>
        <h1>WorkBuddy2API</h1>
        <p className="sub">登录面板以管理账号池与密钥</p>

        {defaultCredentials && (
          <div className="notice warn" role="status">
            当前仍是初始密码，登录后请先修改。
          </div>
        )}

        <label className="field">
          <span className="lbl">用户名</span>
          <input
            className="input"
            value={username}
            autoComplete="username"
            autoFocus
            onChange={(e) => setUsername(e.target.value)}
            placeholder="请输入用户名"
          />
        </label>

        <label className="field">
          <span className="lbl">密码</span>
          <input
            className="input"
            type="password"
            value={password}
            autoComplete="current-password"
            onChange={(e) => setPassword(e.target.value)}
            placeholder="请输入密码"
          />
        </label>

        {err && (
          <div className="formerr" role="alert">
            {err}
          </div>
        )}

        <button type="submit" className="btn primary block lg" disabled={busy || !username || !password}>
          {busy ? '登录中…' : '登录'}
        </button>

        <p className="tiny muted center">会话保存在 HttpOnly Cookie 中，前端不存储密码。</p>
      </form>
    </div>
  )
}
