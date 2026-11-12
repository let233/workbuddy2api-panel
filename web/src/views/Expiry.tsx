// Expiry.tsx 积分到期：按 UTC+8 自然日聚合的到期分布 + 临期高亮明细 + CSV 导出。
import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { ago, credits, cstDate, duration, num } from '../lib/format'
import { Badge, Box, Segmented, Skeleton, Stat, StatsGrid, type Tone } from '../components/ui'
import { BarChart } from '../components/charts'
import { DataTable, type Column } from '../components/DataTable'

type Bucket = { date?: string; expires_at?: string; remain: number }

type ExpiryRow = {
  uid: string
  nickname?: string
  realm?: string
  credits?: number
  credits_expiry_buckets?: Bucket[]
  credits_earliest_expiry?: string
  credits_earliest_remaining?: number
}

type ExpiryResp = {
  accounts?: ExpiryRow[]
  count?: number
  balance_refresh_sec?: number
  balance_refreshed_at?: string
}

type Model = {
  uid: string
  nickname: string
  realm: string
  credits: number
  buckets: Bucket[]
  expiryAt: string
  remain: number
  days: number | null
  within7: number
  within30: number
}

type Filter = 'all' | 'near'

const FILTER_OPTIONS: { value: Filter; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'near', label: '7 天内' },
]

/** UTC+8 自然日编号：跨时区比较日期不能用浏览器本地时区。 */
function cstDay(ms: number): number {
  return Math.floor((ms + 8 * 3600 * 1000) / 86400000)
}

function msOf(v?: string): number {
  if (!v) return 0
  const t = Date.parse(v)
  return Number.isNaN(t) ? 0 : t
}

/** 到期离今天还有几天（UTC+8 自然日，今天 = 0，已过 = 负数）。 */
function dayDiff(v?: string): number | null {
  const ms = msOf(v)
  if (!ms) return null
  return cstDay(ms) - cstDay(Date.now())
}

function daysText(d: number | null): string {
  if (d === null) return '无到期日'
  if (d < 0) return '已过期'
  if (d === 0) return '今天到期'
  if (d === 1) return '明天到期'
  return d + ' 天后到期'
}

function toneOf(d: number | null): Tone {
  if (d === null) return 'muted'
  if (d <= 3) return 'bad'
  if (d <= 7) return 'warn'
  return 'ok'
}

function nameOf(m: Model): string {
  return m.nickname || m.uid.slice(0, 8)
}

function bucketDay(b: Bucket): string {
  return b.expires_at || b.date || ''
}

/** 余额刷新时间的文案；零值时间（0001-01-01）当作没刷过，返回空串。 */
function refreshText(v?: string): string {
  if (!v) return ''
  const t = Date.parse(v)
  if (Number.isNaN(t) || new Date(t).getFullYear() < 2000) return ''
  return ago(v) + '更新'
}

function csvCell(v: string): string {
  let s = v
  const c = s.charAt(0)
  if (c === '=' || c === '+' || c === '-' || c === '@') s = "'" + s
  if (s.indexOf('"') >= 0 || s.indexOf(',') >= 0 || s.indexOf('\n') >= 0 || s.indexOf('\r') >= 0) {
    return '"' + s.split('"').join('""') + '"'
  }
  return s
}

/** 本地时间戳（导出文件名不能用 toISOString，那是 UTC）。 */
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

