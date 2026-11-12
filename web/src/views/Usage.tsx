import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useBusy, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { bytes, credits as fmtCredits, num, pct, shortTime, tok } from '../lib/format'
import { Badge, Box, Empty, Pager, Segmented, Skeleton, Stat, StatsGrid, Tabs } from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'
import { BarChart, LineChart, StackBar } from '../components/charts'

// 字段口径与 internal/usage/usage.go 的 Snapshot/Agg/KeyedAgg/Point/CreditAgg 一一对应。
// cache_hit_rate 是百分数（命中/(命中+未命中)*100，无样本时后端省略该字段）。
type Agg = {
  requests: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cache_hit_tokens: number
  cache_miss_tokens: number
  avg_latency_ms: number
  avg_tokens_per_second: number
  credits: number
  credit_samples: number
  credit_tokens: number
  credits_per_1m_tokens: number
  cache_hit_rate?: number
}

type KeyedAgg = Agg & {
  key: string
  realm?: string
  extra?: string
}

type Point = Agg & {
  t: string
  scope: string
}

type CreditAgg = {
  key: string
  realm?: string
  nickname?: string
  rate?: number
  requests: number
  credits: number
  credit_samples: number
  credit_tokens: number
  credits_per_1m_tokens: number
  cache_hit_tokens?: number
  cache_miss_tokens?: number
  cache_hit_rate?: number
}

type Snapshot = {
  totals: Agg
  by_realm: KeyedAgg[] | null
  by_account: KeyedAgg[] | null
  by_model: KeyedAgg[] | null
  by_api_key: KeyedAgg[] | null
  series: Point[] | null
  credit_by_account: CreditAgg[] | null
  credit_by_model: CreditAgg[] | null
  credit_by_api_key: CreditAgg[] | null
  buckets: number
  file_bytes: number
  since?: string
  last_error?: string
  failed_flushes?: number
  window_from?: string
  window_to?: string
  generated: string
}

type RangeKey = 'today' | '7d' | '30d' | 'custom'
type DimKey = 'account' | 'model' | 'realm' | 'apikey'
type CreditDim = 'account' | 'model' | 'apikey'
type SortKey = 'total_tokens' | 'requests' | 'errors' | 'avg_latency_ms'

const RANGE_OPTIONS: { value: RangeKey; label: string }[] = [
  { value: 'today', label: '今天' },
  { value: '7d', label: '7 天' },
  { value: '30d', label: '30 天' },
  { value: 'custom', label: '自定义' },
]

// 旧面板的维度页签：账号 / 模型 / 域 / API Key（积分维度没有「按域」，后端也没有 credit_by_realm）。
const DIM_TABS: { value: DimKey; label: string }[] = [
  { value: 'account', label: '账号' },
  { value: 'model', label: '模型' },
  { value: 'realm', label: '域' },
  { value: 'apikey', label: 'API Key' },
]

const CREDIT_TABS: { value: CreditDim; label: string }[] = [
  { value: 'account', label: '账号' },
  { value: 'model', label: '模型' },
  { value: 'apikey', label: 'API Key' },
]

const SORT_OPTIONS: { value: SortKey; label: string }[] = [
  { value: 'total_tokens', label: '合计 Token' },
  { value: 'requests', label: '请求数' },
  { value: 'errors', label: '失败数' },
  { value: 'avg_latency_ms', label: '平均延迟' },
]

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

function startOfToday(): number {
  const d = new Date()
  d.setHours(0, 0, 0, 0)
  return d.getTime()
}

/** datetime-local 需要本地时间字符串，不能用 toISOString（那是 UTC）。 */
function toInput(ms: number): string {
  const d = new Date(ms)
  return (
    d.getFullYear() +
    '-' +
    pad2(d.getMonth() + 1) +
    '-' +
    pad2(d.getDate()) +
    'T' +
    pad2(d.getHours()) +
    ':' +
    pad2(d.getMinutes())
  )
}

