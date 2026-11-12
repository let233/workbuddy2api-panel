import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useBusy, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { credits as fmtCredits, num, pct, shortTime, tok } from '../lib/format'
import { Box, Empty, Pager, Segmented, Skeleton, Stat, StatsGrid } from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'
import { BarChart, LineChart } from '../components/charts'

// 字段口径与 internal/usage/usage.go 的 Snapshot/Agg/KeyedAgg/Point 一一对应。
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

type Snapshot = {
  totals: Agg
  by_realm: KeyedAgg[] | null
  by_account: KeyedAgg[] | null
  by_model: KeyedAgg[] | null
  by_api_key: KeyedAgg[] | null
  series: Point[] | null
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
type DimKey = 'account' | 'apikey' | 'model'
type MetricKey = 'requests' | 'tokens' | 'credits'

const RANGE_OPTIONS: { value: RangeKey; label: string }[] = [
  { value: 'today', label: '今天' },
  { value: '7d', label: '7 天' },
  { value: '30d', label: '30 天' },
  { value: 'custom', label: '自定义' },
]

const DIM_OPTIONS: { value: DimKey; label: string }[] = [
  { value: 'account', label: '账号' },
  { value: 'apikey', label: 'API Key' },
  { value: 'model', label: '模型' },
]

const METRIC_OPTIONS: { value: MetricKey; label: string }[] = [
  { value: 'requests', label: '请求' },
  { value: 'tokens', label: 'Token' },
  { value: 'credits', label: '积分' },
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
function pointLabel(p: Point): string {
  const t = p.t || ''
  if (!t) return '-'
  if (p.scope === 'day' || t.length <= 10) return t.slice(5)
  return t.slice(5, 10) + ' ' + t.slice(11, 13) + ':00'
}

function dimLabel(r: KeyedAgg, dim: DimKey): string {
  if (dim === 'account') {
    const name = r.extra || (r.key ? r.key.slice(0, 8) : '未知账号')
    return r.realm ? name + ' · ' + r.realm : name
  }
  if (dim === 'apikey') {
    if (r.extra) return r.extra
    return r.key ? 'Key ' + r.key.slice(0, 8) : '未鉴权 / 历史数据'
  }
  return r.key || '未知模型'
}

function metricValue(r: KeyedAgg, m: MetricKey): number {
  if (m === 'tokens') return r.total_tokens
  if (m === 'credits') return r.credits
  return r.requests
}

export default function Usage() {
  const { tick } = useRefresh()
  const toast = useToast()
  const busy = useBusy()

  const [range, setRange] = useState<RangeKey>('7d')
  const [fromText, setFromText] = useState(() => toInput(startOfToday()))
  const [toText, setToText] = useState(() => toInput(Date.now()))
  const [dim, setDim] = useState<DimKey>('account')
  const [metric, setMetric] = useState<MetricKey>('requests')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(15)

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
  const detailRows = series.slice().reverse()
  const pages = Math.max(1, Math.ceil(detailRows.length / pageSize))
  const cur = Math.min(page, pages)
  const paged = detailRows.slice((cur - 1) * pageSize, cur * pageSize)

  const dimRows = dim === 'account' ? d.by_account ?? [] : dim === 'apikey' ? d.by_api_key ?? [] : d.by_model ?? []
  const top = dimRows
    .slice()
    .sort((a, b) => metricValue(b, metric) - metricValue(a, metric))
    .slice(0, 10)
    .map((r) => ({ label: dimLabel(r, dim), value: metricValue(r, metric) }))

  const metricLabel = metric === 'requests' ? '请求量' : metric === 'tokens' ? 'Token' : '积分'
  const metricFormat = (v: number) => (metric === 'tokens' ? tok(v) : metric === 'credits' ? fmtCredits(v) : num(v))
  const dimName = DIM_OPTIONS.find((o) => o.value === dim)?.label ?? '账号'
  const windowText =
    (d.window_from ? shortTime(d.window_from) : '不限起点') +
    ' → ' +
    (d.window_to ? shortTime(d.window_to) : '现在')

  const columns: Column<Point>[] = [
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
      key: 'errors',
      title: '失败',
      align: 'right',
      sortValue: (r) => r.errors,
      render: (r) => (r.errors > 0 ? <span className="warn-text">{num(r.errors)}</span> : <span className="muted">0</span>),
    },
    { key: 'prompt', title: 'Prompt', align: 'right', sortValue: (r) => r.prompt_tokens, render: (r) => tok(r.prompt_tokens) },
    {
      key: 'completion',
      title: 'Completion',
      align: 'right',
      sortValue: (r) => r.completion_tokens,
      render: (r) => tok(r.completion_tokens),
    },
    { key: 'total', title: '合计 Token', align: 'right', sortValue: (r) => r.total_tokens, render: (r) => tok(r.total_tokens) },
    {
      key: 'cache',
      title: '缓存命中',
      align: 'right',
      sortValue: (r) => r.cache_hit_rate ?? 0,
      render: (r) => (r.cache_hit_rate === undefined ? '—' : pct(r.cache_hit_rate)),
    },
    {
      key: 'credits',
      title: '积分',
      align: 'right',
      sortValue: (r) => r.credits,
      render: (r) => (r.credit_samples > 0 ? fmtCredits(r.credits) : <span className="muted">—</span>),
    },
    {
      key: 'latency',
      title: '平均延迟',
      align: 'right',
      sortValue: (r) => r.avg_latency_ms,
      render: (r) => (r.avg_latency_ms > 0 ? Math.round(r.avg_latency_ms) + ' ms' : '—'),
      hideMobile: true,
    },
  ]

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>用量</h2>
          <p className="lead">
            请求量、Token 与积分消耗 · {windowText} · 生成于 {shortTime(d.generated)}
          </p>
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
          用量文件已 {d.failed_flushes ?? 0} 轮没写成功：{d.last_error}
        </div>
      ) : null}

      <StatsGrid>
        <Stat label="请求数" value={num(t.requests)} sub={'失败 ' + num(t.errors)} tone={t.errors > 0 ? 'warn' : undefined} />
        <Stat
          label="总 Token"
          value={tok(t.total_tokens)}
          sub={'prompt ' + tok(t.prompt_tokens) + ' · completion ' + tok(t.completion_tokens)}
        />
        <Stat
          label="积分"
          value={t.credit_samples > 0 ? fmtCredits(t.credits) : '—'}
          sub={t.credit_samples > 0 ? '样本 ' + num(t.credit_samples) + ' 次' : '窗口内没有实测扣费样本'}
        />
        <Stat
          label="缓存命中率"
          value={t.cache_hit_tokens + t.cache_miss_tokens > 0 ? pct(t.cache_hit_rate ?? 0) : '—'}
          sub={'命中 ' + tok(t.cache_hit_tokens) + ' / 未命中 ' + tok(t.cache_miss_tokens)}
        />
        <Stat label="平均延迟" value={t.avg_latency_ms > 0 ? Math.round(t.avg_latency_ms) + ' ms' : '—'} sub={'共 ' + num(d.buckets) + ' 个时段'} />
        <Stat label="平均速度" value={t.avg_tokens_per_second > 0 ? tok(t.avg_tokens_per_second) + '/s' : '—'} sub="按请求加权" />
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
            <Box title="请求量" sub="每小时/每天请求数与失败数">
              <LineChart labels={labels} series={[
                { label: '请求', points: series.map((p) => p.requests) },
                { label: '失败', points: series.map((p) => p.errors) },
              ]} height={220} formatY={num} emptyText="窗口内没有请求" />
            </Box>
            <Box title="Token 消耗" sub="prompt 与 completion 分开看">
              <LineChart labels={labels} series={[
                { label: 'Prompt', points: series.map((p) => p.prompt_tokens) },
                { label: 'Completion', points: series.map((p) => p.completion_tokens) },
              ]} height={220} formatY={tok} emptyText="窗口内没有 Token 记录" />
            </Box>
          </div>

          <Box title={'Top 10 · 按' + dimName} sub={'当前按' + metricLabel + '排序，点上方维度切换口径'}>
            <div className="toolbar">
              <Segmented value={dim} options={DIM_OPTIONS} onChange={setDim} ariaLabel="分组维度" />
              <span className="sep" />
              <Segmented value={metric} options={METRIC_OPTIONS} onChange={setMetric} ariaLabel="排序指标" />
            </div>
            <BarChart items={top} horizontal format={metricFormat} height={260} emptyText="该维度在这个窗口内没有数据" />
          </Box>

          <Box
            title="时段明细"
            sub={'共 ' + num(detailRows.length) + ' 条 · 最新在上'}
            footer={
              <Pager
                page={cur}
                pageSize={pageSize}
                total={detailRows.length}
                onPage={setPage}
                onPageSize={(n) => {
                  setPageSize(n)
                  setPage(1)
                }}
              />
            }
          >
            <DataTable
              columns={columns}
              rows={paged}
              rowKey={(r) => r.t}
              loading={q.loading}
              empty="窗口内没有时段数据"
              emptyHint="换一个时间范围试试"
              initialSort={{ key: 't', dir: 'desc' }}
            />
          </Box>
        </>
      )}
    </div>
  )
}
