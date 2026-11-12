import { useState } from 'react'
import { api, errorText } from '../lib/api'
import { useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { credits as fmtCredits, num, shortTime } from '../lib/format'
import { Badge, Box, Empty, Segmented, Skeleton, Stat, StatsGrid } from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'
import { LineChart } from '../components/charts'

// 字段口径与 internal/balance/balance.go 的 Series/Point/Daily 一一对应。
type BalPoint = {
  t: number
  u: string
  c: number
  d?: string
  cons?: number
  gain?: number
  f?: number
}

type BalDaily = {
  day: string
  first: number
  last: number
  change: number
  min: number
  max: number
  n: number
  full: boolean
  consumed: number
  gained: number
}

type BalSeries = {
  uid: string
  name?: string
  points: BalPoint[] | null
  current: number
  min: number
  max: number
  change: number
  daily?: BalDaily[] | null
  realm?: string
}

type BalResp = {
  ok: boolean
  now: string
  days: number
  series: BalSeries[] | null
}

type RangeKey = 7 | 14 | 30

const RANGE_OPTIONS: { value: RangeKey; label: string }[] = [
  { value: 7, label: '7 天' },
  { value: 14, label: '14 天' },
  { value: 30, label: '30 天' },
]

type Row = {
  uid: string
  name: string
  realm: string
  current: number
  min: number
  max: number
  change: number
  consumed: number
  gained: number
  hasDaily: boolean
  pointCount: number
}

function pad2(n: number): string {
  return String(n).padStart(2, '0')
}

function realmOf(s: BalSeries): string {
  return s.realm || 'unknown'
}

function signed(v: number): string {
  return (v > 0 ? '+' : '') + fmtCredits(v)
}

function accountName(s: BalSeries): string {
  if (s.name) return s.name
  return s.uid ? s.uid.slice(0, 8) : '未知账号'
}

export default function BalHist() {
  const { tick } = useRefresh()
  const [days, setDays] = useState<RangeKey>(7)
  const [realmPick, setRealmPick] = useState('')

  const q = useQuery<BalResp>(
    (signal) => api.get<BalResp>('/balance_history', { days }, signal),
    [tick, days],
  )

  const d = q.data

  if (!d) {
    return (
      <div className="page">
        <div className="pagehead">
          <div>
            <h2>余额趋势</h2>
            <p className="lead">后台定时任务采样的积分余额快照</p>
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

  const all = d.series ?? []
  const realms: string[] = []
  for (const s of all) {
    const r = realmOf(s)
    if (realms.indexOf(r) < 0) realms.push(r)
  }
  const realm = realmPick && realms.indexOf(realmPick) >= 0 ? realmPick : realms[0] ?? ''

  // cn 与 global 积分不同源，必须按域分开看，不做跨域求和（panel.go:1749 注释口径）。
  const filtered = all.filter((s) => realm === '' || realmOf(s) === realm)

  const rows: Row[] = filtered.map((s) => {
    const daily = s.daily ?? []
    return {
      uid: s.uid,
      name: accountName(s),
      realm: realmOf(s),
      current: s.current,
      min: s.min,
      max: s.max,
      change: s.change,
      consumed: daily.reduce((a, x) => a + (x.consumed ?? 0), 0),
      gained: daily.reduce((a, x) => a + (x.gained ?? 0), 0),
      hasDaily: daily.length > 0,
      pointCount: (s.points ?? []).length,
    }
  })

  const totalCurrent = rows.reduce((a, r) => a + r.current, 0)
  const totalChange = rows.reduce((a, r) => a + r.change, 0)
  const totalPoints = rows.reduce((a, r) => a + r.pointCount, 0)

  // 时间轴：原始点数过多时按本地日期折叠，避免 30 天的曲线糊成一团。
  const tsSet: number[] = []
  const seenTs: Record<string, true> = {}
  for (const s of filtered) {
    for (const p of s.points ?? []) {
      const k = String(p.t)
      if (!seenTs[k]) {
        seenTs[k] = true
        tsSet.push(p.t)
      }
    }
  }
  tsSet.sort((a, b) => a - b)
  const byDay = tsSet.length > 60
  const keys: string[] = []
  const keyLabels: string[] = []
  for (const t of tsSet) {
    const key = byDay
      ? (() => {
          const dt = new Date(t * 1000)
          return dt.getFullYear() + '-' + pad2(dt.getMonth() + 1) + '-' + pad2(dt.getDate())
        })()
      : String(t)
    if (keys.indexOf(key) < 0) {
      keys.push(key)
      keyLabels.push(byDay ? shortTime(t * 1000).slice(0, 5) : shortTime(t * 1000))
    }
  }

  const topAccounts = rows.slice().sort((a, b) => b.current - a.current).slice(0, 8)
  const chartSeries = topAccounts.map((r) => {
    const src = filtered.find((s) => s.uid === r.uid)
    const map: Record<string, number> = {}
    for (const p of src?.points ?? []) {
      const key = byDay
        ? (() => {
            const dt = new Date(p.t * 1000)
            return dt.getFullYear() + '-' + pad2(dt.getMonth() + 1) + '-' + pad2(dt.getDate())
          })()
        : String(p.t)
      map[key] = p.c
    }
    return { label: r.name, points: keys.map((k) => (k in map ? map[k] : null)) }
  })

  // 余额绝对值大且平稳时，整条线会被压在图表顶部（LineChart 的 y 轴从 0 起）。
  // 这里把 y 轴零点平移到窗口最小值下方，formatY 再加回基准，刻度与悬浮读数仍是真实余额。
  let vMin = 0
  let vMax = 0
  let hasPoint = false
  for (const s of chartSeries) {
    for (const v of s.points) {
      if (v == null) continue
      if (!hasPoint) {
        vMin = v
        vMax = v
        hasPoint = true
      } else {
        if (v < vMin) vMin = v
        if (v > vMax) vMax = v
      }
    }
  }
  const yBase = hasPoint && vMin > 0 ? Math.max(0, vMin - Math.max(1, (vMax - vMin) * 0.25)) : 0
  const balanceSeries = chartSeries.map((s) => ({
    label: s.label,
    points: s.points.map((v) => (v == null ? null : v - yBase)),
  }))
  const balanceFormatY = (v: number) => fmtCredits(v + yBase)

  // 每日消耗 / 到账：两者都是非负量，正好喂给 LineChart（BarChart 会把负值抹平）。
  const dayMap: Record<string, { cons: number; gain: number }> = {}
  for (const s of filtered) {
    for (const x of s.daily ?? []) {
      const cur = dayMap[x.day] ?? { cons: 0, gain: 0 }
      cur.cons += x.consumed ?? 0
      cur.gain += x.gained ?? 0
      dayMap[x.day] = cur
    }
  }
  const dayKeys = Object.keys(dayMap).sort()
  const consSeries = [
    { label: '消耗', points: dayKeys.map((k) => dayMap[k].cons) },
    { label: '到账', points: dayKeys.map((k) => dayMap[k].gain) },
  ]

  const columns: Column<Row>[] = [
    {
      key: 'name',
      title: '账号',
      sortValue: (r) => r.name,
      render: (r) => (
        <div>
          <div className="truncate">{r.name}</div>
          <div className="tiny muted mono">{r.uid ? r.uid.slice(0, 8) : '-'}</div>
        </div>
      ),
    },
    { key: 'realm', title: '域', render: (r) => <span className="chip">{r.realm}</span>, width: '88px' },
    { key: 'current', title: '当前', align: 'right', sortValue: (r) => r.current, render: (r) => fmtCredits(r.current) },
    { key: 'min', title: '最低', align: 'right', sortValue: (r) => r.min, render: (r) => fmtCredits(r.min) },
    { key: 'max', title: '最高', align: 'right', sortValue: (r) => r.max, render: (r) => fmtCredits(r.max) },
    {
      key: 'change',
      title: '净变化',
      align: 'right',
      sortValue: (r) => r.change,
      render: (r) => <Badge tone={r.change > 0 ? 'ok' : r.change < 0 ? 'bad' : 'muted'}>{signed(r.change)}</Badge>,
    },
    {
      key: 'consumed',
      title: '消耗',
      align: 'right',
      sortValue: (r) => r.consumed,
      render: (r) => (r.hasDaily ? fmtCredits(r.consumed) : <span className="muted">—</span>),
    },
    {
      key: 'gained',
      title: '到账',
      align: 'right',
      sortValue: (r) => r.gained,
      render: (r) => (r.hasDaily ? fmtCredits(r.gained) : <span className="muted">—</span>),
    },
    { key: 'points', title: '采样点数', align: 'right', sortValue: (r) => r.pointCount, render: (r) => num(r.pointCount), hideMobile: true },
  ]

  const realmOptions = realms.map((r) => ({ value: r, label: r }))

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>余额趋势</h2>
          <p className="lead">
            积分余额快照，回看 {d.days} 天 · 数据截至 {shortTime(d.now)}
          </p>
        </div>
        <div className="acts">
          <button className="btn ghost" onClick={q.reload} disabled={q.loading}>
            {q.loading ? '刷新中…' : '刷新'}
          </button>
        </div>
      </div>

      <div className="toolbar">
        <Segmented value={days} options={RANGE_OPTIONS} onChange={setDays} ariaLabel="时间范围" />
        {realmOptions.length > 1 ? (
          <>
            <span className="sep" />
            <Segmented value={realm} options={realmOptions} onChange={setRealmPick} ariaLabel="积分域" />
          </>
        ) : null}
        <span className="muted small">cn 与 global 积分不同源，按域分开统计</span>
      </div>

      {q.error ? (
        <div className="notice bad">
          <span>{errorText(q.error)}</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      {all.length === 0 ? (
        <Box>
          <Empty
            title="还没有历史快照，后台定时任务跑一次后出现"
            hint="快照由后端定时任务采样，等待一轮后刷新本页"
            action={
              <button className="btn" onClick={q.reload}>
                重新读取
              </button>
            }
          />
        </Box>
      ) : (
        <>
          <StatsGrid>
            <Stat label="账号数" value={num(rows.length)} sub={'采样点 ' + num(totalPoints) + ' 个'} />
            <Stat label="当前合计" value={fmtCredits(totalCurrent)} sub={realm ? '域 ' + realm : ''} tone="accent" />
            <Stat
              label="净变化"
              value={signed(totalChange)}
              sub={days + ' 天内'}
              tone={totalChange > 0 ? 'ok' : totalChange < 0 ? 'bad' : 'muted'}
            />
            <Stat
              label="采样时间轴"
              value={num(keys.length)}
              sub={byDay ? '按天折叠显示' : '按原始采样点显示'}
            />
          </StatsGrid>

          <Box
            title="余额曲线"
            sub={rows.length > 8 ? '仅显示余额最高的 8 个账号，明细见下表' : '按账号一条曲线'}
          >
            <LineChart labels={keyLabels} series={balanceSeries} height={260} formatY={balanceFormatY} emptyText="该域暂无采样" />
          </Box>

          <Box title="每日消耗与到账" sub={realm ? '域 ' + realm : '按采样相邻下降/上升量累计'}>
            <LineChart labels={dayKeys.map((k) => k.slice(5))} series={consSeries} height={200} formatY={fmtCredits} emptyText="还没有按日聚合的数据" />
          </Box>

          <Box
            title="账号明细"
            sub={'共 ' + num(rows.length) + ' 个账号'}
            footer={
              <div className="row row-wrap gap2 small muted">
                <span>合计 {fmtCredits(totalCurrent)}</span>
                <span>净变化 {signed(totalChange)}</span>
                <span>最低 / 最高不求和（各账号区间不同）</span>
              </div>
            }
          >
            <DataTable
              columns={columns}
              rows={rows}
              rowKey={(r) => r.uid}
              loading={q.loading}
              empty="该域暂无账号快照"
              emptyHint="切到其他域，或等待下一次余额刷新"
              initialSort={{ key: 'current', dir: 'desc' }}
            />
          </Box>
        </>
      )}
    </div>
  )
}