/** 区间在浏览器本地时区算好再发给服务端：服务端时区与浏览器不一致时「今天」才不会算错。 */
function buildWindow(range: RangeKey, fromText: string, toText: string): { from: number; to: number } {
  const nowSec = Math.floor(Date.now() / 1000)
  if (range === 'today') return { from: Math.floor(startOfToday() / 1000), to: nowSec }
  if (range === '7d') return { from: nowSec - 7 * 86400, to: nowSec }
  if (range === '30d') return { from: nowSec - 30 * 86400, to: nowSec }
  const f = Date.parse(fromText)
  const t = Date.parse(toText)
  if (!isFinite(f) || !isFinite(t) || t <= f) return { from: nowSec - 7 * 86400, to: nowSec }
  return { from: Math.floor(f / 1000), to: Math.floor(t / 1000) }
}

/** 服务端时序标签：小时桶形如 2026-09-16T13，日桶形如 2026-09-16。 */
function pointLabel(p: { t: string; scope: string }): string {
  const t = p.t || ''
  if (!t) return '-'
  if (p.scope === 'day' || t.length <= 10) return t.slice(5)
  return t.slice(5, 10) + ' ' + t.slice(11, 13) + ':00'
}

/** 缓存命中率统一按「有样本才显示」，后端无样本时省略字段。 */
function hitRate(a: { cache_hit_rate?: number; cache_hit_tokens?: number; cache_miss_tokens?: number }): number | null {
  if (typeof a.cache_hit_rate === 'number' && isFinite(a.cache_hit_rate)) return a.cache_hit_rate
  const hit = a.cache_hit_tokens ?? 0
  const miss = a.cache_miss_tokens ?? 0
  if (hit + miss <= 0) return null
  return (hit / (hit + miss)) * 100
}

function hitTone(hr: number): 'ok' | 'warn' | 'bad' {
  return hr >= 90 ? 'ok' : hr >= 80 ? 'warn' : 'bad'
}

function dimName(r: KeyedAgg, dim: DimKey): { main: string; sub: string } {
  if (dim === 'account') {
    const uid = r.key || ''
    const main = r.extra || (uid ? uid.slice(0, 8) : '未知账号')
    return { main, sub: uid ? uid.slice(0, 8) : '' }
  }
  if (dim === 'apikey') {
    const uid = r.key || ''
    if (!uid) return { main: '未按 Key 记录', sub: '升级前历史 / 未鉴权调用' }
    return { main: r.extra || uid.slice(0, 8), sub: 'Key ' + uid.slice(0, 12) }
  }
  if (dim === 'realm') return { main: r.key || '未知域', sub: '' }
  return { main: r.key || '未知模型', sub: '' }
}

function creditName(r: CreditAgg, dim: CreditDim): { main: string; sub: string } {
  if (dim === 'account') {
    const uid = r.key || ''
    return {
      main: r.nickname || (uid ? uid.slice(0, 8) : '—'),
      sub: (r.realm ? r.realm + ' · ' : '') + (uid ? uid.slice(0, 8) : '—'),
    }
  }
  if (dim === 'apikey') {
    const uid = r.key || ''
    if (!uid) return { main: '未按 Key 记录', sub: '升级前历史 / 未鉴权调用' }
    return { main: r.nickname || uid.slice(0, 8), sub: 'Key ' + uid.slice(0, 12) }
  }
  return { main: r.key || '未知模型', sub: '' }
}

function pickDimRows(d: Snapshot, dim: DimKey): KeyedAgg[] {
  if (dim === 'account') return d.by_account ?? []
  if (dim === 'model') return d.by_model ?? []
  if (dim === 'realm') return d.by_realm ?? []
  return d.by_api_key ?? []
}

function pickCreditRows(d: Snapshot, dim: CreditDim): CreditAgg[] {
  if (dim === 'account') return d.credit_by_account ?? []
  if (dim === 'model') return d.credit_by_model ?? []
  return d.credit_by_api_key ?? []
}

function sortValueOf(a: Agg, key: SortKey): number {
  if (key === 'requests') return a.requests
  if (key === 'errors') return a.errors
  if (key === 'avg_latency_ms') return a.avg_latency_ms
  return a.total_tokens
}

function fmtSortValue(v: number, key: SortKey): string {
  if (key === 'total_tokens') return tok(v)
  if (key === 'avg_latency_ms') return v > 0 ? Math.round(v) + ' ms' : '—'
  return num(v)
}

