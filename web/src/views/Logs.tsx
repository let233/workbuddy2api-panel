import { useEffect, useMemo, useRef, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useDebounced, useLocalStorage, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { clock, cstDate, credits as fmtCredits, num, pct, tok } from '../lib/format'
import { Badge, Box, Empty, Select, Segmented, Skeleton, Stat, StatsGrid, Switch } from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'

// ---------- 运行日志 ----------

type LogEntry = {
  ts: string
  seq: number
  ch: 'chat' | 'task' | 'sys'
  text: string
}

type LogsResp = {
  entries: LogEntry[] | null
  archive_error?: string | null
}

type Level = 'debug' | 'info' | 'warn' | 'error'
type LevelFilter = 'all' | Level
type ChanFilter = 'all' | 'chat' | 'task' | 'sys'

const CAP = 800

const LEVEL_OPTIONS: { value: LevelFilter; label: string }[] = [
  { value: 'all', label: '全部级别' },
  { value: 'error', label: '错误' },
  { value: 'warn', label: '警告' },
  { value: 'info', label: '信息' },
  { value: 'debug', label: '调试' },
]

const CHAN_OPTIONS: { value: ChanFilter; label: string }[] = [
  { value: 'all', label: '全部频道' },
  { value: 'chat', label: '对话' },
  { value: 'task', label: '任务' },
  { value: 'sys', label: '系统' },
]

const CHAN_LABEL: Record<LogEntry['ch'], string> = {
  chat: '对话',
  task: '任务',
  sys: '系统',
}

function levelOf(text: string): Level {
  const cleaned = text.replace(/\b[a-z_]+=(""|'')/g, '')
  if (/error|失败|错误/i.test(cleaned)) return 'error'
  if (/warn|冷却|熔断/i.test(cleaned)) return 'warn'
  if (/debug|调试/i.test(cleaned)) return 'debug'
  return 'info'
}

// ---------- 请求记录 ----------

type ReqEvent = {
  time: string
  request_id: string
  path?: string
  account?: string
  model?: string
  model_eff?: string
  status: number
  ok: boolean
  outcome: string
  duration_ms: number
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
  credit?: number
  credit_known?: boolean
  client_ip?: string
  user_agent?: string
}

type ArchiveStats = {
  enabled: boolean
  target?: string
  rows: number
  days: number
  bytes: number
  dropped_writes: number
  last_error?: string
}

type MetricsResp = {
  completed: number
  in_flight: number
  succeeded: number
  failed: number
  success_rate: number
  http_success_rate: number
  avg_duration_ms: number
  recent?: ReqEvent[] | null
  archive: ArchiveStats
}

type ReqLogsResp = {
  entries: ReqEvent[] | null
  limit: number
}

const OUTCOME_LABEL: Record<string, string> = {
  success: '成功',
  http_error: 'HTTP 错误',
  stream_error: '流错误',
  interrupted: '中断',
}

const OUTCOME_OPTIONS = [
  { value: 'all', label: '全部结果' },
  { value: 'success', label: '成功' },
  { value: 'http_error', label: 'HTTP 错误' },
  { value: 'stream_error', label: '流错误' },
  { value: 'interrupted', label: '中断' },
]

const REQ_RANGES = [
  { value: 'all', label: '全部时间', sec: 0 },
  { value: '1h', label: '近 1 小时', sec: 3600 },
  { value: '6h', label: '近 6 小时', sec: 21600 },
  { value: '24h', label: '近 24 小时', sec: 86400 },
  { value: '7d', label: '近 7 天', sec: 604800 },
]

const PAGE_SIZES = [100, 300, 1000]

function resultLabel(e: ReqEvent): string {
  const base = OUTCOME_LABEL[e.outcome] || 'HTTP ' + e.status
  if (e.status === 429) return '429 限流'
  if (e.outcome === 'http_error' && e.status) return base + ' ' + e.status
  return base
}

function resultTone(e: ReqEvent): 'ok' | 'warn' | 'bad' {
  if (e.ok) return 'ok'
  if (e.status === 429 || e.outcome === 'interrupted' || e.outcome === 'stream_error') return 'warn'
  return 'bad'
}

function modelText(e: ReqEvent): string {
  const eff = e.model_eff || ''
  const req = e.model || ''
  if (eff && req && eff !== req) return req + ' → ' + eff
  return eff || req || '-'
}

function msLabel(v: number | undefined): string {
  if (!v || v <= 0) return '-'
  if (v < 1000) return Math.round(v) + 'ms'
  return (v / 1000).toFixed(v < 10000 ? 2 : 1) + 's'
}

function uaText(ua?: string): string {
  if (!ua) return '-'
  const s = ua.trim()
  if (s.length <= 28) return s
  return s.slice(0, 28) + '…'
}

// ---------- 错误汇总 ----------

type ErrStat = {
  account: string
  kind: string
  code?: string
  model?: string
  count: number
  last?: string
  sev: 'bad' | 'warn' | 'info'
  until?: string
  sample?: string
  sample_full?: string
}

type ErrResp = {
  rows: ErrStat[] | null
  scanned: number
  updated_at: string
}

// ---------- 积分历史 ----------

type CreditEntry = {
  time: string
  uid: string
  account: string
  delta: number
  before: number
  after: number
}

type CreditResp = {
  entries: CreditEntry[] | null
  limit: number
}

function signed(v: number): string {
  const abs = num(Math.abs(v))
  if (v > 0) return '+' + abs
  if (v < 0) return '\u2212' + abs
  return '0'
}

const EXPORT_DAYS = [1, 3, 7]

export default function Logs() {
  const { tick } = useRefresh()
  const toast = useToast()

  // ---- 运行日志 ----
  const [lines, setLines] = useState<LogEntry[]>([])
  const [level, setLevel] = useState<LevelFilter>('all')
  const [chan, setChan] = useState<ChanFilter>('all')
  const [keyword, setKeyword] = useState('')
  const kw = useDebounced(keyword, 250).trim().toLowerCase()
  const [autoScroll, setAutoScroll] = useLocalStorage<boolean>('wb.logs.autoscroll', true)
  const [paused, setPaused] = useState(false)

  const cursor = useRef(0)
  const boxRef = useRef<HTMLDivElement | null>(null)
  const stickRef = useRef(true)

  const q = useQuery<LogsResp>(
    (signal) => api.get<LogsResp>('/logs', { since_seq: cursor.current }, signal),
    [tick, paused],
    { poll: 5000, enabled: !paused },
  )

  useEffect(() => {
    const batch = q.data?.entries ?? []
    if (batch.length === 0) return
    let maxSeq = cursor.current
    for (const e of batch) if (e.seq > maxSeq) maxSeq = e.seq
    cursor.current = maxSeq
    setLines((prev) => {
      const seen = new Set(prev.map((l) => l.seq))
      const add = batch.filter((e) => !seen.has(e.seq))
      if (add.length === 0) return prev
      add.sort((a, b) => a.seq - b.seq)
      const next = prev.concat(add)
      return next.length > CAP ? next.slice(next.length - CAP) : next
    })
  }, [q.data])

  useEffect(() => {
    if (!autoScroll) return
    const el = boxRef.current
    if (!el || !stickRef.current) return
    el.scrollTop = el.scrollHeight
  }, [lines, autoScroll])

  // ---- 请求记录 ----
  const [outcome, setOutcome] = useState('all')
  const [rangeKey, setRangeKey] = useState('all')
  const [limit, setLimit] = useState(300)
  const [reqKeyword, setReqKeyword] = useState('')
  const reqKw = useDebounced(reqKeyword, 250).trim().toLowerCase()

  const rangeSec = REQ_RANGES.find((r) => r.value === rangeKey)?.sec ?? 0
  // from 只在换区间或顶栏刷新时重算：若每次渲染都取 Date.now()，deps 每秒变化会让 useQuery 不停重发请求
  const from = useMemo(
    () => (rangeSec > 0 ? Math.floor(Date.now() / 1000) - rangeSec : undefined),
    [rangeSec, tick],
  )

  const metrics = useQuery<MetricsResp>(
    (signal) => api.get<MetricsResp>('/request_metrics', { recent: 0 }, signal),
    [tick],
    { poll: 15000 },
  )
  const archiveOn = metrics.data?.archive?.enabled === true

  const archiveRows = useQuery<ReqLogsResp>(
    (signal) =>
      api.get<ReqLogsResp>(
        '/request_logs',
        { limit, outcome: outcome === 'all' ? undefined : outcome, from },
        signal,
      ),
    [tick, limit, outcome, from, archiveOn],
    { poll: 15000, enabled: archiveOn },
  )

  // 归档关闭时回落内存指标里的 recent（最近 100 条），这时筛选只能在前端做
  const memoryRows = useQuery<MetricsResp>(
    (signal) => api.get<MetricsResp>('/request_metrics', undefined, signal),
    [tick],
    { poll: 15000, enabled: metrics.data !== undefined && !archiveOn },
  )

  const rawReqs: ReqEvent[] = archiveOn ? archiveRows.data?.entries ?? [] : memoryRows.data?.recent ?? []

  const reqs = useMemo(() => {
    const hay = (e: ReqEvent): string[] => [
      e.account || '',
      e.model || '',
      e.model_eff || '',
      e.request_id || '',
      e.client_ip || '',
      e.user_agent || '',
      e.path || '',
    ]
    return rawReqs.filter((e) => {
      if (!archiveOn) {
        if (outcome !== 'all' && e.outcome !== outcome) return false
        if (from) {
          const t = new Date(e.time).getTime() / 1000
          if (isFinite(t) && t < from) return false
        }
      }
      if (reqKw === '') return true
      return hay(e).some((v) => v.toLowerCase().includes(reqKw))
    })
  }, [archiveOn, rawReqs, reqKw, outcome, from])

  const reqLoading = archiveOn ? archiveRows.loading : memoryRows.loading
  const reqError = archiveOn ? archiveRows.error : memoryRows.error
  const reqFirst = archiveOn ? archiveRows.first : memoryRows.first

  // ---- 错误汇总 / 积分历史 ----
  const errs = useQuery<ErrResp>((signal) => api.get<ErrResp>('/error_summary', undefined, signal), [tick], {
    poll: 15000,
  })
  const [creditLimit, setCreditLimit] = useState(100)
  const creditQ = useQuery<CreditResp>(
    (signal) => api.get<CreditResp>('/credit_history', { limit: creditLimit }, signal),
    [tick, creditLimit],
    { poll: 30000 },
  )

  // ---- 导出 ----
  const [exportDays, setExportDays] = useState(3)
  const [exporting, setExporting] = useState(false)

  const exportZip = async () => {
    setExporting(true)
    try {
      const res = await fetch(api.downloadUrl('/logs/export', { days: exportDays }), {
        credentials: 'same-origin',
      })
      if (!res.ok) {
        let msg = 'HTTP ' + res.status
        try {
          const j = (await res.json()) as { error?: string }
          if (j && j.error) msg = j.error
        } catch {
          msg = msg
        }
        throw new Error(msg)
      }
      const blob = await res.blob()
      const cd = res.headers.get('Content-Disposition') || ''
      const m = /filename="?([^";]+)"?/.exec(cd)
      const name = m && m[1] ? m[1] : 'panel-logs-' + exportDays + 'd.zip'
      const href = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = href
      a.download = name
      document.body.appendChild(a)
      a.click()
      a.remove()
      URL.revokeObjectURL(href)
      toast.ok('已导出日志压缩包')
    } catch (e) {
      toast.err(errorText(e))
    } finally {
      setExporting(false)
    }
  }

  const filtered = useMemo(
    () =>
      lines.filter((l) => {
        if (chan !== 'all' && l.ch !== chan) return false
        if (level !== 'all' && levelOf(l.text) !== level) return false
        if (kw !== '' && !l.text.toLowerCase().includes(kw)) return false
        return true
      }),
    [lines, chan, level, kw],
  )

  const errCount = useMemo(() => lines.filter((l) => levelOf(l.text) === 'error').length, [lines])
  const warnCount = useMemo(() => lines.filter((l) => levelOf(l.text) === 'warn').length, [lines])
  const chanCount = useMemo(
    () => ({
      task: lines.filter((l) => l.ch === 'task').length,
      chat: lines.filter((l) => l.ch === 'chat').length,
      sys: lines.filter((l) => l.ch === 'sys').length,
    }),
    [lines],
  )

  const reloadAll = () => {
    cursor.current = 0
    setLines([])
    // 暂停时 q.reload() 不会触发请求（effect 被 enabled=false 短路），先恢复拉取
    setPaused(false)
    q.reload()
    metrics.reload()
    if (archiveOn) archiveRows.reload()
    errs.reload()
    creditQ.reload()
  }

  const clearView = () => {
    setLines([])
    toast.info('已清空当前视图，只影响本页显示')
  }

  const reqDirty = outcome !== 'all' || rangeKey !== 'all' || reqKeyword !== ''
  const creditEntries = creditQ.data?.entries ?? []
  const creditNet = creditEntries.reduce((sum, e) => sum + (Number(e.delta) || 0), 0)

  const reqColumns: Column<ReqEvent>[] = [
    {
      key: 'time',
      title: '时间',
      width: '120px',
      render: (e) => (
        <div>
          <div>{clock(e.time)}</div>
          <div className="tiny muted">{cstDate(e.time)}</div>
        </div>
      ),
      sortValue: (e) => new Date(e.time).getTime(),
    },
    {
      key: 'result',
      title: '结果',
      width: '110px',
      render: (e) => <Badge tone={resultTone(e)}>{resultLabel(e)}</Badge>,
      sortValue: (e) => e.status,
    },
    {
      key: 'model',
      title: '模型',
      render: (e) => <span className="mono">{modelText(e)}</span>,
      sortValue: (e) => modelText(e),
    },
    { key: 'account', title: '账号', render: (e) => e.account || '-', hideMobile: true },
    { key: 'ip', title: 'IP', mono: true, hideMobile: true, render: (e) => e.client_ip || '-' },
    {
      key: 'ua',
      title: 'UA',
      hideMobile: true,
      render: (e) => <span title={e.user_agent || ''}>{uaText(e.user_agent)}</span>,
    },
    {
      key: 'duration',
      title: '耗时',
      align: 'right',
      render: (e) => msLabel(e.duration_ms),
      sortValue: (e) => e.duration_ms,
    },
    {
      key: 'tokens',
      title: 'token',
      align: 'right',
      render: (e) => (
        <span title={'prompt ' + num(e.prompt_tokens) + ' · completion ' + num(e.completion_tokens)}>
          {tok(e.total_tokens)}
        </span>
      ),
      sortValue: (e) => e.total_tokens || 0,
    },
    {
      key: 'credit',
      title: '积分',
      align: 'right',
      hideMobile: true,
      render: (e) => (e.credit_known ? fmtCredits(e.credit) : '—'),
      sortValue: (e) => e.credit || 0,
    },
    {
      key: 'rid',
      title: '请求 ID',
      mono: true,
      hideMobile: true,
      render: (e) => (
        <span title={e.request_id}>{e.request_id ? e.request_id.slice(0, 12) + '…' : '-'}</span>
      ),
    },
  ]

  const errColumns: Column<ErrStat>[] = [
    { key: 'account', title: '账号', render: (r) => r.account || '(全局)' },
    {
      key: 'kind',
      title: '类型',
      render: (r) => <Badge tone={r.sev === 'bad' ? 'bad' : r.sev === 'warn' ? 'warn' : 'muted'}>{r.kind}</Badge>,
      sortValue: (r) => r.kind,
    },
    { key: 'code', title: '上游码', mono: true, render: (r) => r.code || '-' },
    {
      key: 'model',
      title: '模型',
      hideMobile: true,
      render: (r) => (r.model ? <span className="mono">{r.model}</span> : '-'),
    },
    { key: 'count', title: '次数', align: 'right', render: (r) => num(r.count), sortValue: (r) => r.count },
    { key: 'last', title: '最近', width: '90px', render: (r) => r.last || '-' },
    { key: 'until', title: '解除时刻', width: '110px', hideMobile: true, render: (r) => r.until || '-' },
    {
      key: 'sample',
      title: '样本',
      hideMobile: true,
      render: (r) => (
        <span className="truncate" title={r.sample_full || r.sample || ''}>
          {r.sample || '-'}
        </span>
      ),
    },
  ]

  const creditColumns: Column<CreditEntry>[] = [
    {
      key: 'time',
      title: '时间',
      width: '120px',
      render: (e) => clock(e.time),
      sortValue: (e) => new Date(e.time).getTime(),
    },
    { key: 'account', title: '账号', render: (e) => e.account || e.uid || '-' },
    {
      key: 'delta',
      title: '变动',
      align: 'right',
      render: (e) => <span className={e.delta < 0 ? 'warn-text' : ''}>{signed(Number(e.delta) || 0)}</span>,
      sortValue: (e) => e.delta,
    },
    { key: 'after', title: '变动后余额', align: 'right', render: (e) => num(e.after), sortValue: (e) => e.after },
    {
      key: 'note',
      title: '说明',
      hideMobile: true,
      render: (e) => (
        <span className="muted">
          余额 {num(e.before)} → {num(e.after)}
        </span>
      ),
    },
  ]

  const reqSub =
    '来源：' +
    (archiveOn ? '归档查询' : '内存指标') +
    ' · 每 15 秒刷新' +
    (archiveOn ? '' : ' · 筛选仅作用于已取回记录')

  const logSub =
    '等宽字体、可换行；含 error/失败 的行标红，含 warn/冷却/熔断 的行标黄 · 任务 ' +
    num(chanCount.task) +
    ' · 对话 ' +
    num(chanCount.chat) +
    ' · 系统 ' +
    num(chanCount.sys)

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>请求与日志</h2>
          <p className="lead">请求记录、错误汇总、服务端运行日志与积分明细；日志每 5 秒增量拉取</p>
        </div>
        <div className="acts">
          {errCount > 0 ? <Badge tone="bad">{errCount} 错误</Badge> : null}
          {warnCount > 0 ? <Badge tone="warn">{warnCount} 警告</Badge> : null}
          <button className="btn sm" onClick={reloadAll}>
            刷新
          </button>
        </div>
      </div>

      <StatsGrid>
        <Stat label="已完成请求" value={num(metrics.data?.completed)} sub={'进行中 ' + num(metrics.data?.in_flight)} />
        <Stat
          label="完成成功率"
          value={metrics.data ? pct(metrics.data.success_rate) : '-'}
          tone={metrics.data && metrics.data.success_rate < 90 ? 'warn' : 'ok'}
          sub={'HTTP 成功 ' + (metrics.data ? pct(metrics.data.http_success_rate) : '-')}
        />
        <Stat label="平均耗时" value={metrics.data ? msLabel(metrics.data.avg_duration_ms) : '-'} />
        <Stat
          label="归档条数"
          value={metrics.data?.archive?.enabled ? num(metrics.data.archive.rows) : '未开启'}
          sub={metrics.data?.archive?.enabled ? metrics.data.archive.days + ' 天' : '仅内存指标'}
          tone={metrics.data?.archive?.enabled ? undefined : 'muted'}
        />
      </StatsGrid>

      {metrics.error ? (
        <div className="notice bad">
          <span>读取请求指标失败：{errorText(metrics.error)}</span>
          <button className="btn sm" onClick={() => metrics.reload()}>
            重试
          </button>
        </div>
      ) : null}
      {metrics.data?.archive?.last_error ? (
        <div className="notice warn">
          <span>
            归档写入异常：{metrics.data.archive.last_error}
            {metrics.data.archive.dropped_writes > 0
              ? ' · 已丢弃 ' + num(metrics.data.archive.dropped_writes) + ' 条'
              : ''}
          </span>
        </div>
      ) : null}
      {metrics.data && !archiveOn ? (
        <div className="notice warn">
          <span>
            请求归档未开启，下方只显示进程内最近 100 条；开启 logging.request_archive 后才能按时间范围查询历史。
          </span>
        </div>
      ) : null}

      <Box
        title="请求记录"
        sub={reqSub}
        flush
        actions={
          reqDirty ? (
            <button
              className="btn sm ghost"
              onClick={() => {
                setOutcome('all')
                setRangeKey('all')
                setReqKeyword('')
              }}
            >
              清除筛选
            </button>
          ) : null
        }
      >
        <div className="filters" style={{ padding: '12px 16px 0' }}>
          <div className="segmented-scroll">
            <Segmented<string>
              value={outcome}
              options={OUTCOME_OPTIONS}
              onChange={(v) => {
                setOutcome(v)
              }}
              ariaLabel="结果筛选"
            />
          </div>
          <Select
            ariaLabel="时间范围"
            value={rangeKey}
            onChange={setRangeKey}
            options={REQ_RANGES.map((r) => ({ value: r.value, label: r.label }))}
          />
          <Select
            ariaLabel="取回条数"
            value={String(limit)}
            onChange={(v) => setLimit(Number(v))}
            menuMaxHeight={220}
            options={PAGE_SIZES.map((n) => ({ value: String(n), label: n + ' 条' }))}
          />
          <input
            className="input"
            placeholder="关键字：账号/模型/IP/请求 ID"
            value={reqKeyword}
            onChange={(e) => {
              setReqKeyword(e.target.value)
            }}
            aria-label="请求关键字"
          />
        </div>

        {reqFirst && reqLoading ? (
          <div style={{ padding: 16 }}>
            <Skeleton rows={6} />
          </div>
        ) : reqError ? (
          <div style={{ padding: 16 }}>
            <div className="notice bad">
              <span>读取请求记录失败：{errorText(reqError)}</span>
              <button
                className="btn sm"
                onClick={() => {
                  if (archiveOn) archiveRows.reload()
                  else memoryRows.reload()
                }}
              >
                重试
              </button>
            </div>
          </div>
        ) : (
          <DataTable<ReqEvent>
            columns={reqColumns}
            rows={reqs}
            rowKey={(e) => e.request_id || e.time}
            loading={reqLoading}
            empty={reqDirty ? '没有符合筛选条件的请求记录' : '暂无请求记录'}
            emptyHint={
              reqDirty ? '放宽结果/时间范围，或清空关键字再试。' : '发起一次对话或调用一次接口后，这里会出现记录。'
            }
          />
        )}
        <div className="tbl-caption">
          命中 {num(reqs.length)} / {num(rawReqs.length)} 条
          {archiveOn ? '' : ' · 内存指标最多保留 100 条'}
        </div>
      </Box>

      <Box
        title="错误汇总"
        sub="按「账号 × 错误类型」聚合日志环里的 WARN/ERR，每 15 秒刷新"
        flush
        actions={<Badge tone="muted">{num(errs.data?.rows?.length ?? 0)} 类</Badge>}
      >
        {errs.error ? (
          <div style={{ padding: 16 }}>
            <div className="notice bad">
              <span>读取错误汇总失败：{errorText(errs.error)}</span>
              <button className="btn sm" onClick={() => errs.reload()}>
                重试
              </button>
            </div>
          </div>
        ) : errs.first && errs.loading ? (
          <div style={{ padding: 16 }}>
            <Skeleton rows={5} />
          </div>
        ) : (
          <DataTable<ErrStat>
            columns={errColumns}
            rows={errs.data?.rows ?? []}
            rowKey={(r) => r.account + '|' + r.kind + '|' + (r.code || '') + '|' + (r.model || '')}
            loading={errs.loading}
            empty="窗口内没有 WARN/ERR"
            emptyHint="日志环已轮转或一切正常；出现限流、鉴权、传输失败时这里会汇总。"
          />
        )}
        <div className="tbl-caption">
          扫描 {num(errs.data?.scanned)} 行
          {errs.data?.updated_at ? ' · 截至 ' + errs.data.updated_at : ''}
        </div>
      </Box>

      {q.error ? (
        <div className="notice bad">
          <span>拉取日志失败：{errorText(q.error)}</span>
          <button className="btn sm" onClick={reloadAll}>
            重试
          </button>
        </div>
      ) : null}
      {q.data?.archive_error ? (
        <div className="notice warn">
          <span>日志落盘异常：{q.data.archive_error}</span>
        </div>
      ) : null}
      {paused ? (
        <div className="notice warn">
          <span>已暂停拉取，恢复后会自动补齐暂停期间的日志。</span>
        </div>
      ) : null}

      <Box
        title="运行日志"
        sub={logSub}
        actions={
          <div className="row">
            <Select
              className="xs"
              ariaLabel="导出天数"
              value={String(exportDays)}
              onChange={(v) => setExportDays(Number(v))}
              menuMaxHeight={240}
              options={EXPORT_DAYS.map((d) => ({ value: String(d), label: '近 ' + d + ' 天' }))}
            />
            <button className="btn sm" onClick={exportZip} disabled={exporting}>
              {exporting ? '导出中…' : '导出 ZIP'}
            </button>
            <button
              className="btn sm"
              onClick={() => {
                setPaused((v) => !v)
              }}
            >
              {paused ? '继续拉取' : '暂停拉取'}
            </button>
            <button className="btn sm ghost" onClick={reloadAll}>
              重新载入
            </button>
            <button className="btn sm ghost" onClick={clearView} disabled={lines.length === 0}>
              清空视图
            </button>
          </div>
        }
      >
        <div className="filters">
          <div className="segmented-scroll">
            <Segmented<LevelFilter> value={level} options={LEVEL_OPTIONS} onChange={setLevel} ariaLabel="日志级别" />
          </div>
          <div className="segmented-scroll">
            <Segmented<ChanFilter> value={chan} options={CHAN_OPTIONS} onChange={setChan} ariaLabel="日志频道" />
          </div>
          <input
            className="input"
            placeholder="关键字过滤"
            value={keyword}
            onChange={(e) => {
              setKeyword(e.target.value)
            }}
            aria-label="关键字过滤"
          />
          <div className="row">
            <Switch
              checked={autoScroll}
              onChange={(v) => {
                stickRef.current = true
                setAutoScroll(v)
              }}
              label="自动滚到底部"
            />
            <span className="small muted">自动滚动</span>
          </div>
          <span className="tiny muted">
            {filtered.length === lines.length
              ? num(lines.length) + ' 条'
              : num(filtered.length) + '/' + num(lines.length) + ' 条'}
          </span>
        </div>

        {q.first && lines.length === 0 ? (
          <Skeleton rows={6} />
        ) : lines.length === 0 ? (
          <Empty
            title="暂无日志"
            hint="服务刚启动或日志已被清空；发起一次对话或手动触发任务后这里会出现记录。"
            action={
              <button className="btn sm" onClick={reloadAll}>
                重新载入
              </button>
            }
          />
        ) : filtered.length === 0 ? (
          <Empty
            title="没有匹配的日志"
            hint="放宽级别/频道筛选，或清空关键字再试。"
            action={
              <button
                className="btn sm"
                onClick={() => {
                  setLevel('all')
                  setChan('all')
                  setKeyword('')
                }}
              >
                重置筛选
              </button>
            }
          />
        ) : (
          <div
            className="feed-box"
            ref={boxRef}
            onScroll={(e) => {
              const el = e.currentTarget
              stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
            }}
          >
            <div className="feed">
              {filtered.map((l) => {
                const lv = levelOf(l.text)
                return (
                  <div className={'line ' + lv} key={l.seq}>
                    <span className="t">{clock(l.ts)}</span>
                    <span className={'lv ' + lv}>{lv}</span>
                    <span className="msg">
                      {chan === 'all' ? <span className="dim">[{CHAN_LABEL[l.ch]}] </span> : null}
                      {l.text}
                    </span>
                  </div>
                )
              })}
            </div>
          </div>
        )}
      </Box>

      <Box
        title="积分明细"
        sub="余额变动记录，来源为后端的积分快照历史"
        flush
        actions={
          <Select
            className="xs"
            ariaLabel="积分记录条数"
            value={String(creditLimit)}
            onChange={(v) => setCreditLimit(Number(v))}
            menuMaxHeight={220}
            options={PAGE_SIZES.map((n) => ({ value: String(n), label: n + ' 条' }))}
          />
        }
      >
        {creditQ.error ? (
          <div style={{ padding: 16 }}>
            <div className="notice bad">
              <span>读取积分历史失败：{errorText(creditQ.error)}</span>
              <button className="btn sm" onClick={() => creditQ.reload()}>
                重试
              </button>
            </div>
          </div>
        ) : creditQ.first && creditQ.loading ? (
          <div style={{ padding: 16 }}>
            <Skeleton rows={5} />
          </div>
        ) : (
          <DataTable<CreditEntry>
            columns={creditColumns}
            rows={creditEntries}
            rowKey={(e) => e.time + '|' + e.uid}
            loading={creditQ.loading}
            empty="暂无积分变动记录"
            emptyHint="执行签到、领奖或消耗积分后，这里会出现变动明细。"
            initialSort={{ key: 'time', dir: 'desc' }}
          />
        )}
        <div className="tbl-caption">
          {num(creditEntries.length)} 条 · 净 {signed(creditNet)}
        </div>
      </Box>
    </div>
  )
}
