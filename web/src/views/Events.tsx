import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useDebounced, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { bytes, credits, num, pct, shortTime, tok } from '../lib/format'
import {
  Badge,
  Box,
  ConfirmDialog,
  Empty,
  KV,
  Modal,
  Pager,
  Segmented,
  Skeleton,
  Stat,
  StatsGrid,
} from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'

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
  ttfb_ms?: number
  attempts?: number
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
  credit?: number
  credit_known?: boolean
  cache_hit_tokens?: number
  cache_miss_tokens?: number
  client_ip?: string
  user_agent?: string
  mode?: string
  reasoning_effort?: string
  reasoning_effort_requested?: string
  reasoning_tokens?: number
  executor_type?: string
  tools_declared?: number
  tool_calls?: number
  context?: unknown
}

type RequestLogsResp = {
  entries: ReqEvent[] | null
  limit: number
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
  started_at: string
  completed: number
  in_flight: number
  succeeded: number
  failed: number
  success_rate: number
  http_success_rate: number
  avg_duration_ms: number
  archive: ArchiveStats
}

type StatusFilter = 'all' | 'ok' | 'rate' | 'fail'
type RangeKey = '15m' | '1h' | '6h' | '24h' | '7d' | 'all'

const STATUS_OPTIONS: { value: StatusFilter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'ok', label: '成功' },
  { value: 'rate', label: '限流' },
  { value: 'fail', label: '失败' },
]

const RANGES: { value: RangeKey; label: string; ms: number }[] = [
  { value: '15m', label: '近 15 分钟', ms: 15 * 60 * 1000 },
  { value: '1h', label: '近 1 小时', ms: 60 * 60 * 1000 },
  { value: '6h', label: '近 6 小时', ms: 6 * 60 * 60 * 1000 },
  { value: '24h', label: '近 24 小时', ms: 24 * 60 * 60 * 1000 },
  { value: '7d', label: '近 7 天', ms: 7 * 24 * 60 * 60 * 1000 },
  { value: 'all', label: '全部时间', ms: 0 },
]

const OUTCOME_LABEL: Record<string, string> = {
  success: '成功',
  http_error: 'HTTP 错误',
  stream_error: '流中断',
  interrupted: '已中断',
}

function timeMs(e: ReqEvent): number {
  const t = new Date(e.time).getTime()
  return isNaN(t) ? 0 : t
}

function statusTone(e: ReqEvent): 'ok' | 'warn' | 'bad' | 'muted' {
  if (e.ok) return 'ok'
  if (e.status === 429) return 'warn'
  if (e.status >= 400) return 'bad'
  return 'muted'
}

function reasonOf(e: ReqEvent): string {
  if (e.ok) return '-'
  if (e.outcome === 'interrupted') return '客户端中断'
  if (e.outcome === 'stream_error') return '响应流中断'
  if (e.status === 429) return '429 限流'
  if (e.status === 401 || e.status === 403) return `${e.status} 鉴权/权限`
  if (e.status >= 500) return `${e.status} 上游错误`
  if (e.status >= 400) return `HTTP ${e.status}`
  return OUTCOME_LABEL[e.outcome] ?? (e.outcome || '未知')
}

