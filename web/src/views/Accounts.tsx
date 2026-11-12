// Accounts.tsx 账号池：指标概览、分页筛选、批量动作、导入导出与详情弹层。
import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useBusy, useDebounced, useQuery, useSelection } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { ago, clock, credits, cstDate, duration, num, shortTime } from '../lib/format'
import {
  Badge,
  Bar,
  Box,
  ConfirmDialog,
  CopyText,
  Field,
  KV,
  Modal,
  Pager,
  Pre,
  Segmented,
  Skeleton,
  Stat,
  StatsGrid,
  type Tone,
} from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'

type Bucket = { date: string; expires_at?: string; remain: number }

type Account = {
  uid: string
  nickname?: string
  credits?: number
  credits_total?: number
  reserve_blocked?: boolean
  session_dead_fails?: number
  credits_expiring?: number
  credits_earliest_expiry?: string
  credits_earliest_remaining?: number
  credits_expiry_buckets?: Bucket[]
  cooling?: boolean
  cool_kind?: string
  cool_remaining_sec?: number
  until?: string
  reason?: string
  realm?: string
  enterprise?: boolean
  api_key?: boolean
  disabled?: boolean
  disabled_reason?: string
  paused?: boolean
  group?: string
  success_count?: number
  err_total?: number
  last_success?: string
  last_err?: string
  checkin_done?: boolean
  consecutive_fails?: number
  in_flight?: number
}

type AccountsResp = {
  accounts?: Account[]
  total: number
  page: number
  size: number
  pages: number
  realm?: string
  group?: string
  counts?: { all?: number; cn?: number; global?: number }
  groups?: { name: string; count: number }[]
  source?: string
}

type OverviewResp = {
  total?: number
  healthy?: number
  cooling?: number
  disabled?: number
  paused?: number
  credits_sum?: number
}

type ProxyView = { id: string; name: string; enabled?: boolean; status?: string; latency_ms?: number }
type Binding = { mode?: string; proxy_id?: string }
type ProxySnap = { proxies?: ProxyView[]; bindings?: Record<string, Binding> }

type ProbeResp = { verdict?: string; detail?: string; advice?: string }
type BalanceResp = { ok?: boolean; credits?: number; credits_total?: number }

type Filter = 'all' | 'healthy' | 'cooling' | 'disabled' | 'paused'
type ProxyMode = 'random' | 'direct' | 'fixed'
type SimpleAct = 'pause' | 'resume' | 'revive' | 'disable'

const FILTERS: { value: Filter; label: string }[] = [
  { value: 'all', label: '全部状态' },
  { value: 'healthy', label: '健康' },
  { value: 'cooling', label: '冷却' },
  { value: 'disabled', label: '禁用' },
  { value: 'paused', label: '暂停' },
]

const PROXY_OPTIONS: { value: ProxyMode; label: string }[] = [
  { value: 'random', label: '随机池' },
  { value: 'direct', label: '直连' },
  { value: 'fixed', label: '固定节点' },
]

const VERDICT: Record<string, { tone: Tone; text: string }> = {
  lifted: { tone: 'ok', text: '上游已解封，可以点「解冻」恢复使用' },
  still_banned: { tone: 'warn', text: '仍被封禁，继续等待；勿反复解冻，可能加重风控' },
  blocked: { tone: 'warn', text: '会话仍有效，但被上游风控拦截（可能仍是封禁），继续观察' },
  session_dead: { tone: 'warn', text: '不是封禁问题：登录态失效，需要重新登录' },
  inconclusive: { tone: 'warn', text: '探测无明确结论（网络/限流等），稍后再试' },
  unknown: { tone: 'bad', text: '无法探测：上游客户端未配置' },
}

function acctName(s: Account): string {
  const n = (s.nickname || '').trim()
  return n || s.uid.slice(0, 8)
}

function statusOf(s: Account): { text: string; tone: Tone } {
  if (s.disabled) return { text: '禁用', tone: 'bad' }
  if (s.paused) return { text: '暂停', tone: 'accent' }
  if (s.cooling) return { text: '冷却', tone: 'warn' }
  if (s.reserve_blocked) return { text: '保留停牌', tone: 'warn' }
  return { text: '健康', tone: 'ok' }
}

function matchFilter(s: Account, f: Filter): boolean {
  if (f === 'all') return true
  if (f === 'disabled') return !!s.disabled
  if (f === 'paused') return !!s.paused
  if (f === 'cooling') return !!s.cooling
  return !s.disabled && !s.paused && !s.cooling
}