export default function Expiry() {
  const { tick } = useRefresh()
  const toast = useToast()
  const [filter, setFilter] = useState<Filter>('all')

  const q = useQuery<ExpiryResp>((signal) => api.get('/expiry', undefined, signal), [tick], {
    poll: 30000,
  })
  const data = q.data

  const models: Model[] = useMemo(() => {
    const list = data?.accounts ?? []
    return list.map((r) => {
      const buckets = (r.credits_expiry_buckets ?? []).filter((b) => b.remain > 0)
      const future = buckets.filter((b) => {
        const d = dayDiff(bucketDay(b))
        return d !== null && d >= 0
      })
      const first = future.length
        ? future.reduce((a, b) => (msOf(bucketDay(b)) < msOf(bucketDay(a)) ? b : a))
        : undefined
      const expiryAt = r.credits_earliest_expiry || (first ? bucketDay(first) : '')
      const remain = r.credits_earliest_expiry ? (r.credits_earliest_remaining ?? 0) : first ? first.remain : 0
      let within7 = 0
      let within30 = 0
      for (const b of buckets) {
        const d = dayDiff(bucketDay(b))
        if (d === null || d < 0) continue
        if (d <= 7) within7 += b.remain
        if (d <= 30) within30 += b.remain
      }
      return {
        uid: r.uid,
        nickname: (r.nickname || '').trim(),
        realm: r.realm || 'cn',
        credits: r.credits ?? 0,
        buckets,
        expiryAt,
        remain,
        days: dayDiff(expiryAt),
        within7,
        within30,
      }
    })
  }, [data])

  const sum7 = models.reduce((s, m) => s + m.within7, 0)
  const sum30 = models.reduce((s, m) => s + m.within30, 0)
  const totalRemain = models.reduce((s, m) => s + m.buckets.reduce((t, b) => t + b.remain, 0), 0)
  const expired = models.reduce(
    (s, m) =>
      s +
      m.buckets.reduce((t, b) => {
        const d = dayDiff(bucketDay(b))
        return d !== null && d < 0 ? t + b.remain : t
      }, 0),
    0,
  )
  const nearCount = models.filter((m) => m.days !== null && m.days >= 0 && m.days <= 7).length

  const chart = useMemo(() => {
    const byDay = new Map<string, number>()
    for (const m of models) {
      for (const b of m.buckets) {
        const day = bucketDay(b)
        const d = dayDiff(day)
        if (d === null || d < 0) continue
        const key = cstDate(day)
        byDay.set(key, (byDay.get(key) ?? 0) + b.remain)
      }
    }
    return [...byDay.entries()]
      .sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0))
      .slice(0, 21)
      .map(([day, value]) => {
        const d = dayDiff(day)
        return { label: day.slice(5), value, tone: toneOf(d) }
      })
  }, [models])

  const shown = filter === 'near' ? models.filter((m) => m.days !== null && m.days >= 0 && m.days <= 7) : models

  const doCsv = () => {
    if (!shown.length) {
      toast.warn('当前没有可导出的行')
      return
    }
    const head = ['uid', '昵称', '域', '剩余积分', '最早到期', '剩余天数', '该批剩余', '7天内到期', '批次（日期:积分）']
    const lines = [head.map(csvCell).join(',')]
    for (const m of shown) {
      const bucketsText = m.buckets.map((b) => cstDate(bucketDay(b)) + ':' + b.remain).join(' | ')
      lines.push(
        [
          m.uid,
          nameOf(m),
          m.realm,
          String(m.credits),
          m.expiryAt ? cstDate(m.expiryAt) : '',
          m.days === null ? '' : String(m.days),
          String(m.remain),
          String(m.within7),
          bucketsText,
        ]
          .map(csvCell)
          .join(','),
      )
    }
    const blob = new Blob(['\ufeff' + lines.join('\r\n')], { type: 'text/csv;charset=utf-8' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'workbuddy-expiry-' + (filter === 'near' ? 'near' : 'all') + '-' + stamp() + '.csv'
    document.body.appendChild(a)
    a.click()
    a.remove()
    setTimeout(() => URL.revokeObjectURL(url), 1000)
    toast.ok('已导出 ' + shown.length + ' 行')
  }

  const head = (
    <div className="pagehead">
      <div>
        <h2>积分到期</h2>
        <p className="lead">
          {data
            ? [
                data.balance_refresh_sec ? '每 ' + duration(data.balance_refresh_sec) + ' 后台刷新余额' : '',
                refreshText(data.balance_refreshed_at) || '余额尚未刷新',
                '日期按 UTC+8 自然日',
              ]
                .filter(Boolean)
                .join(' · ')
            : '按到期日聚合的积分批次与临期提醒'}
        </p>
      </div>
      <div className="acts">
        <button className="btn sm" disabled={!shown.length} onClick={doCsv}>
          导出 CSV
        </button>
        <button className="btn sm" disabled={q.loading} onClick={q.reload}>
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
            <Skeleton rows={6} />
          </Box>
        )}
      </div>
    )
  }

  const columns: Column<Model>[] = [
    {
      key: 'account',
      title: '账号',
      sortValue: (m) => nameOf(m).toLowerCase(),
      render: (m) => (
        <div className="entity">
          <span className="avatar-sq">{nameOf(m).slice(0, 1).toUpperCase()}</span>
          <div className="meta">
            <div className="n truncate">{nameOf(m)}</div>
            <div className="s tiny mono truncate">
              {m.uid.slice(0, 8)} · {m.realm}
            </div>
          </div>
        </div>
      ),
    },
    {
      key: 'credits',
      title: '剩余积分',
      align: 'right',
      mono: true,
      width: '112px',
      sortValue: (m) => m.credits,
      render: (m) => credits(m.credits),
    },
    {
      key: 'expiry',
      title: '最早到期',
      width: '170px',
      sortValue: (m) => msOf(m.expiryAt),
      render: (m) => {
        if (!m.expiryAt) return <span className="muted">—</span>
        return (
          <span>
            <Badge tone={toneOf(m.days)}>{cstDate(m.expiryAt)}</Badge>
            <span className="tiny muted"> {daysText(m.days)}</span>
          </span>
        )
      },
    },
    {
      key: 'remain',
      title: '该批剩余',
      align: 'right',
      mono: true,
      width: '108px',
      sortValue: (m) => m.remain,
      render: (m) => credits(m.remain),
    },
    {
      key: 'within7',
      title: '7 天内',
      align: 'right',
      mono: true,
      width: '100px',
      hideMobile: true,
      sortValue: (m) => m.within7,
      render: (m) => (m.within7 > 0 ? credits(m.within7) : <span className="muted">—</span>),
    },
    {
      key: 'buckets',
      title: '批次',
      align: 'right',
      width: '72px',
      hideMobile: true,
      sortValue: (m) => m.buckets.length,
      render: (m) => num(m.buckets.length),
    },
    {
      key: 'daily',
      title: '日均可用',
      align: 'right',
      mono: true,
      width: '104px',
      hideMobile: true,
      sortValue: (m) => (m.days !== null && m.days > 0 ? Math.ceil(m.remain / m.days) : 0),
      render: (m) =>
        m.days !== null && m.days > 0 ? credits(Math.ceil(m.remain / m.days)) : <span className="muted">—</span>,
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
        <Stat label="7 天内过期" value={credits(sum7)} tone={sum7 > 0 ? 'bad' : 'muted'} sub={nearCount + ' 个账号'} />
        <Stat label="30 天内过期" value={credits(sum30)} tone={sum30 > 0 ? 'warn' : 'muted'} sub="含 7 天内部分" />
        <Stat
          label="已过期"
          value={credits(expired)}
          tone={expired > 0 ? 'bad' : 'muted'}
          sub={expired > 0 ? '已失效，无法再用' : '后端已剔除过期批次'}
        />
        <Stat label="未来到期积分" value={credits(totalRemain)} tone="accent" sub="按批次合计" />
        <Stat label="涉及账号" value={num(models.length)} sub="有未来到期批次" />
      </StatsGrid>

      <Box title="到期分布" sub="按 UTC+8 自然日聚合，柱高是该日到期的积分；最多显示 21 个日期">
        <BarChart items={chart} height={220} format={(v: number) => credits(v)} emptyText="没有未来到期的积分批次" />
      </Box>

      <Box
        title="到期明细"
        sub={filter === 'near' ? '只看 7 天内到期的账号' : '按剩余积分排序，越靠前越该先消耗'}
        actions={
          <Segmented value={filter} options={FILTER_OPTIONS} onChange={setFilter} ariaLabel="到期筛选" />
        }
      >
        <DataTable<Model>
          columns={columns}
          rows={shown}
          rowKey={(m) => m.uid}
          loading={q.loading}
          empty={models.length === 0 ? '没有到期的积分批次' : '7 天内没有到期积分'}
          emptyHint={
            models.length === 0
              ? '后台刷新余额后才会出现批次；可以先点右上角「刷新」，或等下一次自动刷新。'
              : '切到「全部」查看更远的批次。'
          }
          initialSort={{ key: 'credits', dir: 'desc' }}
          caption={
            <span className="tiny muted">
              临期高亮：3 天内红色、7 天内黄色；CSV 导出的是当前筛选结果。
            </span>
          }
        />
      </Box>
    </div>
  )
}