export default function Events() {
  const { tick } = useRefresh()
  const toast = useToast()

  const [status, setStatus] = useState<StatusFilter>('all')
  const [range, setRange] = useState<RangeKey>('24h')
  const [account, setAccount] = useState('')
  const [model, setModel] = useState('')
  const [keyword, setKeyword] = useState('')
  const acc = useDebounced(account, 300).trim()
  const mdl = useDebounced(model, 300).trim()
  const kw = useDebounced(keyword, 300).trim().toLowerCase()

  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [selected, setSelected] = useState<ReqEvent | null>(null)
  const [confirmClear, setConfirmClear] = useState(false)
  const [clearing, setClearing] = useState(false)

  const rangeMs = RANGES.find((r) => r.value === range)?.ms ?? 0

  const q = useQuery<RequestLogsResp>(
    (signal) =>
      api.get<RequestLogsResp>(
        '/request_logs',
        {
          limit: 1000,
          account: acc === '' ? undefined : acc,
          model: mdl === '' ? undefined : mdl,
          from: rangeMs > 0 ? Math.floor((Date.now() - rangeMs) / 1000) : undefined,
        },
        signal,
      ),
    [tick, acc, mdl, rangeMs],
    { poll: 10000 },
  )

  const m = useQuery<MetricsResp>(
    (signal) => api.get<MetricsResp>('/request_metrics', { recent: 0 }, signal),
    [tick],
    { poll: 15000 },
  )

  const rows = useMemo(() => {
    const list = q.data?.entries ?? []
    const filtered = list.filter((e) => {
      if (status === 'ok' && !e.ok) return false
      if (status === 'rate' && e.status !== 429) return false
      if (status === 'fail' && (e.ok || e.status === 429)) return false
      if (kw !== '') {
        const hay = [e.account, e.model, e.model_eff, e.path, e.request_id, e.client_ip]
          .filter((x): x is string => typeof x === 'string')
          .join(' ')
          .toLowerCase()
        if (!hay.includes(kw)) return false
      }
      return true
    })
    return filtered.slice().sort((a, b) => timeMs(b) - timeMs(a))
  }, [q.data, status, kw])

  const pageCount = Math.max(1, Math.ceil(rows.length / pageSize))
  const current = Math.min(page, pageCount)
  const paged = rows.slice((current - 1) * pageSize, current * pageSize)

  const clearArchive = () => {
    setClearing(true)
    api
      .post<{ ok: boolean; rows: number; bytes: number }>('/request_logs/clear')
      .then((res) => {
        toast.ok(`已清空归档 ${num(res.rows)} 条（${bytes(res.bytes)}）`)
        setConfirmClear(false)
        q.reload()
        m.reload()
      })
      .catch((e: unknown) => {
        toast.err(errorText(e))
      })
      .finally(() => {
        setClearing(false)
      })
  }

  const columns: Column<ReqEvent>[] = [
    {
      key: 'time',
      title: '时间',
      mono: true,
      sortValue: (e) => timeMs(e),
      render: (e) => shortTime(e.time),
    },
    {
      key: 'account',
      title: '账号',
      render: (e) => e.account || <span className="dim">-</span>,
    },
    {
      key: 'model',
      title: '模型',
      mono: true,
      hideMobile: true,
      render: (e) => (
        <span className="truncate" title={e.model_eff ? `${e.model} → ${e.model_eff}` : e.model}>
          {e.model || '-'}
          {e.model_eff ? <span className="muted tiny"> → {e.model_eff}</span> : null}
        </span>
      ),
    },
    {
      key: 'status',
      title: '状态',
      render: (e) => <Badge tone={statusTone(e)}>{e.status === 0 ? OUTCOME_LABEL[e.outcome] ?? '0' : String(e.status)}</Badge>,
    },
    {
      key: 'duration',
      title: '耗时',
      align: 'right',
      mono: true,
      sortValue: (e) => e.duration_ms,
      render: (e) => `${num(e.duration_ms)} ms`,
    },
    {
      key: 'tokens',
      title: 'Token',
      align: 'right',
      mono: true,
      hideMobile: true,
      sortValue: (e) => e.total_tokens ?? 0,
      render: (e) => (e.total_tokens ? tok(e.total_tokens) : '-'),
    },
    {
      key: 'reason',
      title: '结果',
      render: (e) =>
        e.ok ? <span className="muted">成功</span> : <span className="warn-text">{reasonOf(e)}</span>,
    },
  ]

  const detailItems = selected
    ? [
        { k: '请求 ID', v: selected.request_id, mono: true },
        { k: '时间', v: new Date(selected.time).toLocaleString('zh-CN'), mono: true },
        { k: '路径', v: selected.path || '-', mono: true },
        { k: '账号', v: selected.account || '-' },
        { k: '请求模型', v: selected.model || '-', mono: true },
        { k: '实际模型', v: selected.model_eff || '未降级', mono: true },
        { k: '状态码', v: selected.status === 0 ? '无响应' : String(selected.status), mono: true },
        { k: '结果', v: OUTCOME_LABEL[selected.outcome] ?? selected.outcome },
        { k: '失败原因', v: reasonOf(selected) },
        { k: '总耗时', v: `${num(selected.duration_ms)} ms`, mono: true },
        { k: '首字节', v: selected.ttfb_ms ? `${num(selected.ttfb_ms)} ms` : '-', mono: true },
        { k: '重试次数', v: String(selected.attempts ?? 0) },
        {
          k: 'Token',
          v: `输入 ${num(selected.prompt_tokens ?? 0)} / 输出 ${num(selected.completion_tokens ?? 0)} / 合计 ${num(
            selected.total_tokens ?? 0,
          )}`,
          mono: true,
        },
        {
          k: '缓存 Token',
          v:
            selected.cache_hit_tokens || selected.cache_miss_tokens
              ? `命中 ${num(selected.cache_hit_tokens ?? 0)} / 未命中 ${num(selected.cache_miss_tokens ?? 0)}`
              : '-',
          mono: true,
        },
        { k: '消费积分', v: selected.credit_known ? credits(selected.credit ?? 0) : '未知', mono: true },
        { k: '客户端 IP', v: selected.client_ip || '-' },
        { k: 'User-Agent', v: selected.user_agent || '-' },
        { k: '模式', v: selected.mode || '-' },
        {
          k: '思考档位',
          v: `请求 ${selected.reasoning_effort_requested || '未携带'} / 生效 ${selected.reasoning_effort || '-'}`,
        },
        { k: '推理 Token', v: num(selected.reasoning_tokens ?? 0), mono: true },
        { k: '工具', v: `声明 ${selected.tools_declared ?? 0} / 调用 ${selected.tool_calls ?? 0}` },
        { k: '执行器', v: selected.executor_type || '-' },
      ]
    : []

  const md = m.data

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>请求事件</h2>
          <p className="lead">网关转发的每一次请求：状态、耗时、Token 与失败原因，每 10 秒刷新</p>
        </div>
        <div className="acts">
          <button className="btn sm" onClick={() => { q.reload(); m.reload() }} disabled={q.loading}>
            刷新
          </button>
          <button className="btn sm danger" onClick={() => setConfirmClear(true)}>
            清空归档
          </button>
        </div>
      </div>

      {md ? (
        <StatsGrid>
          <Stat label="完成请求" value={num(md.completed)} sub={`在途 ${num(md.in_flight)}`} />
          <Stat label="成功率" value={pct(md.success_rate)} sub={`成功 ${num(md.succeeded)} · 失败 ${num(md.failed)}`} />
          <Stat label="HTTP 成功率" value={pct(md.http_success_rate)} sub="按状态码口径" />
          <Stat label="平均耗时" value={`${num(Math.round(md.avg_duration_ms))} ms`} sub={`自 ${shortTime(md.started_at)} 起`} />
          <Stat
            label="归档"
            value={md.archive.enabled ? num(md.archive.rows) + ' 条' : '未启用'}
            sub={md.archive.enabled ? `${bytes(md.archive.bytes)} · ${md.archive.days} 天` : '仅内存指标'}
          />
        </StatsGrid>
      ) : m.error ? (
        <div className="notice bad">
          <span>指标读取失败：{errorText(m.error)}</span>
          <button className="btn sm" onClick={m.reload}>
            重试
          </button>
        </div>
      ) : (
        <Box>
          <Skeleton rows={2} />
        </Box>
      )}

      {md?.archive.last_error ? (
        <div className="notice warn">
          <span>归档写入异常：{md.archive.last_error}</span>
        </div>
      ) : null}
      {q.error ? (
        <div className="notice bad">
          <span>事件读取失败：{errorText(q.error)}</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      <Box title="事件明细" sub={`已取回最近 ${num((q.data?.entries ?? []).length)} 条`}>
        <div className="filters">
          <div className="segmented-scroll">
            <Segmented<StatusFilter>
              value={status}
              options={STATUS_OPTIONS}
              onChange={(v) => {
                setStatus(v)
                setPage(1)
              }}
              ariaLabel="状态筛选"
            />
          </div>
          <select
            className="select"
            value={range}
            onChange={(e) => { setRange(e.target.value as RangeKey); setPage(1) }}
            aria-label="时间范围"
          >
            {RANGES.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </select>
          <input
            className="input"
            placeholder="账号"
            value={account}
            onChange={(e) => { setAccount(e.target.value); setPage(1) }}
            aria-label="按账号筛选"
          />
          <input
            className="input"
            placeholder="模型"
            value={model}
            onChange={(e) => { setModel(e.target.value); setPage(1) }}
            aria-label="按模型筛选"
          />
          <input
            className="input"
            placeholder="关键字（请求 ID / 路径 / IP）"
            value={keyword}
            onChange={(e) => { setKeyword(e.target.value); setPage(1) }}
            aria-label="关键字"
          />
        </div>

        <DataTable<ReqEvent>
          columns={columns}
          rows={paged}
          rowKey={(e) => e.request_id}
          loading={q.first && q.loading}
          empty="没有匹配的请求事件"
          emptyHint="请求事件来自网关转发记录；放宽时间范围或清空筛选后再试。"
          onRowClick={(e) => setSelected(e)}
          caption="点击任意一行查看完整详情"
        />
      </Box>

      {rows.length > 0 ? (
        <Pager
          page={current}
          pageSize={pageSize}
          total={rows.length}
          onPage={setPage}
          onPageSize={(n) => {
            setPageSize(n)
            setPage(1)
          }}
        />
      ) : null}

      <Modal
        open={selected !== null}
        title="请求详情"
        onClose={() => setSelected(null)}
        wide
        footer={
          <button className="btn" onClick={() => setSelected(null)}>
            关闭
          </button>
        }
      >
        {selected ? (
          <div>
            {!selected.ok ? (
              <div className="notice bad">
                <span>失败原因：{reasonOf(selected)}</span>
              </div>
            ) : null}
            <KV items={detailItems} />
            {selected.context ? (
              <div className="col">
                <div className="tiny muted">会话上下文（已脱敏记录）</div>
                <pre className="pre">{JSON.stringify(selected.context, null, 2)}</pre>
              </div>
            ) : null}
          </div>
        ) : (
          <Empty title="未选择事件" />
        )}
      </Modal>

      <ConfirmDialog
        open={confirmClear}
        title="清空请求归档"
        message="将删除归档库中的全部请求事件记录，此操作不可恢复；内存指标会在下一次刷新重算。"
        confirmText="清空归档"
        danger
        busy={clearing}
        onConfirm={clearArchive}
        onClose={() => setConfirmClear(false)}
      />
    </div>
  )
}
