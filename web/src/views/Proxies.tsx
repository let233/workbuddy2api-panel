// Proxies.tsx 代理池：节点列表 + 导入/编辑/删除 + 单测与全测 + 健康检测策略。
// 写操作全部走后端既有接口，body 字段严格按 internal/panel/proxies.go 的解析（DisallowUnknownFields）：
//   POST   /proxies            { lines }
//   PATCH  /proxies/{id}       { name, url, password, enabled }
//   DELETE /proxies/{id}
//   POST   /proxies/check      全量排队（202）
//   POST   /proxies/{id}/check 单节点排队（202）
//   PUT    /proxies/health     { url, interval_seconds, timeout_seconds, failure_threshold, success_threshold }
import { useMemo, useState } from 'react'
import { ApiError, api, errorText } from '../lib/api'
import { ago, clock } from '../lib/format'
import { useBusy, useDebounced, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { DataTable, type Column } from '../components/DataTable'
import {
  Badge,
  Box,
  ConfirmDialog,
  Field,
  KV,
  Modal,
  Pager,
  Segmented,
  Skeleton,
  Stat,
  StatsGrid,
  Switch,
  type Tone,
} from '../components/ui'

type ProxyView = {
  id: string
  name: string
  url: string
  protocol: string
  password_set: boolean
  enabled: boolean
  /** healthy | unhealthy | pending | disabled */
  status: string
  checking: boolean
  latency_ms: number
  checked_at: string
  last_error: string
  failures: number
  successes: number
  bound_count: number
}

type Binding = { mode: string; proxy_id?: string }

type HealthSettings = {
  url: string
  interval_seconds: number
  timeout_seconds: number
  failure_threshold: number
  success_threshold: number
}

type Snapshot = {
  proxies: ProxyView[] | null
  bindings: Record<string, Binding> | null
  realms: Record<string, Binding> | null
  health: HealthSettings
}

type ImportResult = { line: number; status: string; id?: string; error?: string; name?: string }
type ImportResp = { ok: boolean; results: ImportResult[] }

type StatusFilter = 'all' | 'healthy' | 'unhealthy' | 'pending' | 'disabled'

const STATUS: Record<string, { text: string; tone: Tone }> = {
  healthy: { text: '可用', tone: 'ok' },
  unhealthy: { text: '失败', tone: 'bad' },
  pending: { text: '待检测', tone: 'muted' },
  disabled: { text: '已停用', tone: 'muted' },
}

const IMPORT_STATUS: Record<string, { text: string; tone: Tone }> = {
  added: { text: '新增', tone: 'ok' },
  duplicate: { text: '重复', tone: 'muted' },
  invalid: { text: '无效', tone: 'bad' },
  unsupported: { text: '不支持', tone: 'warn' },
}

const MODE_TEXT: Record<string, string> = {
  random: '随机池',
  direct: '直连',
  fixed: '固定节点',
}

const DEFAULT_HEALTH: HealthSettings = {
  url: 'https://www.workbuddy.cn/',
  interval_seconds: 60,
  timeout_seconds: 10,
  failure_threshold: 3,
  success_threshold: 2,
}

type EditForm = { id: string; name: string; url: string; password: string; enabled: boolean }
type HealthForm = { url: string; interval: string; timeout: string; fail: string; success: string }

function maskUrl(u: string, hasPwd: boolean): string {
  if (!hasPwd) return u
  const at = u.lastIndexOf('@')
  if (at < 0) return u
  return u.slice(0, at) + ':***' + u.slice(at)
}

function hostOf(u: string): string {
  const i = u.indexOf('://')
  const rest = i >= 0 ? u.slice(i + 3) : u
  const at = rest.lastIndexOf('@')
  const hostPort = at >= 0 ? rest.slice(at + 1) : rest
  return hostPort.replace(/\/$/, '')
}

function byteLen(s: string): number {
  return new TextEncoder().encode(s).length
}

function latencyTone(ms: number): Tone {
  if (ms <= 0) return 'muted'
  if (ms < 300) return 'ok'
  if (ms < 800) return 'warn'
  return 'bad'
}

export default function Proxies() {
  const { tick } = useRefresh()
  const toast = useToast()
  const { busy, run } = useBusy()

  // 检测是后端异步排队的（202），轮询让「测试 → 状态变化」自动出现，不用手点刷新。
  const q = useQuery<Snapshot | null>((signal) => api.get<Snapshot | null>('/proxies', undefined, signal), [tick], {
    poll: 5000,
  })

  const [search, setSearch] = useState('')
  const debounced = useDebounced(search, 200)
  const [status, setStatus] = useState<StatusFilter>('all')

  const [form, setForm] = useState<EditForm | null>(null)
  const [formErr, setFormErr] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [addText, setAddText] = useState('')
  const [imported, setImported] = useState<ImportResult[] | null>(null)
  const [del, setDel] = useState<ProxyView | null>(null)
  const [hForm, setHForm] = useState<HealthForm | null>(null)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)

  const snap = q.data ?? null
  const proxies = useMemo(() => snap?.proxies ?? [], [snap])
  const bindings = snap?.bindings ?? {}
  const realms = snap?.realms ?? {}
  const health = snap?.health ?? DEFAULT_HEALTH

  const stats = useMemo(() => {
    let ok = 0
    let bad = 0
    let pending = 0
    let off = 0
    let latSum = 0
    let latCount = 0
    for (const p of proxies) {
      if (p.status === 'healthy') {
        ok++
        if (p.latency_ms > 0) {
          latSum += p.latency_ms
          latCount++
        }
      } else if (p.status === 'unhealthy') bad++
      else if (p.status === 'disabled') off++
      else pending++
    }
    return { ok, bad, pending, off, avg: latCount > 0 ? Math.round(latSum / latCount) : 0, latCount }
  }, [proxies])

  const rows = useMemo(() => {
    const needle = debounced.trim().toLowerCase()
    return proxies.filter((p) => {
      if (status !== 'all' && p.status !== status) return false
      if (!needle) return true
      return (
        (p.name || '').toLowerCase().includes(needle) ||
        p.url.toLowerCase().includes(needle) ||
        p.id.toLowerCase().includes(needle)
      )
    })
  }, [proxies, debounced, status])

  const pageCount = Math.max(1, Math.ceil(rows.length / pageSize))
  const safePage = Math.min(page, pageCount)
  const pageRows = useMemo(
    () => rows.slice((safePage - 1) * pageSize, safePage * pageSize),
    [rows, safePage, pageSize],
  )

  const nameById = useMemo(() => {
    const m = new Map<string, string>()
    for (const p of proxies) m.set(p.id, p.name || hostOf(p.url))
    return m
  }, [proxies])

  const realmDesc = (realm: string): string => {
    const b = realms[realm]
    if (!b) return '随机池（未设置）'
    if (b.mode === 'fixed') return '固定：' + (nameById.get(b.proxy_id ?? '') ?? b.proxy_id ?? '已删除的节点')
    return MODE_TEXT[b.mode] ?? b.mode
  }

  /** select 的当前值：random / direct / <proxyId>；固定节点已被删除时回落「随机池」。 */
  const realmValue = (realm: string): string => {
    const b = realms[realm]
    if (!b || b.mode === 'random' || !b.mode) return 'random'
    if (b.mode === 'direct') return 'direct'
    if (b.mode === 'fixed' && b.proxy_id && nameById.has(b.proxy_id)) return b.proxy_id
    return 'random'
  }

  const saveRealm = (realm: 'cn' | 'global', value: string) => {
    void run('realm:' + realm, async () => {
      try {
        const body =
          value === 'random' || value === 'direct' ? { mode: value } : { mode: 'fixed', proxy_id: value }
        await api.put('/proxies/realm/' + realm, body)
        toast.ok(realm + ' 默认出口已更新')
      } catch (e) {
        toast.err('保存失败：' + errorText(e))
      } finally {
        // 无论成败都以服务端为准，避免下拉和服务端值不一致。
        q.reload()
      }
    })
  }

  const unavailable = q.error instanceof ApiError && q.error.status === 501

  const checkOne = (p: ProxyView) => {
    void run('check:' + p.id, async () => {
      try {
        await api.post('/proxies/' + encodeURIComponent(p.id) + '/check')
        toast.ok('已排队检测 ' + (p.name || hostOf(p.url)) + '，结果几秒后自动出现')
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const checkAll = () => {
    void run('check:all', async () => {
      try {
        await api.post('/proxies/check')
        toast.ok('已排队检测全部节点')
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const toggleEnabled = (p: ProxyView, next: boolean) => {
    void run('enable:' + p.id, async () => {
      try {
        await api.patch('/proxies/' + encodeURIComponent(p.id), {
          name: '',
          url: '',
          password: '',
          enabled: next,
        })
        toast.ok((next ? '已启用 ' : '已停用 ') + (p.name || hostOf(p.url)) + '，检测状态已重置')
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const openEdit = (p: ProxyView) => {
    setFormErr(null)
    setForm({ id: p.id, name: p.name, url: p.url, password: '', enabled: p.enabled })
  }

  const submitEdit = () => {
    if (!form) return
    const name = form.name.trim()
    const url = form.url.trim()
    if (byteLen(name) > 128) {
      setFormErr('名称最多 128 字节（中文约 42 字）')
      return
    }
    if (!/^(https?|socks5h?):\/\//i.test(url)) {
      setFormErr('地址需以 http:// 、https:// 、socks5:// 或 socks5h:// 开头')
      return
    }
    if (form.password !== '' && !/@/.test(url)) {
      setFormErr('设置密码要求地址里带用户名，如 socks5://user@host:1080')
      return
    }
    setFormErr(null)
    void run('save', async () => {
      try {
        await api.patch('/proxies/' + encodeURIComponent(form.id), {
          name,
          url,
          password: form.password,
          enabled: form.enabled,
        })
        toast.ok('已保存，该节点的检测状态已重置')
        setForm(null)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const submitDelete = () => {
    if (!del) return
    void run('del', async () => {
      try {
        await api.del('/proxies/' + encodeURIComponent(del.id))
        toast.ok('已删除节点')
        setDel(null)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const submitImport = () => {
    const lines = addText.trim()
    if (!lines) {
      toast.err('先粘贴至少一条代理地址')
      return
    }
    // 无效行要能原样改完重提：按后端返回的行号回填到输入框，其余行清掉。
    const rawLines = lines.split(/\r?\n/)
    void run('import', async () => {
      try {
        const res = await api.post<ImportResp>('/proxies', { lines })
        const list = res.results ?? []
        setImported(list)
        let added = 0
        let dup = 0
        let invalid = 0
        let unsupported = 0
        const badLines: string[] = []
        for (const r of list) {
          if (r.status === 'added') added++
          else if (r.status === 'duplicate') dup++
          else if (r.status === 'unsupported') unsupported++
          else invalid++
          if (r.status === 'invalid') {
            const src = rawLines[r.line - 1]
            if (src && src.trim()) badLines.push(src.trim())
          }
        }
        const parts = ['添加 ' + added + ' 个']
        if (dup > 0) parts.push('重复 ' + dup + ' 个')
        if (invalid > 0) parts.push('无效 ' + invalid + ' 条')
        if (unsupported > 0) parts.push('不支持 ' + unsupported + ' 条')
        const summary = parts.join(' · ')
        setAddText(badLines.join('\n'))
        if (added > 0) toast.ok(summary)
        else toast.warn('没有新增节点：' + summary)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const openHealth = () => {
    setHForm({
      url: health.url,
      interval: String(health.interval_seconds),
      timeout: String(health.timeout_seconds),
      fail: String(health.failure_threshold),
      success: String(health.success_threshold),
    })
  }

  const submitHealth = () => {
    if (!hForm) return
    const url = hForm.url.trim()
    if (!/^https?:\/\//i.test(url)) {
      toast.err('检测地址必须是 http:// 或 https:// 开头')
      return
    }
    const iv = Number(hForm.interval)
    const to = Number(hForm.timeout)
    const ft = Number(hForm.fail)
    const st = Number(hForm.success)
    if (!Number.isInteger(iv) || iv < 1 || iv > 86400) {
      toast.err('检测间隔需为 1-86400 秒')
      return
    }
    if (!Number.isInteger(to) || to < 1 || to > 300) {
      toast.err('超时需为 1-300 秒')
      return
    }
    if (!Number.isInteger(ft) || ft < 1 || ft > 100 || !Number.isInteger(st) || st < 1 || st > 100) {
      toast.err('失败/成功阈值需为 1-100 的整数')
      return
    }
    void run('health', async () => {
      try {
        await api.put('/proxies/health', {
          url,
          interval_seconds: iv,
          timeout_seconds: to,
          failure_threshold: ft,
          success_threshold: st,
        })
        toast.ok('检测策略已保存，所有节点将重新检测')
        setHForm(null)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const columns: Column<ProxyView>[] = [
    {
      key: 'name',
      title: '节点',
      width: '20%',
      sortValue: (p) => p.name || hostOf(p.url),
      render: (p) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="bold truncate">{p.name || hostOf(p.url)}</span>
          <span className="muted tiny mono truncate">{p.id.slice(0, 8)}</span>
          <div className="row gap1">
            {p.bound_count > 0 ? <span className="chip">绑定 {p.bound_count} 号</span> : null}
            {p.password_set ? <span className="chip">凭证</span> : null}
          </div>
        </div>
      ),
    },
    {
      key: 'url',
      title: '地址',
      render: (p) => {
        const masked = maskUrl(p.url, p.password_set)
        return (
          <div className="stack" style={{ gap: 2 }}>
            <span className="mono small truncate" title={masked}>
              {masked}
            </span>
            <span className="row gap1">
              <span className="chip">{p.protocol}</span>
              <span className="muted tiny">{hostOf(p.url)}</span>
            </span>
          </div>
        )
      },
    },
    {
      key: 'status',
      title: '状态',
      width: '132px',
      sortValue: (p) => p.status,
      render: (p) => {
        const s = STATUS[p.status] ?? { text: p.status || '未知', tone: 'muted' as Tone }
        return (
          <div className="stack" style={{ gap: 2 }}>
            <div className="row gap1 v-center">
              <Badge tone={s.tone}>{s.text}</Badge>
              {p.checking ? <Badge tone="accent">检测中</Badge> : null}
            </div>
            {p.last_error ? (
              <span className="muted tiny truncate" title={p.last_error}>
                {p.last_error}
              </span>
            ) : null}
          </div>
        )
      },
    },
    {
      key: 'probe',
      title: '探测',
      width: '150px',
      render: (p) => {
        const t = Date.parse(p.checked_at || '')
        const checked = isFinite(t) && t > 0
        return (
          <div className="stack" style={{ gap: 2 }}>
            <div className="row gap1 v-center">
              <Badge tone={latencyTone(p.latency_ms)}>{p.latency_ms > 0 ? p.latency_ms + ' ms' : '—'}</Badge>
            </div>
            <span className="muted tiny" title={checked ? clock(t) : ''}>
              {checked ? ago(t) : '尚未检测'}
            </span>
            <span className="muted tiny">
              成功 {p.successes} / 失败 {p.failures}
            </span>
          </div>
        )
      },
    },
    {
      key: 'enabled',
      title: '启用',
      width: '88px',
      sortValue: (p) => (p.enabled ? 0 : 1),
      render: (p) => (
        <Switch
          checked={p.enabled}
          label={(p.enabled ? '停用 ' : '启用 ') + (p.name || hostOf(p.url))}
          disabled={busy === 'enable:' + p.id}
          onChange={(next) => {
            if (next !== p.enabled) toggleEnabled(p, next)
          }}
        />
      ),
    },
    {
      key: 'actions',
      title: '操作',
      width: '168px',
      render: (p) => (
        <div className="row gap1">
          <button
            type="button"
            className="btn ghost sm"
            disabled={busy === 'check:' + p.id}
            onClick={() => checkOne(p)}
          >
            {busy === 'check:' + p.id ? '排队…' : '测试'}
          </button>
          <button type="button" className="btn ghost sm" onClick={() => openEdit(p)}>
            编辑
          </button>
          <button
            type="button"
            className="btn danger sm"
            disabled={p.bound_count > 0}
            title={p.bound_count > 0 ? '先解除 ' + p.bound_count + ' 个账号的绑定再删除' : undefined}
            onClick={() => setDel(p)}
          >
            删除
          </button>
        </div>
      ),
    },
  ]

  const healthItems = [
    { k: '检测地址', v: health.url, mono: true },
    { k: '检测间隔', v: health.interval_seconds + ' 秒' },
    { k: '超时', v: health.timeout_seconds + ' 秒' },
    { k: '判失败阈值', v: health.failure_threshold + ' 次连续失败' },
    { k: '判恢复阈值', v: health.success_threshold + ' 次连续成功' },
  ]

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>代理池</h2>
          <div className="lead">节点导入、可用性检测与健康策略；账号绑定的出口在这里统一维护</div>
        </div>
        <div className="acts">
          <button type="button" className="btn ghost" onClick={() => q.reload()} disabled={q.loading}>
            {q.loading ? '刷新中…' : '刷新'}
          </button>
          <button type="button" className="btn ghost" onClick={checkAll} disabled={busy === 'check:all' || proxies.length === 0}>
            {busy === 'check:all' ? '排队中…' : '全部检测'}
          </button>
          <button type="button" className="btn primary" onClick={() => { setImported(null); setAdding(true) }}>
            添加节点
          </button>
        </div>
      </div>

      {q.error ? (
        <div className="notice bad">
          <span>
            {unavailable
              ? '代理池不可用：服务端没有启用 ProxyPool，本页无法管理节点。'
              : '加载失败：' + errorText(q.error)}
          </span>
          {!unavailable && (
            <button type="button" className="btn ghost sm" onClick={() => q.reload()}>
              重试
            </button>
          )}
        </div>
      ) : null}

      <StatsGrid>
        <Stat
          label="节点总数"
          value={q.first ? '—' : proxies.length}
          sub={stats.off > 0 ? stats.off + ' 个已停用' : '全部启用'}
        />
        <Stat label="可用" value={q.first ? '—' : stats.ok} tone="ok" sub="可用于出站" />
        <Stat
          label="异常"
          value={q.first ? '—' : stats.bad}
          tone={stats.bad > 0 ? 'bad' : undefined}
          sub={stats.pending > 0 ? stats.pending + ' 个待检测' : '无待检测'}
        />
        <Stat
          label="平均延迟"
          value={q.first ? '—' : stats.avg > 0 ? stats.avg + ' ms' : '—'}
          sub={stats.latCount > 0 ? '基于 ' + stats.latCount + ' 个可用节点' : '暂无可用节点'}
        />
      </StatsGrid>

      <Box
        title="节点列表"
        sub={
          proxies.length > 0
            ? proxies.length + ' 个代理 · ' + stats.ok + ' 个可用 · 每 5 秒自动刷新检测结果'
            : undefined
        }
        actions={
          <>
            <input
              className="input"
              placeholder="搜索名称 / 地址"
              aria-label="搜索代理节点"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              style={{ maxWidth: 200 }}
            />
            <Segmented<StatusFilter>
              ariaLabel="按状态筛选"
              value={status}
              onChange={setStatus}
              options={[
                { value: 'all', label: '全部' },
                { value: 'healthy', label: '可用' },
                { value: 'unhealthy', label: '失败' },
                { value: 'pending', label: '待检测' },
                { value: 'disabled', label: '停用' },
              ]}
            />
          </>
        }
        flush
      >
        {q.first ? (
          <div className="tbl-state">
            <Skeleton rows={5} />
          </div>
        ) : (
          <>
          <DataTable<ProxyView>
            columns={columns}
            rows={pageRows}
            rowKey={(p) => p.id}
            loading={q.loading}
            empty={proxies.length === 0 ? '代理池还是空的' : '没有匹配的节点'}
            emptyHint={
              proxies.length === 0 ? (
                <>
                  点「添加节点」粘贴代理地址（每行一条，支持明文 URI、base64 订阅和 Clash YAML），保存后会自动检测。
                  <button type="button" className="btn primary sm" onClick={() => { setImported(null); setAdding(true) }}>
                    添加节点
                  </button>
                </>
              ) : (
                '换个关键词，或把状态筛选切回「全部」。'
              )
            }
          />
          <Pager
            page={safePage}
            pageSize={pageSize}
            total={rows.length}
            onPage={setPage}
            onPageSize={(n) => {
              setPageSize(n)
              setPage(1)
            }}
          />
          </>
        )}
      </Box>

      <div className="grid2">
        <Box title="默认出口" sub="账号没有显式绑定时按这里走">
          {q.first ? (
            <Skeleton rows={3} />
          ) : (
            <div className="stack">
              <Field label="cn 域" hint={'当前：' + realmDesc('cn')}>
                <select
                  className="select"
                  aria-label="cn 域默认出口"
                  value={realmValue('cn')}
                  disabled={busy === 'realm:cn'}
                  onChange={(e) => saveRealm('cn', e.target.value)}
                >
                  <option value="random">随机池（默认）</option>
                  <option value="direct">直连（不走代理）</option>
                  {proxies.map((p) => (
                    <option key={p.id} value={p.id}>
                      {(p.name || hostOf(p.url)) + ' · ' + (STATUS[p.status]?.text ?? p.status)}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="global 域" hint={'当前：' + realmDesc('global')}>
                <select
                  className="select"
                  aria-label="global 域默认出口"
                  value={realmValue('global')}
                  disabled={busy === 'realm:global'}
                  onChange={(e) => saveRealm('global', e.target.value)}
                >
                  <option value="random">随机池（默认）</option>
                  <option value="direct">直连（不走代理）</option>
                  {proxies.map((p) => (
                    <option key={p.id} value={p.id}>
                      {(p.name || hostOf(p.url)) + ' · ' + (STATUS[p.status]?.text ?? p.status)}
                    </option>
                  ))}
                </select>
              </Field>
              <div className="muted tiny">
                账号级绑定：{Object.keys(bindings).length} 个账号已单独指定，优先于这里的默认出口。
              </div>
            </div>
          )}
        </Box>

        <Box
          title="健康检测"
          sub="改策略会让所有节点重新检测"
          actions={
            <button type="button" className="btn ghost sm" onClick={openHealth}>
              修改
            </button>
          }
        >
          {q.first ? <Skeleton rows={4} /> : <KV items={healthItems} />}
        </Box>
      </div>

      <div className="muted tiny">
        凭证在列表里始终打码；编辑时密码留空表示不修改。判定可用需要连续 N 次成功，失败 N 次后踢出随机池。
      </div>

      <Modal
        open={adding}
        title="添加节点"
        onClose={() => {
          setAdding(false)
          setImported(null)
        }}
        wide
        footer={
          <>
            <button type="button" className="btn ghost" onClick={() => { setAdding(false); setImported(null) }}>
              关闭
            </button>
            <button type="button" className="btn primary" onClick={submitImport} disabled={busy === 'import'}>
              {busy === 'import' ? '导入中…' : '导入'}
            </button>
          </>
        }
      >
        <div className="stack">
          <Field
            label="代理地址"
            hint="每行一条：socks5://user:pass@host:1080 、http://host:8080；也支持整段 base64 订阅或 Clash YAML。重复地址会被跳过。"
          >
            <textarea
              className="textarea"
              rows={8}
              placeholder={'socks5://user:pass@127.0.0.1:1080\nhttp://127.0.0.1:8080'}
              value={addText}
              onChange={(e) => setAddText(e.target.value)}
            />
          </Field>

          {imported && imported.length > 0 ? (
            <div className="feed">
              <div className="small bold">
                导入结果 · 添加 {imported.filter((r) => r.status === 'added').length} 个 · 重复{' '}
                {imported.filter((r) => r.status === 'duplicate').length} 个 · 无效{' '}
                {imported.filter((r) => r.status !== 'added' && r.status !== 'duplicate').length} 条
              </div>
              {imported.map((r) => {
                const s = IMPORT_STATUS[r.status] ?? { text: r.status, tone: 'muted' as Tone }
                return (
                  <div key={r.line + ':' + r.status + ':' + (r.name ?? '')} className="row gap2 v-center">
                    <Badge tone={s.tone}>{s.text}</Badge>
                    <span className="small truncate">
                      第 {r.line} 行{r.name ? ' · ' + r.name : ''}
                    </span>
                    {r.error ? <span className="muted tiny truncate">{r.error}</span> : null}
                  </div>
                )
              })}
            </div>
          ) : null}
        </div>
      </Modal>

      <Modal
        open={form !== null}
        title="编辑节点"
        onClose={() => setForm(null)}
        footer={
          <>
            <button type="button" className="btn ghost" onClick={() => setForm(null)}>
              取消
            </button>
            <button type="button" className="btn primary" onClick={submitEdit} disabled={busy === 'save'}>
              {busy === 'save' ? '保存中…' : '保存'}
            </button>
          </>
        }
      >
        {form ? (
          <div className="stack">
            <Field label="名称" error={formErr && byteLen(form.name.trim()) > 128 ? formErr : undefined}>
              <input
                className="input"
                value={form.name}
                placeholder={hostOf(form.url)}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </Field>
            <Field label="地址" hint="改地址会重置该节点的检测状态">
              <input
                className="input mono"
                value={form.url}
                onChange={(e) => setForm({ ...form, url: e.target.value })}
              />
            </Field>
            <Field
              label="密码"
              hint={form.password === '' ? '留空 = 不修改（已存的密码继续生效）' : '写回地址里的密码部分'}
            >
              <input
                className="input"
                type="password"
                autoComplete="new-password"
                value={form.password}
                placeholder="不修改"
                onChange={(e) => setForm({ ...form, password: e.target.value })}
              />
            </Field>
            <Field label="启用">
              <Switch
                checked={form.enabled}
                label="启用该节点"
                onChange={(next) => setForm({ ...form, enabled: next })}
              />
            </Field>
            {formErr ? <div className="notice bad">{formErr}</div> : null}
          </div>
        ) : null}
      </Modal>

      <ConfirmDialog
        open={del !== null}
        title="删除节点"
        message={
          del
            ? '确认删除「' +
              (del.name || hostOf(del.url)) +
              '」？' +
              (del.bound_count > 0 ? '该节点还被 ' + del.bound_count + ' 个账号固定绑定，需先解绑。' : '')
            : ''
        }
        confirmText="删除"
        danger
        busy={busy === 'del'}
        onConfirm={submitDelete}
        onClose={() => setDel(null)}
      />

      <Modal
        open={hForm !== null}
        title="健康检测策略"
        onClose={() => setHForm(null)}
        footer={
          <>
            <button type="button" className="btn ghost" onClick={() => setHForm(null)}>
              取消
            </button>
            <button type="button" className="btn primary" onClick={submitHealth} disabled={busy === 'health'}>
              {busy === 'health' ? '保存中…' : '保存'}
            </button>
          </>
        }
      >
        {hForm ? (
          <div className="stack">
            <div className="notice warn">保存后所有节点的检测状态会清空并重新检测。</div>
            <Field label="检测地址" hint="HTTP(S)，不带凭证与锚点">
              <input
                className="input mono"
                value={hForm.url}
                onChange={(e) => setHForm({ ...hForm, url: e.target.value })}
              />
            </Field>
            <div className="formgrid">
              <Field label="检测间隔（秒）" hint="1-86400">
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={86400}
                  value={hForm.interval}
                  onChange={(e) => setHForm({ ...hForm, interval: e.target.value })}
                />
              </Field>
              <Field label="超时（秒）" hint="1-300">
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={300}
                  value={hForm.timeout}
                  onChange={(e) => setHForm({ ...hForm, timeout: e.target.value })}
                />
              </Field>
              <Field label="判失败阈值" hint="连续失败 1-100 次">
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={100}
                  value={hForm.fail}
                  onChange={(e) => setHForm({ ...hForm, fail: e.target.value })}
                />
              </Field>
              <Field label="判恢复阈值" hint="连续成功 1-100 次">
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={100}
                  value={hForm.success}
                  onChange={(e) => setHForm({ ...hForm, success: e.target.value })}
                />
              </Field>
            </div>
          </div>
        ) : null}
      </Modal>

      {proxies.length === 0 && !q.first && !q.error ? (
        <div className="notice ok">
          <span>节点为空时所有账号走直连。</span>
        </div>
      ) : null}

      {proxies.length > 0 && stats.ok === 0 && !q.first ? (
        <div className="notice warn">
          <span>当前没有可用节点：随机池会退回直连，固定绑定则会失败。先点「全部检测」确认网络。</span>
        </div>
      ) : null}
    </div>
  )
}