function msOf(v?: string): number {
  if (!v) return 0
  const t = Date.parse(v)
  return Number.isNaN(t) ? 0 : t
}

/** 本地时间戳，供导出文件名用（不能 toISOString：那是 UTC）。 */
function stamp(): string {
  const d = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  return (
    String(d.getFullYear()) +
    p(d.getMonth() + 1) +
    p(d.getDate()) +
    '-' +
    p(d.getHours()) +
    p(d.getMinutes()) +
    p(d.getSeconds())
  )
}

type ImportResult = {
  ok?: boolean
  dry_run?: boolean
  total?: number
  imported?: number
  skipped?: number
  creates?: number
  overwrites?: number
  errors?: string[]
  async?: boolean
  job_id?: string
}

type ImportProgress = {
  state?: string
  total?: number
  processed?: number
  imported?: number
  skipped?: number
  creates?: number
  overwrites?: number
  errors?: string[]
  current?: string
  error?: string
}

export default function Accounts() {
  const { tick } = useRefresh()
  const toast = useToast()
  const { busy, run } = useBusy()

  const [realm, setRealm] = useState<'all' | 'cn' | 'global'>('all')
  const [group, setGroup] = useState('')
  const [statusFilter, setStatusFilter] = useState<Filter>('all')
  const [keyword, setKeyword] = useState('')
  const [page, setPage] = useState(1)
  const [size, setSize] = useState(20)

  const [detailUid, setDetailUid] = useState<string | null>(null)
  const [rawOpen, setRawOpen] = useState(false)
  const [probe, setProbe] = useState<ProbeResp | null>(null)
  const [proxyMode, setProxyMode] = useState<ProxyMode>('random')
  const [proxyId, setProxyId] = useState('')

  const [confirm, setConfirm] = useState<{ kind: 'remove' | 'disable'; uids: string[]; name: string } | null>(
    null,
  )

  const [importOpen, setImportOpen] = useState(false)
  const [paste, setPaste] = useState('')
  const [importFile, setImportFile] = useState<File | null>(null)
  const [fileKey, setFileKey] = useState(0)
  const [preview, setPreview] = useState<ImportResult | null>(null)
  const [job, setJob] = useState<ImportProgress | null>(null)
  const [importErr, setImportErr] = useState('')

  const q = useQuery<AccountsResp>(
    (signal) =>
      api.get(
        '/accounts',
        {
          page,
          size,
          realm: realm === 'all' ? undefined : realm,
          group: group || undefined,
        },
        signal,
      ),
    [page, size, realm, group, tick],
    { poll: 5000 },
  )
  const ov = useQuery<OverviewResp>((signal) => api.get('/overview', undefined, signal), [tick], {
    poll: 15000,
  })
  const px = useQuery<ProxySnap>((signal) => api.get('/proxies', undefined, signal), [tick], {
    poll: 30000,
  })

  const sel = useSelection<string>()
  const data = q.data
  const kw = useDebounced(keyword, 250).trim().toLowerCase()

  const rows = useMemo(() => {
    const list = data?.accounts ?? []
    return list.filter((s) => {
      if (!matchFilter(s, statusFilter)) return false
      if (!kw) return true
      return (s.nickname || '').toLowerCase().indexOf(kw) >= 0 || s.uid.toLowerCase().indexOf(kw) >= 0
    })
  }, [data, statusFilter, kw])

  const proxies = px.data?.proxies ?? []
  const bindings = px.data?.bindings ?? {}
  const proxyName = (id?: string): string => {
    if (!id) return ''
    const hit = proxies.find((p) => p.id === id)
    return hit ? hit.name : id
  }
  const proxyText = (uid: string): string => {
    if (!px.data) return '—'
    const b = bindings[uid]
    if (!b || !b.mode || b.mode === 'random') return '随机池'
    if (b.mode === 'direct') return '直连'
    return proxyName(b.proxy_id) || '固定节点'
  }

  const call = (uid: string, action: string, body?: unknown) =>
    api.post('/accounts/' + encodeURIComponent(uid) + '/' + action, body)

  const doAct = (uid: string, action: SimpleAct, okMsg: string) => {
    void run(action + ':' + uid, async () => {
      try {
        await call(uid, action, action === 'disable' ? { reason: '面板手动禁用' } : undefined)
        toast.ok(okMsg)
        q.reload()
        ov.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const doBalance = (uid: string) => {
    void run('balance:' + uid, async () => {
      try {
        const r = await api.post<BalanceResp>('/accounts/' + encodeURIComponent(uid) + '/balance')
        toast.ok(typeof r.credits === 'number' ? '余额已更新：' + credits(r.credits) : '余额已更新')
        q.reload()
        ov.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const doProbe = (uid: string) => {
    void run('probe:' + uid, async () => {
      try {
        const r = await api.post<ProbeResp>('/accounts/' + encodeURIComponent(uid) + '/probe_ban')
        setProbe(r)
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const applyProxy = (uid: string) => {
    if (proxyMode === 'fixed' && !proxyId) {
      toast.warn('先选一个代理节点')
      return
    }
    void run('proxy:' + uid, async () => {
      try {
        const body: Binding = proxyMode === 'fixed' ? { mode: 'fixed', proxy_id: proxyId } : { mode: proxyMode }
        await api.put('/accounts/' + encodeURIComponent(uid) + '/proxy', body)
        toast.ok(
          proxyMode === 'random' ? '已改为随机池' : proxyMode === 'direct' ? '已改为直连' : '已绑定固定节点',
        )
        px.reload()
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const runBatch = (action: 'balance' | 'pause' | 'resume' | 'revive', label: string) => {
    const uids = sel.list
    if (!uids.length) {
      toast.warn('先勾选账号')
      return
    }
    void run('batch:' + action, async () => {
      let okCount = 0
      let firstErr = ''
      for (const uid of uids) {
        try {
          await call(uid, action)
          okCount++
        } catch (e) {
          if (!firstErr) firstErr = errorText(e)
        }
      }
      if (okCount) {
        toast.ok(label + '：成功 ' + okCount + ' 个' + (uids.length > okCount ? '，失败 ' + (uids.length - okCount) + ' 个' : ''))
      }
      if (firstErr) toast.err(firstErr)
      q.reload()
      ov.reload()
    })
  }

  const runConfirm = () => {
    const c = confirm
    if (!c) return
    void run('confirm', async () => {
      const remove = c.kind === 'remove'
      let okCount = 0
      let firstErr = ''
      for (const uid of c.uids) {
        try {
          await call(uid, remove ? 'remove' : 'disable', remove ? undefined : { reason: '面板批量禁用' })
          okCount++
        } catch (e) {
          if (!firstErr) firstErr = errorText(e)
        }
      }
      if (okCount) {
        toast.ok(
          (remove ? '已删除 ' : '已禁用 ') + okCount + ' 个账号' +
            (c.uids.length > okCount ? '，失败 ' + (c.uids.length - okCount) + ' 个' : ''),
        )
      }
      if (firstErr) toast.err(firstErr)
      if (c.uids.length === 1 && detailUid === c.uids[0]) setDetailUid(null)
      setConfirm(null)
      sel.clear()
      q.reload()
      ov.reload()
    })
  }

  const openDetail = (s: Account) => {
    setProbe(null)
    setRawOpen(false)
    setDetailUid(s.uid)
    const b = bindings[s.uid]
    setProxyMode(b?.mode === 'fixed' ? 'fixed' : b?.mode === 'direct' ? 'direct' : 'random')
    setProxyId(b?.proxy_id ?? '')
  }

  const buildForm = (): FormData => {
    if (!importFile && !paste.trim()) throw new Error('请粘贴 JSON 或选择一个文件')
    const fd = new FormData()
    if (importFile) {
      fd.append('file', importFile)
    } else {
      fd.append('file', new File([paste], 'pasted.json', { type: 'application/json' }))
    }
    return fd
  }

  const doPreview = () => {
    void run('preview', async () => {
      setImportErr('')
      setPreview(null)
      setJob(null)
      try {
        const fd = buildForm()
        fd.append('dryRun', '1')
        setPreview(await api.form<ImportResult>('/import/cockpit', fd))
      } catch (e) {
        setImportErr(errorText(e))
      }
    })
  }

  const doImport = () => {
    void run('import', async () => {
      setImportErr('')
      setPreview(null)
      setJob(null)
      try {
        const fd = buildForm()
        fd.append('async', '1')
        const r = await api.form<ImportResult>('/import/cockpit', fd)
        if (!r.job_id) {
          toast.ok('导入完成：新增 ' + num(r.creates) + '，覆盖 ' + num(r.overwrites) + '，跳过 ' + num(r.skipped))
          q.reload()
          return
        }
        setJob({ state: 'running', total: r.total, processed: 0 })
        let last: ImportProgress | null = null
        for (let i = 0; i < 400; i++) {
          await new Promise((resolve) => setTimeout(resolve, 900))
          last = await api.get<ImportProgress>('/import/cockpit/jobs/' + encodeURIComponent(r.job_id))
          setJob(last)
          if (last.state !== 'running') break
        }
        if (last && last.state === 'done') {
          toast.ok('导入完成：新增 ' + num(last.creates) + '，覆盖 ' + num(last.overwrites) + '，跳过 ' + num(last.skipped))
          q.reload()
          ov.reload()
        } else if (last && last.state === 'failed') {
          toast.err(last.error || '导入失败')
        } else {
          toast.warn('导入仍在后台进行，稍后刷新列表查看')
        }
      } catch (e) {
        setImportErr(errorText(e))
      }
    })
  }

  const closeImport = () => {
    setImportOpen(false)
    setPaste('')
    setImportFile(null)
    setFileKey(fileKey + 1)
    setPreview(null)
    setJob(null)
    setImportErr('')
  }

  const counts: { all?: number; cn?: number; global?: number } = data?.counts ?? {}
  const exportHref = api.downloadUrl(
    '/accounts/export',
    sel.size ? { uids: sel.list.join(',') } : { realm: realm === 'all' ? undefined : realm, group: group || undefined },
  )
  const exportName =
    'workbuddy-accounts-' + (sel.size ? 'picked' + sel.size : 'all') + '-' + stamp() + '.json'

  const ovv = (v?: number): string => (ov.data ? num(v ?? 0) : '—')

  const head = (
    <div className="pagehead">
      <div>
        <h2>账号池</h2>
        <p className="lead">
          {data
            ? '共 ' + num(data.total) + ' 个账号 · 第 ' + data.page + '/' + Math.max(data.pages, 1) + ' 页 · 每 5 秒自动刷新'
            : '账号目录、健康状态与额度'}
        </p>
      </div>
      <div className="acts">
        <button className="btn sm ghost" onClick={() => setImportOpen(true)}>
          导入
        </button>
        <a
          className="btn sm"
          href={exportHref}
          download={exportName}
          title={sel.size ? '导出已勾选的 ' + sel.size + ' 个账号' : '导出当前域/分组的全部账号'}
        >
          导出
        </a>
        <button
          className="btn sm"
          disabled={q.loading}
          onClick={() => {
            q.reload()
            ov.reload()
            px.reload()
          }}
        >
          {q.loading ? '刷新中…' : '刷新'}
        </button>
      </div>
    </div>
  )

  if (!data) {
    return (
      <div className="page">
        {head}
        {q.error ? (
          <div className="notice bad">
            <span>{errorText(q.error)}</span>
            <button className="btn sm" onClick={q.reload}>
              重试
            </button>
          </div>
        ) : (
          <Box>
            <Skeleton rows={8} />
          </Box>
        )}
      </div>
    )
  }

  const detail = detailUid ? (data.accounts ?? []).find((s) => s.uid === detailUid) : undefined

  const columns: Column<Account>[] = [
    {
      key: 'account',
      title: '账号',
      sortValue: (s) => acctName(s).toLowerCase(),
      render: (s) => (
        <div className="entity">
          <span className="avatar-sq">{acctName(s).slice(0, 1).toUpperCase()}</span>
          <div className="meta">
            <div className="n truncate">{acctName(s)}</div>
            <div className="s tiny mono truncate">
              {s.uid.slice(0, 8)}
              {s.group ? ' · ' + s.group : ''}
            </div>
          </div>
        </div>
      ),
    },
    {
      key: 'status',
      title: '状态',
      width: '116px',
      sortValue: (s) => {
        const st = statusOf(s).text
        return st === '健康' ? 0 : st === '冷却' ? 1 : st === '暂停' ? 2 : 3
      },
      render: (s) => {
        const st = statusOf(s)
        return (
          <span title={s.reason || s.disabled_reason || ''}>
            <Badge tone={st.tone}>{st.text}</Badge>
            {s.cooling && s.cool_remaining_sec ? (
              <span className="tiny muted"> {duration(s.cool_remaining_sec)}</span>
            ) : null}
          </span>
        )
      },
    },
    {
      key: 'credits',
      title: '积分/额度',
      align: 'right',
      mono: true,
      width: '130px',
      sortValue: (s) => s.credits ?? 0,
      render: (s) => (
        <span>
          {credits(s.credits)}
          <span className="tiny muted"> / {credits(s.credits_total)}</span>
        </span>
      ),
    },
    {
      key: 'expiry',
      title: '到期',
      width: '132px',
      sortValue: (s) => msOf(s.credits_earliest_expiry),
      render: (s) => {
        if (!s.credits_earliest_expiry) return <span className="muted">—</span>
        const days = Math.floor((msOf(s.credits_earliest_expiry) - Date.now()) / 86400000)
        return (
          <span>
            <span className={days <= 7 ? 'warn-text' : ''}>{cstDate(s.credits_earliest_expiry)}</span>
            {days <= 7 ? (
              <span className="tiny muted"> {days <= 0 ? '已过期' : days + ' 天后'}</span>
            ) : null}
          </span>
        )
      },
    },
    {
      key: 'last',
      title: '最后刷新',
      width: '108px',
      hideMobile: true,
      sortValue: (s) => msOf(s.last_success),
      render: (s) =>
        s.last_success ? (
          <span title={cstDate(s.last_success) + ' ' + clock(s.last_success)}>{ago(s.last_success)}</span>
        ) : (
          <span className="muted">—</span>
        ),
    },
    {
      key: 'proxy',
      title: '代理',
      width: '116px',
      hideMobile: true,
      render: (s) => <span className="small">{proxyText(s.uid)}</span>,
    },
    {
      key: 'act',
      title: '操作',
      width: '76px',
      align: 'right',
      className: 'c-acts',
      render: (s) => (
        <button
          className="btn sm"
          onClick={(e) => {
            e.stopPropagation()
            openDetail(s)
          }}
        >
          详情
        </button>
      ),
    },
  ]

  return (
    <div className="page">
      {head}

      {q.error ? (
        <div className="notice bad">
          <span>刷新失败：{errorText(q.error)}（显示的是上一次的数据）</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      <StatsGrid>
        <Stat label="账号总数" value={num(data.total)} sub={data.source === 'memory' ? '内存池，重启会丢' : '来自账号目录'} />
        <Stat label="健康" value={ovv(ov.data?.healthy)} tone="ok" sub="可参与选号" />
        <Stat label="冷却" value={ovv(ov.data?.cooling)} tone="warn" sub="临时限流" />
        <Stat label="禁用" value={ovv(ov.data?.disabled)} tone="bad" sub="需解冻" />
        <Stat label="暂停" value={ovv(ov.data?.paused)} tone="accent" sub="保留但不选号" />
        <Stat label="积分合计" value={ov.data ? credits(ov.data.credits_sum) : '—'} sub="池内可用" />
      </StatsGrid>

      <Box
        title="账号列表"
        sub={
          statusFilter !== 'all' || kw
            ? '本页 ' + (data.accounts ? data.accounts.length : 0) + ' 条中匹配 ' + rows.length + ' 条'
            : '点行打开详情，可刷新余额、探测、暂停、绑定代理'
        }
      >
        <div className="filters">
          <select
            className="select"
            aria-label="域"
            value={realm}
            onChange={(e) => {
              setRealm(e.target.value as 'all' | 'cn' | 'global')
              setPage(1)
            }}
          >
            <option value="all">全部域 · {num(counts.all)}</option>
            <option value="cn">CN 域 · {num(counts.cn)}</option>
            <option value="global">Global 域 · {num(counts.global)}</option>
          </select>
          <select
            className="select"
            aria-label="分组"
            value={group}
            onChange={(e) => {
              setGroup(e.target.value)
              setPage(1)
            }}
          >
            <option value="">全部分组</option>
            {(data.groups ?? []).map((g) => (
              <option key={g.name} value={g.name}>
                {g.name} · {g.count}
              </option>
            ))}
          </select>
          <select
            className="select"
            aria-label="状态"
            value={statusFilter}
            onChange={(e) => {
              setStatusFilter(e.target.value as Filter)
              setPage(1)
            }}
          >
            {FILTERS.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
          <input
            className="input"
            placeholder="搜索昵称 / UID"
            aria-label="搜索账号"
            value={keyword}
            onChange={(e) => {
              setKeyword(e.target.value)
              setPage(1)
            }}
          />
          <select
            className="select"
            aria-label="每页条数"
            value={size}
            onChange={(e) => {
              setSize(Number(e.target.value))
              setPage(1)
            }}
          >
            {[10, 20, 50, 100].map((n) => (
              <option key={n} value={n}>
                {n} 条/页
              </option>
            ))}
          </select>
        </div>

        {sel.size > 0 ? (
          <div className="actionbar">
            <span className="small muted">已选 {sel.size} 个</span>
            <button className="btn sm" disabled={!!busy} onClick={() => runBatch('balance', '刷新余额')}>
              刷新余额
            </button>
            <button className="btn sm" disabled={!!busy} onClick={() => runBatch('pause', '暂停')}>
              暂停
            </button>
            <button className="btn sm" disabled={!!busy} onClick={() => runBatch('resume', '恢复选号')}>
              恢复选号
            </button>
            <button className="btn sm ok" disabled={!!busy} onClick={() => runBatch('revive', '解冻')}>
              解冻
            </button>
            <button
              className="btn sm danger"
              disabled={!!busy}
              onClick={() => setConfirm({ kind: 'disable', uids: sel.list, name: sel.size + ' 个账号' })}
            >
              禁用
            </button>
            <button
              className="btn sm danger"
              disabled={!!busy}
              onClick={() => setConfirm({ kind: 'remove', uids: sel.list, name: sel.size + ' 个账号' })}
            >
              删除
            </button>
            <button className="btn sm ghost ml-auto" disabled={!!busy} onClick={sel.clear}>
              清空勾选
            </button>
          </div>
        ) : null}

        <DataTable<Account>
          columns={columns}
          rows={rows}
          rowKey={(s) => s.uid}
          loading={q.loading}
          selectable
          selected={sel.set}
          onToggle={(key, next) => {
            if (next !== sel.has(key)) sel.toggle(key)
          }}
          onToggleAll={(next, keys) => {
            if (next) sel.addMany(keys)
            else keys.forEach((k) => { if (sel.has(k)) sel.toggle(k) })
          }}
          onRowClick={openDetail}
          empty={data.total === 0 ? '账号池是空的' : '没有匹配的账号'}
          emptyHint={
            data.total === 0
              ? '点右上角「导入」，粘贴 cockpit 账号 JSON 或选择文件。'
              : '换一个状态，或清空关键字；状态与关键字只在本页内过滤。'
          }
          initialSort={{ key: 'last', dir: 'desc' }}
          caption={
            <span className="tiny muted">
              状态与关键字只在当前页内过滤（服务端只支持域/分组/分页）；导出会带上已勾选的账号。
            </span>
          }
        />

        {data.total > 0 ? (
          <Pager
            page={data.page}
            pageSize={data.size}
            total={data.total}
            onPage={setPage}
            onPageSize={(n) => {
              setSize(n)
              setPage(1)
            }}
          />
        ) : null}
      </Box>

      <Modal
        open={!!detail}
        title={detail ? '账号详情 · ' + acctName(detail) : '账号详情'}
        onClose={() => setDetailUid(null)}
        wide
        footer={
          <button className="btn ghost" onClick={() => setDetailUid(null)}>
            关闭
          </button>
        }
      >
        {detail ? (
          <div className="stack">
            <div className="row v-center gap2 row-wrap">
              <div className="entity">
                <span className="avatar-sq">{acctName(detail).slice(0, 1).toUpperCase()}</span>
                <div className="meta">
                  <div className="n">{acctName(detail)}</div>
                  <div className="s tiny mono">{detail.uid}</div>
                </div>
              </div>
              <CopyText value={detail.uid} label="复制 UID" />
            </div>

            <KV
              items={[
                { k: '状态', v: statusOf(detail).text },
                { k: '域 / 分组', v: (detail.realm || 'cn') + (detail.group ? ' · ' + detail.group : ' · 未分组') },
                { k: '积分 / 总额度', v: credits(detail.credits) + ' / ' + credits(detail.credits_total), mono: true },
                {
                  k: '最近到期',
                  v: detail.credits_earliest_expiry
                    ? cstDate(detail.credits_earliest_expiry) + ' · 剩 ' + credits(detail.credits_earliest_remaining)
                    : '—',
                },
                {
                  k: '最后成功',
                  v: detail.last_success ? shortTime(detail.last_success) + ' · ' + ago(detail.last_success) : '—',
                },
                { k: '最后错误', v: detail.last_err || '—' },
                { k: '成功 / 失败', v: num(detail.success_count) + ' / ' + num(detail.err_total), mono: true },
                { k: '代理', v: proxyText(detail.uid) },
                ...(detail.disabled ? [{ k: '禁用原因', v: detail.disabled_reason || '未记录' }] : []),
                ...(detail.cooling
                  ? [{
                      k: '冷却原因',
                      v:
                        (detail.reason || detail.cool_kind || '限流') +
                        (detail.cool_remaining_sec ? ' · 剩 ' + duration(detail.cool_remaining_sec) : ''),
                    }]
                  : []),
                ...(detail.enterprise ? [{ k: '账号类型', v: '企业版' }] : []),
                ...(detail.api_key ? [{ k: '账号类型', v: 'API Key（不能签到）' }] : []),
              ]}
            />

            <div className="grid2">
              <div className="stack">
                <p className="small muted">账号动作</p>
                <div className="row row-wrap gap2">
                  <button className="btn sm primary" disabled={!!busy} onClick={() => doBalance(detail.uid)}>
                    刷新余额
                  </button>
                  <button className="btn sm" disabled={!!busy} onClick={() => doProbe(detail.uid)}>
                    探测封禁
                  </button>
                  {detail.paused ? (
                    <button className="btn sm" disabled={!!busy} onClick={() => doAct(detail.uid, 'resume', '已恢复参与选号')}>
                      恢复选号
                    </button>
                  ) : (
                    <button
                      className="btn sm"
                      disabled={!!busy || !!detail.disabled}
                      onClick={() => doAct(detail.uid, 'pause', '已暂停，选号会跳过它')}
                    >
                      暂停选号
                    </button>
                  )}
                  {detail.disabled ? (
                    <button className="btn sm ok" disabled={!!busy} onClick={() => doAct(detail.uid, 'revive', '已解冻，恢复可用')}>
                      解冻
                    </button>
                  ) : (
                    <button
                      className="btn sm danger"
                      disabled={!!busy}
                      onClick={() => setConfirm({ kind: 'disable', uids: [detail.uid], name: acctName(detail) })}
                    >
                      禁用
                    </button>
                  )}
                  <button
                    className="btn sm danger"
                    disabled={!!busy}
                    onClick={() => setConfirm({ kind: 'remove', uids: [detail.uid], name: acctName(detail) })}
                  >
                    删除
                  </button>
                  <button className="btn sm ghost" onClick={() => setRawOpen(!rawOpen)}>
                    {rawOpen ? '隐藏原始 JSON' : '原始 JSON'}
                  </button>
                </div>
              </div>

              <div className="stack">
                <p className="small muted">代理绑定</p>
                {px.data ? (
                  <div className="stack">
                    <Segmented value={proxyMode} options={PROXY_OPTIONS} onChange={setProxyMode} ariaLabel="代理模式" />
                    {proxyMode === 'fixed' ? (
                      <select
                        className="select"
                        aria-label="选择代理节点"
                        value={proxyId}
                        onChange={(e) => setProxyId(e.target.value)}
                      >
                        <option value="">选择节点…</option>
                        {proxies.map((p) => (
                          <option key={p.id} value={p.id}>
                            {p.name}
                            {p.enabled === false ? '（停用）' : ''}
                          </option>
                        ))}
                      </select>
                    ) : null}
                    <button
                      className="btn sm"
                      disabled={!!busy || (proxyMode === 'fixed' && !proxyId)}
                      onClick={() => applyProxy(detail.uid)}
                    >
                      保存代理
                    </button>
                  </div>
                ) : (
                  <p className="small muted">
                    代理池不可用{px.error ? '：' + errorText(px.error) : ''}
                  </p>
                )}
              </div>
            </div>

            {probe ? (
              <div className={'notice ' + (VERDICT[probe.verdict || '']?.tone ?? 'warn')}>
                <span>
                  探测结果：{VERDICT[probe.verdict || '']?.text ?? '未知结果（' + (probe.verdict || '') + '）'}
                  {probe.detail ? ' · ' + probe.detail : ''}
                </span>
              </div>
            ) : null}

            {rawOpen ? <Pre maxHeight={280}>{JSON.stringify(detail, null, 2)}</Pre> : null}
          </div>
        ) : null}
      </Modal>

      <Modal
        open={importOpen}
        title="导入账号"
        onClose={closeImport}
        wide
        footer={
          <div className="row gap2">
            <button className="btn ghost" onClick={closeImport}>
              关闭
            </button>
            <button className="btn" disabled={!!busy} onClick={doPreview}>
              预览
            </button>
            <button className="btn primary" disabled={!!busy} onClick={doImport}>
              开始导入
            </button>
          </div>
        }
      >
        <div className="stack">
          <Field label="粘贴 JSON" hint="支持数组、单个对象、{account} 嵌套、{accounts|data|list|items} 信封或 {uid: {...}} 字典。">
            <textarea
              className="textarea"
              rows={8}
              placeholder='[{"uid":"...","account":{...},"auth":{...}}]'
              value={paste}
              onChange={(e) => setPaste(e.target.value)}
              disabled={!!importFile}
            />
          </Field>
          <Field label="或选择文件（.json）" hint="选了文件就忽略上面的粘贴内容；多个文件请分次导入。">
            <div className="row v-center gap2 row-wrap">
              <input
                key={fileKey}
                className="input grow"
                type="file"
                accept=".json,application/json"
                onChange={(e) => setImportFile(e.target.files && e.target.files[0] ? e.target.files[0] : null)}
              />
              {importFile ? (
                <button
                  className="btn sm ghost"
                  onClick={() => {
                    setImportFile(null)
                    setFileKey(fileKey + 1)
                  }}
                >
                  清除文件
                </button>
              ) : null}
            </div>
          </Field>

          {importErr ? (
            <div className="notice bad">
              <span>{importErr}</span>
            </div>
          ) : null}

          {preview ? (
            <div className="stack">
              <div className={'notice ' + (preview.errors && preview.errors.length ? 'warn' : 'ok')}>
                <span>
                  预览：共 {num(preview.total)} 条，将导入 {num(preview.imported)}，跳过 {num(preview.skipped)}；新增{' '}
                  {num(preview.creates)}，覆盖 {num(preview.overwrites)}
                </span>
              </div>
              {preview.errors && preview.errors.length ? (
                <Pre maxHeight={140}>{preview.errors.join('\n')}</Pre>
              ) : null}
            </div>
          ) : null}

          {job ? (
            <div className="stack">
              <div className="row v-center gap2">
                <span className="small muted grow">
                  {job.state === 'running'
                    ? '导入中 ' + num(job.processed) + '/' + num(job.total)
                    : job.state === 'done'
                      ? '导入完成'
                      : '导入失败'}
                </span>
                <span className="small mono">
                  {job.total ? Math.round(((job.processed ?? 0) / job.total) * 100) : 0}%
                </span>
              </div>
              <Bar
                pct={job.total ? Math.min(100, Math.round(((job.processed ?? 0) / job.total) * 100)) : 0}
                tone={job.state === 'failed' ? 'bad' : job.state === 'done' ? 'ok' : 'accent'}
              />
              {job.current ? <p className="tiny muted truncate">当前：{job.current}</p> : null}
              {job.error ? (
                <div className="notice bad">
                  <span>{job.error}</span>
                </div>
              ) : null}
              {job.state === 'done' ? (
                <KV
                  items={[
                    { k: '新增', v: num(job.creates), mono: true },
                    { k: '覆盖', v: num(job.overwrites), mono: true },
                    { k: '跳过', v: num(job.skipped), mono: true },
                    { k: '失败', v: num(job.errors ? job.errors.length : 0), mono: true },
                  ]}
                />
              ) : null}
              {job.errors && job.errors.length ? <Pre maxHeight={140}>{job.errors.join('\n')}</Pre> : null}
            </div>
          ) : null}
        </div>
      </Modal>

      <ConfirmDialog
        open={!!confirm}
        title={confirm && confirm.kind === 'remove' ? '删除账号' : '禁用账号'}
        message={
          confirm
            ? confirm.kind === 'remove'
              ? '将从账号目录删除 ' + confirm.name + '，并尽量清掉对应授权文件。此操作不可撤销。'
              : '将禁用 ' + confirm.name + '，它们不再参与选号；随时可以解冻。'
            : ''
        }
        confirmText={confirm && confirm.kind === 'remove' ? '删除' : '禁用'}
        danger
        busy={!!busy}
        onConfirm={runConfirm}
        onClose={() => setConfirm(null)}
      />
    </div>
  )
}
