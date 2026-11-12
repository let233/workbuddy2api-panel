// api.ts 统一的面板 API 客户端。
// 约定：/panel/api/* 全部同源、靠 HttpOnly Cookie 鉴权，因此前端不需要也不存放任何令牌。
// 401 单独走 onUnauthorized 回调（把登录层推上来），其他错误抛出 ApiError 由调用方决定提示方式。

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  constructor(status: number, code: string) {
    super(code)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export type Query = Record<string, string | number | boolean | undefined | null>

export function qs(params?: Query): string {
  if (!params) return ''
  const usp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === '') continue
    usp.set(k, String(v))
  }
  const s = usp.toString()
  return s ? '?' + s : ''
}

let unauthorizedHandler: (() => void) | null = null

/** 注册 401 处理（登录层）。返回注销函数，便于组件卸载。 */
export function onUnauthorized(fn: () => void): () => void {
  unauthorizedHandler = fn
  return () => {
    if (unauthorizedHandler === fn) unauthorizedHandler = null
  }
}

type Options = {
  method?: string
  body?: unknown
  signal?: AbortSignal
  /** 显式指定原始 body（如 FormData），此时忽略 body 的 JSON 序列化 */
  raw?: BodyInit
  headers?: Record<string, string>
}

async function request<T>(path: string, opts: Options = {}): Promise<T> {
  const { method = 'GET', body, raw, signal, headers = {} } = opts
  const init: RequestInit = { method, signal, credentials: 'same-origin', headers: { ...headers } }
  if (raw !== undefined) {
    init.body = raw
  } else if (body !== undefined) {
    ;(init.headers as Record<string, string>)['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }

  let res: Response
  try {
    res = await fetch(path.startsWith('/') ? path : '/panel/api' + path, init)
  } catch (e) {
    if ((e as Error)?.name === 'AbortError') throw e
    throw new ApiError(0, '网络请求失败：' + ((e as Error)?.message || '连接中断'))
  }

  if (res.status === 401) {
    unauthorizedHandler?.()
    throw new ApiError(401, 'login_required')
  }

  const text = await res.text()
  let data: unknown = undefined
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }

  if (!res.ok) {
    const obj = data as { error?: string; message?: string } | undefined
    const msg = obj?.error || obj?.message || (typeof data === 'string' ? data : '') || res.statusText
    throw new ApiError(res.status, String(msg || 'HTTP ' + res.status))
  }
  return data as T
}

/** GET/POST 到 /panel/api 下的接口。path 形如 '/overview'。 */
export const api = {
  get<T = unknown>(path: string, query?: Query, signal?: AbortSignal): Promise<T> {
    return request<T>('/panel/api' + path + qs(query), { signal })
  },
  post<T = unknown>(path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    return request<T>('/panel/api' + path, { method: 'POST', body: body ?? {}, signal })
  },
  put<T = unknown>(path: string, body?: unknown): Promise<T> {
    return request<T>('/panel/api' + path, { method: 'PUT', body: body ?? {} })
  },
  /** PATCH：局部更新（代理池节点、账号绑定代理等）。 */
  patch<T = unknown>(path: string, body?: unknown): Promise<T> {
    return request<T>('/panel/api' + path, { method: 'PATCH', body: body ?? {} })
  },
  del<T = unknown>(path: string, body?: unknown): Promise<T> {
    return request<T>('/panel/api' + path, { method: 'DELETE', body: body ?? {} })
  },
  /** 上传（multipart）：服务端按字段名解析，前端只管照原样塞进去。 */
  form<T = unknown>(path: string, form: FormData): Promise<T> {
    return request<T>('/panel/api' + path, { method: 'POST', raw: form })
  },
  /** 下载地址：cookie 鉴权，直接交给 <a download> 或 location。 */
  downloadUrl(path: string, query?: Query): string {
    return '/panel/api' + path + qs(query)
  },
}

export function errorText(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 401) return '登录已失效，请重新登录'
    return e.code || ('HTTP ' + e.status)
  }
  if (e instanceof Error) return e.message
  return String(e)
}