function fmtMs(v: number): string {
  if (!isFinite(v) || v <= 0) return '—'
  if (v >= 1000) return (v / 1000).toFixed(1).replace(/\.0$/, '') + ' s'
  return Math.round(v) + ' ms'
}

export default function Usage() {
  const { tick } = useRefresh()
  const toast = useToast()
  const busy = useBusy()

  const [range, setRange] = useState<RangeKey>('7d')
  const [fromText, setFromText] = useState(() => toInput(startOfToday()))
  const [toText, setToText] = useState(() => toInput(Date.now()))
  const [dim, setDim] = useState<DimKey>('account')
  const [sortKey, setSortKey] = useState<SortKey>('total_tokens')
  const [creditDim, setCreditDim] = useState<CreditDim>('account')
  const [pageDim, setPageDim] = useState(1)
  const [pageHour, setPageHour] = useState(1)
  const [pageCredit, setPageCredit] = useState(1)

  const pageSize = 10

  const win = useMemo(() => buildWindow(range, fromText, toText), [range, fromText, toText, tick])

  const q = useQuery<Snapshot>(
    (signal) => api.get<Snapshot>('/usage', { from: win.from, to: win.to }, signal),
    [tick, range, fromText, toText],
  )

  const saveNow = () => {
    void busy.run('save', async () => {
      try {
        await api.post('/usage/save')
        toast.ok('用量已落盘')
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const d = q.data

  if (!d) {
    return (
      <div className="page">
        <div className="pagehead">
          <div>
            <h2>用量</h2>
            <p className="lead">请求量、Token 与积分消耗</p>
          </div>
        </div>
        {q.error ? (
          <div className="notice bad">
            <span>{errorText(q.error)}</span>
            <button className="btn sm" onClick={q.reload}>
              重试
            </button>
          </div>
        ) : (
          <Box>
            <Skeleton rows={6} />
          </Box>
        )}
      </div>
    )
  }

  const t = d.totals
  const series = d.series ?? []
  const labels = series.map(pointLabel)

  const dimRows = pickDimRows(d, dim)
  const dimSorted = dimRows.slice().sort((a, b) => sortValueOf(b, sortKey) - sortValueOf(a, sortKey))
  const dimPages = Math.max(1, Math.ceil(dimSorted.length / pageSize))
  const dimCur = Math.min(pageDim, dimPages)
  const dimPaged = dimSorted.slice((dimCur - 1) * pageSize, dimCur * pageSize)

  const creditRows = pickCreditRows(d, creditDim).slice().sort((a, b) => b.credits - a.credits)
  const creditPages = Math.max(1, Math.ceil(creditRows.length / pageSize))
  const creditCur = Math.min(pageCredit, creditPages)
  const creditPaged = creditRows.slice((creditCur - 1) * pageSize, creditCur * pageSize)

  // 时段表按时间字符串降序：日桶 '2026-09-16' 与小时桶 '2026-09-16T13' 混排时不能简单 reverse。
  const hourRows = series.slice().sort((a, b) => (a.t < b.t ? 1 : a.t > b.t ? -1 : 0))
  const hourPages = Math.max(1, Math.ceil(hourRows.length / pageSize))
  const hourCur = Math.min(pageHour, hourPages)
  const hourPaged = hourRows.slice((hourCur - 1) * pageSize, hourCur * pageSize)

  const windowText =
    (d.window_from ? shortTime(d.window_from) : '不限起点') + ' → ' + (d.window_to ? shortTime(d.window_to) : '现在')
  const scopeText =
    windowText +
    ' · ' +
    num(d.buckets) +
    ' 个分桶' +
    (d.since ? ' · 数据自 ' + shortTime(d.since) : '') +
    ' · 文件 ' +
    bytes(d.file_bytes)

  const total = Math.max(0, t.total_tokens)
  const promptShare = total > 0 ? (t.prompt_tokens / total) * 100 : 0
  const completionShare = total > 0 ? (t.completion_tokens / total) * 100 : 0
  const successRate = t.requests > 0 ? ((t.requests - t.errors) / t.requests) * 100 : null
  const overallHit = hitRate(t)

  const accountCount = (d.by_account ?? []).length
  const modelCount = (d.by_model ?? []).length
  const keyCount = (d.by_api_key ?? []).length
  const creditRate = t.credit_tokens > 0 ? (t.credits / t.credit_tokens) * 1_000_000 : 0

  const topItems = dimSorted.slice(0, 10).map((r) => ({
    label: dimName(r, dim).main,
    value: sortValueOf(r, sortKey),
  }))

  const dimColumns: Column<KeyedAgg>[] = [
    {
      key: 'name',
      title: '名称',
      sortValue: (r) => dimName(r, dim).main,
      render: (r) => {
        const n = dimName(r, dim)
        return (
          <div>
            <div className="truncate">{n.main}</div>
            {n.sub ? <div className="tiny muted mono truncate">{n.sub}</div> : null}
          </div>
        )
      },
    },
    ...(dim === 'account'
      ? [
          {
            key: 'realm',
            title: '域',
            width: '72px',
            sortValue: (r: KeyedAgg) => r.realm || '',
            render: (r: KeyedAgg) => <span className="chip">{r.realm || '未知'}</span>,
          } as Column<KeyedAgg>,
        ]
      : []),
    {
      key: 'requests',
      title: '请求',
      align: 'right',
      sortValue: (r) => r.requests,
      render: (r) => num(r.requests),
    },
    {
      key: 'errors',
      title: '失败',
      align: 'right',
      sortValue: (r) => r.errors,
      render: (r) => (r.errors > 0 ? <span className="warn-text">{num(r.errors)}</span> : <span className="muted">—</span>),
    },
    {
      key: 'prompt_tokens',
      title: 'Prompt',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => r.prompt_tokens,
      render: (r) => tok(r.prompt_tokens),
    },
    {
      key: 'cache_hit_rate',
      title: '缓存命中',
      align: 'right',
      sortValue: (r) => hitRate(r) ?? -1,
      render: (r) => {
        const hr = hitRate(r)
        if (hr === null) return <span className="muted">—</span>
        return (
          <Badge tone={hitTone(hr)}>
            {pct(hr)}
            <span className="sr-only">
              {' 命中 ' + tok(r.cache_hit_tokens) + ' 未命中 ' + tok(r.cache_miss_tokens)}
            </span>
          </Badge>
        )
      },
    },
    {
      key: 'completion_tokens',
      title: 'Completion',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => r.completion_tokens,
      render: (r) => tok(r.completion_tokens),
    },
    {
      key: 'total_tokens',
      title: '合计 Token',
      align: 'right',
      width: '132px',
      sortValue: (r) => r.total_tokens,
      render: (r) => (
        <div className="stack gap1" style={{ gap: 4 }}>
          <span className="num">{tok(r.total_tokens)}</span>
          {r.total_tokens > 0 ? (
            <StackBar
              parts={[
                { label: 'Prompt', value: r.prompt_tokens, color: 'var(--accent)' },
                { label: 'Completion', value: r.completion_tokens, color: 'var(--ok)' },
              ]}
              height={5}
            />
          ) : null}
        </div>
      ),
    },
    ...(dim === 'account'
      ? [
          {
            key: 'perf',
            title: '均延迟 · 均速率',
            align: 'right',
            hideMobile: true,
            sortValue: (r: KeyedAgg) => r.avg_latency_ms,
            render: (r: KeyedAgg) => (
              <div>
                <div className="num small">{fmtMs(r.avg_latency_ms)}</div>
                <div className="tiny muted num">
                  {r.avg_tokens_per_second > 0 ? tok(r.avg_tokens_per_second) + '/s' : '—'}
                </div>
              </div>
            ),
          } as Column<KeyedAgg>,
        ]
      : []),
  ]

  const creditColumns: Column<CreditAgg>[] = [
    {
      key: 'name',
      title: '名称',
      sortValue: (r) => creditName(r, creditDim).main,
      render: (r) => {
        const n = creditName(r, creditDim)
        return (
          <div>
            <div className="truncate">{n.main}</div>
            {n.sub ? <div className="tiny muted mono truncate">{n.sub}</div> : null}
          </div>
        )
      },
    },
    ...(creditDim === 'model'
      ? [
          {
            key: 'rate',
            title: '积分倍率',
            align: 'right',
            width: '88px',
            sortValue: (r: CreditAgg) => r.rate ?? 0,
            render: (r: CreditAgg) => (r.rate ? <span className="mono small">x{r.rate}</span> : <span className="muted">—</span>),
          } as Column<CreditAgg>,
        ]
      : []),
    {
      key: 'requests',
      title: '请求',
      align: 'right',
      sortValue: (r) => r.requests,
      render: (r) => num(r.requests),
    },
    {
      key: 'credits',
      title: '扣除积分',
      align: 'right',
      sortValue: (r) => r.credits,
      render: (r) => fmtCredits(r.credits),
    },
    {
      key: 'credit_tokens',
      title: '有效样本 Token',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => r.credit_tokens,
      render: (r) => tok(r.credit_tokens),
    },
    {
      key: 'samples',
      title: '积分 / 1M Token',
      align: 'right',
      sortValue: (r) => r.credits_per_1m_tokens,
      render: (r) =>
        r.credit_tokens > 0 ? (
          <span className="num">{fmtCredits(r.credits_per_1m_tokens)}</span>
        ) : (
          <span className="muted">—</span>
        ),
    },
    {
      key: 'cache_hit_rate',
      title: '缓存命中',
      align: 'right',
      sortValue: (r) => hitRate(r) ?? -1,
      render: (r) => {
        const hr = hitRate(r)
        if (hr === null) return <span className="muted">—</span>
        return <Badge tone={hitTone(hr)}>{pct(hr)}</Badge>
      },
    },
  ]

  const hourColumns: Column<Point>[] = [
    {
      key: 't',
      title: '时段',
      sortValue: (r) => r.t,
      render: (r) => (
        <span className="mono small" title={r.t}>
          {pointLabel(r)}
        </span>
      ),
    },
    { key: 'requests', title: '请求', align: 'right', sortValue: (r) => r.requests, render: (r) => num(r.requests) },
    {
      key: 'prompt_tokens',
      title: 'Prompt',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => r.prompt_tokens,
      render: (r) => tok(r.prompt_tokens),
    },
    {
      key: 'completion_tokens',
      title: 'Completion',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => r.completion_tokens,
      render: (r) => tok(r.completion_tokens),
    },
    {
      key: 'total_tokens',
      title: '合计',
      align: 'right',
      sortValue: (r) => r.total_tokens,
      render: (r) => tok(r.total_tokens),
    },
    {
      key: 'credits',
      title: '积分',
      align: 'right',
      sortValue: (r) => r.credits,
      render: (r) => (r.credits > 0 ? fmtCredits(r.credits) : <span className="muted">—</span>),
    },
    {
      key: 'exchange',
      title: 'Token / 积分',
      align: 'right',
      sortValue: (r) => (r.credits > 0 ? r.total_tokens / r.credits : 0),
      render: (r) =>
        r.credits > 0 && r.total_tokens > 0 ? <span className="num">{tok(r.total_tokens / r.credits)}</span> : <span className="muted">—</span>,
    },
  ]

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>用量</h2>
          <p className="lead">{scopeText}</p>
        </div>
        <div className="acts">
          <button className="btn ghost" onClick={q.reload} disabled={q.loading}>
            {q.loading ? '刷新中…' : '刷新'}
          </button>
          <button className="btn" onClick={saveNow} disabled={busy.busy !== ''}>
            {busy.busy === 'save' ? '落盘中…' : '立即落盘'}
          </button>
        </div>
      </div>

      <div className="toolbar">
        <Segmented value={range} options={RANGE_OPTIONS} onChange={setRange} ariaLabel="时间范围" />
        {range === 'custom' ? (
          <div className="filters">
            <input
              className="input"
              type="datetime-local"
              aria-label="开始时间"
              value={fromText}
              onChange={(e) => setFromText(e.target.value)}
            />
            <span className="muted small">至</span>
            <input
              className="input"
              type="datetime-local"
              aria-label="结束时间"
              value={toText}
              onChange={(e) => setToText(e.target.value)}
            />
          </div>
        ) : null}
      </div>

      {q.error ? (
        <div className="notice bad">
          <span>{errorText(q.error)}</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      {d.last_error ? (
        <div className="notice warn">
          用量文件已 {num(d.failed_flushes ?? 0)} 轮没写成功：{d.last_error}
        </div>
      ) : null}

      <div className="muted small truncate" title={scopeText}>
        {scopeText}
      </div>

      <StatsGrid>
        <Stat
          label="请求数"
          value={num(t.requests)}
          sub={t.errors > 0 ? '其中失败 ' + num(t.errors) + ' 次' : '全部成功'}
          tone={t.errors > 0 ? 'warn' : undefined}
        />
        <Stat
          label="总 Token"
          value={tok(t.total_tokens)}
          sub={'prompt ' + pct(promptShare) + ' · completion ' + pct(completionShare)}
        />
        <Stat
          label="总积分"
          value={t.credit_samples > 0 ? fmtCredits(t.credits) : '—'}
          sub={t.credit_samples > 0 ? '实测样本 ' + num(t.credit_samples) + ' 条' : '窗口内没有实测扣费样本'}
        />
        <Stat
          label="平均缓存命中率"
          value={overallHit === null ? '—' : pct(overallHit)}
          sub={'命中 ' + tok(t.cache_hit_tokens) + ' · 未命中 ' + tok(t.cache_miss_tokens)}
          tone={overallHit === null ? undefined : hitTone(overallHit)}
        />
        <Stat label="Prompt" value={tok(t.prompt_tokens)} sub={'占比 ' + pct(promptShare)} />
        <Stat label="Completion" value={tok(t.completion_tokens)} sub={'占比 ' + pct(completionShare)} />
        <Stat
          label="失败尝试"
          value={num(t.errors)}
          sub={successRate === null ? '窗口内无请求' : '成功率 ' + pct(successRate)}
          tone={t.errors > 0 ? 'bad' : 'ok'}
        />
        <Stat
          label="平均延迟"
          value={fmtMs(t.avg_latency_ms)}
          sub={t.avg_tokens_per_second > 0 ? '吐字 ' + tok(t.avg_tokens_per_second) + '/s' : '无吐字样本'}
        />
      </StatsGrid>

      {t.requests === 0 && series.length === 0 ? (
        <Box>
          <Empty
            title="这个窗口内没有请求"
            hint="换一个时间范围，或等有流量后再看"
            action={
              <button className="btn" onClick={() => setRange('30d')}>
                看近 30 天
              </button>
            }
          />
        </Box>
      ) : (
        <>
          <div className="grid2">
            <Box title="请求量" sub="请求数与失败数">
              <LineChart
                labels={labels}
                series={[
                  { label: '请求', points: series.map((p) => p.requests) },
                  { label: '失败', points: series.map((p) => p.errors), color: 'var(--bad)' },
                ]}
                height={200}
                formatY={num}
                emptyText="窗口内没有请求"
              />
            </Box>
            <Box title="Token 消耗" sub="prompt 与 completion 分开看">
              <LineChart
                labels={labels}
                series={[
                  { label: 'Prompt', points: series.map((p) => p.prompt_tokens) },
                  { label: 'Completion', points: series.map((p) => p.completion_tokens), color: 'var(--ok)' },
                ]}
                height={200}
                formatY={tok}
                emptyText="窗口内没有 Token 记录"
              />
            </Box>
            <Box title="缓存命中率" sub="无样本的时段留空">
              <LineChart
                labels={labels}
                series={[{ label: '命中率 %', points: series.map((p) => hitRate(p)) }]}
                height={200}
                formatY={(v) => pct(v, 0)}
                emptyText="窗口内没有缓存观测样本"
              />
            </Box>
          </div>

          <Box
            title="维度 Top 10"
            sub={'按' + (SORT_OPTIONS.find((o) => o.value === sortKey)?.label ?? '') + '降序，下方明细同口径'}
          >
            <Tabs
              value={dim}
              options={DIM_TABS.map((o) => ({
                value: o.value,
                label: o.label,
                badge: num(pickDimRows(d, o.value).length),
              }))}
              onChange={(v) => {
                setDim(v)
                setPageDim(1)
              }}
            />
            <div className="toolbar" style={{ marginTop: 12 }}>
              <label className="tiny muted" htmlFor="usage-sort">
                排序
              </label>
              <select
                id="usage-sort"
                className="select xs"
                value={sortKey}
                onChange={(e) => {
                  setSortKey(e.target.value as SortKey)
                  setPageDim(1)
                }}
              >
                {SORT_OPTIONS.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            </div>
            <BarChart
              items={topItems}
              horizontal
              height={10 + topItems.length * 26}
              format={(v) => fmtSortValue(v, sortKey)}
              emptyText="该维度在这个窗口内没有数据"
            />
          </Box>

          <Box
            title="维度明细"
            sub={num(dimSorted.length) + ' 行 · 请求数含失败尝试'}
            footer={
              <Pager page={dimCur} pageSize={pageSize} total={dimSorted.length} onPage={setPageDim} />
            }
          >
            <DataTable
              key={dim + '|' + sortKey}
              columns={dimColumns}
              rows={dimPaged}
              rowKey={(r) => r.realm + '|' + r.key}
              loading={q.loading}
              empty="该维度没有数据"
              emptyHint="换一个时间范围，或切到其他维度"
              initialSort={{ key: sortKey, dir: 'desc' }}
            />
          </Box>

          <Box title="积分扣除" sub={num(creditRows.length) + ' 行 · 按扣除积分降序'}>
            <Tabs
              value={creditDim}
              options={CREDIT_TABS.map((o) => ({
                value: o.value,
                label: o.label,
                badge: num(pickCreditRows(d, o.value).length),
              }))}
              onChange={(v) => {
                setCreditDim(v)
                setPageCredit(1)
              }}
            />
            <div style={{ marginTop: 12 }}>
              <StatsGrid>
                <Stat label="扣除积分" value={t.credit_samples > 0 ? fmtCredits(t.credits) : '—'} sub="按上游 usage.credit 累计" />
                <Stat label="匹配 Token" value={tok(t.credit_tokens)} sub="与积分同时观测到的 Token" />
                <Stat
                  label="积分 / 1M Token"
                  value={t.credit_tokens > 0 ? fmtCredits(creditRate) : '—'}
                  sub="越低越划算"
                  tone="accent"
                />
                <Stat label="有效积分样本" value={num(t.credit_samples)} sub="缺字段的历史不参与折算" />
                <Stat
                  label="缓存命中率"
                  value={overallHit === null ? '—' : pct(overallHit)}
                  sub="低命中意味着费用数倍放大"
                  tone={overallHit === null ? undefined : hitTone(overallHit)}
                />
              </StatsGrid>
            </div>
            <div className="muted small truncate" style={{ marginTop: 8 }}>
              {num(accountCount) +
                ' 个账号 · ' +
                num(modelCount) +
                ' 个模型倍率分组 · ' +
                num(keyCount) +
                ' 个 API Key · 仅统计与积分同时观测到的 Token'}
            </div>
            <DataTable
              key={creditDim}
              columns={creditColumns}
              rows={creditPaged}
              rowKey={(r) => r.realm + '|' + r.key}
              loading={q.loading}
              empty="暂无积分扣除记录；升级前仅含 Token 的历史不会伪造积分"
              emptyHint="等有观测到 usage.credit 的请求后再看"
              initialSort={{ key: 'credits', dir: 'desc' }}
            />
            <Pager page={creditCur} pageSize={pageSize} total={creditRows.length} onPage={setPageCredit} />
          </Box>

          <Box
            title="积分与 token 按小时"
            sub={'共 ' + num(hourRows.length) + ' 个时段 · 最新在上'}
            footer={
              <div className="row row-wrap gap2 small muted">
                <span>积分取上游 usage.credit（没观测到的请求不计）</span>
                <span>兑换率 = token ÷ 积分</span>
              </div>
            }
          >
            <DataTable
              columns={hourColumns}
              rows={hourPaged}
              rowKey={(r) => r.t}
              loading={q.loading}
              empty="窗口内没有时段数据"
              emptyHint="换一个时间范围试试"
              initialSort={{ key: 't', dir: 'desc' }}
            />
            <Pager page={hourCur} pageSize={pageSize} total={hourRows.length} onPage={setPageHour} />
          </Box>
        </>
      )}
    </div>
  )
}
