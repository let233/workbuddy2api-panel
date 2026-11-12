import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useLocalStorage, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { cstDate, credits as fmtCredits, num, shortTime } from '../lib/format'
import { Badge, Box, Empty, Segmented, Skeleton, Stat, StatsGrid } from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'
import { LineChart } from '../components/charts'

// 字段口径：internal/balance/balance.go:490-530（Series/Daily）与 internal/panel/panel.go:1745-1778。
type BhPoint = { t: number; c: number; u?: number; d?: number; cons?: number; gain?: number; f?: boolean }

type BhDaily = {
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

type BhSeries = {
  uid: string
  name?: string
  realm?: string
  points: BhPoint[] | null
  current: number
  min: number
  max: number
  change: number
  daily?: BhDaily[] | null
}

type BhResp = { ok: boolean; now: string; days: number; series: BhSeries[] | null }

type RosterItem = {
  uid: string
  nickname?: string
  realm?: string
  group?: string
  paused: boolean
  disabled: boolean
  enterprise: boolean
  api_key: boolean
}

type RosterResp = { accounts: RosterItem[] | null; total: number }

type RangeKey = 7 | 14 | 30

const RANGE_OPTIONS: { value: RangeKey; label: string }[] = [
  { value: 7, label: '7 天' },
  { value: 14, label: '14 天' },
  { value: 30, label: '30 天' },
]

/** 月度消耗/到账只在窗口覆盖整月时才有意义（旧面板同口径）。 */
type MonthFlow = { consumed: number; gained: number; from: string; covered: boolean }

type Row = {
  key: string
  name: string
  realm: string
  current: number
  min: number
  max: number
  change: number
  today: number | null
  yesterday: number | null
  points: number
  todayFlow: { consumed: number; gained: number } | null
  month: MonthFlow | null
  disabled: boolean
  paused: boolean
}

function prevDay(day: string): string {
  const ms = Date.parse(day + 'T00:00:00Z')
  if (!isFinite(ms)) return day
  return new Date(ms - 86400_000).toISOString().slice(0, 10)
}

function dayFlow(daily: BhDaily[] | null | undefined, day: string): { consumed: number; gained: number } | null {
  if (!daily || daily.length === 0) return null
  let seen = false
  let consumed = 0
  let gained = 0
  for (const it of daily) {
    if (it.day !== day) continue
    seen = true
    consumed += it.consumed || 0
    gained += it.gained || 0
  }
  return seen ? { consumed, gained } : null
}

function dayChange(daily: BhDaily[] | null | undefined, day: string): number | null {
  if (!daily) return null
  for (const it of daily) if (it.day === day) return it.change
  return null
}

function monthFlow(daily: BhDaily[] | null | undefined, month: string, days: number): MonthFlow | null {
  if (!daily || daily.length === 0) return null
  const monthStart = month + '-01'
  let consumed = 0
  let gained = 0
  let earliest = ''
  let seen = false
  for (const it of daily) {
    if (!it.day.startsWith(month)) continue
    seen = true
    consumed += it.consumed || 0
    gained += it.gained || 0
    if (!earliest || it.day < earliest) earliest = it.day
  }
  if (!seen) return null
  // 窗口不足 28 天时不可能覆盖整月，直接按受限处理。
  const covered = days >= 28 && earliest <= monthStart
  return { consumed, gained, from: earliest, covered }
}

function fmtSigned(v: number | null): string {
  if (v === null || !isFinite(v)) return '—'
  if (v === 0) return '0'
  return (v > 0 ? '+' : '−') + fmtCredits(Math.abs(v))
}

function signTone(v: number | null): 'ok' | 'bad' | 'muted' {
  if (v === null || v === 0) return 'muted'
  return v > 0 ? 'ok' : 'bad'
}

export default function BalHist() {
  const { tick } = useRefresh()
  const [range, setRange] = useState<RangeKey>(30)
  const [realm, setRealm] = useLocalStorage<string>('bhRealm', 'cn')

  const q = useQuery<BhResp>((signal) => api.get<BhResp>('/balance_history', { days: range }, signal), [tick, range])
  const roster = useQuery<RosterResp>((signal) => api.get<RosterResp>('/accounts/roster', {}, signal), [tick])

  const all = useMemo(() => q.data?.series ?? [], [q.data])
  const realms = useMemo(() => {
    const out: { value: string; label: string; count: number }[] = []
    for (const s of all) {
      const r = s.realm || 'unknown'
      const hit = out.find((x) => x.value === r)
      if (hit) hit.count += 1
      else out.push({ value: r, label: r, count: 1 })
    }
    return out
  }, [all])

  const activeRealm = realms.some((r) => r.value === realm) ? realm : realms[0]?.value ?? ''
  const series = all.filter((s) => (s.realm || 'unknown') === activeRealm)

  const todayDay = cstDate(Date.now())
  const month = todayDay.slice(0, 7)
  const yesterdayDay = prevDay(todayDay)

  const rows: Row[] = series.map((s) => ({
    key: s.uid,
    name: s.name || s.uid.slice(0, 8),
    realm: s.realm || 'unknown',
    current: s.current,
    min: s.min,
    max: s.max,
    change: s.change,
    today: dayChange(s.daily, todayDay),
    yesterday: dayChange(s.daily, yesterdayDay),
    points: (s.points ?? []).length,
    todayFlow: dayFlow(s.daily, todayDay),
    month: monthFlow(s.daily, month, range),
    disabled: false,
    paused: false,
  }))

  const rosterMap = new Map<string, RosterItem>()
  for (const it of roster.data?.accounts ?? []) rosterMap.set(it.uid, it)
  for (const r of rows) {
    const it = rosterMap.get(r.key)
    if (it) {
      r.disabled = it.disabled
      r.paused = it.paused
    }
  }

  const usable = rows.filter((r) => !r.disabled && !r.paused).length
  const sumCurrent = rows.reduce((a, r) => a + r.current, 0)
  const sumChange = rows.reduce((a, r) => a + r.change, 0)
  const sumToday = rows.reduce((a, r) => a + (r.today ?? 0), 0)
  const sumYesterday = rows.reduce((a, r) => a + (r.yesterday ?? 0), 0)
  const sumPoints = rows.reduce((a, r) => a + r.points, 0)
  const sumConsumedToday = rows.reduce((a, r) => a + (r.todayFlow?.consumed ?? 0), 0)
  const sumGainedToday = rows.reduce((a, r) => a + (r.todayFlow?.gained ?? 0), 0)
  const sumConsumedMonth = rows.reduce((a, r) => a + (r.month?.consumed ?? 0), 0)
  const sumGainedMonth = rows.reduce((a, r) => a + (r.month?.gained ?? 0), 0)

  // 余额曲线：LineChart 的 y 轴从 0 起，会把高位余额的波动压平，
  // 所以把零点平移到窗口最低点下方，再用 formatY 加回基准，并在图注里说明轴不从 0 起。
  const shown = rows.slice().sort((a, b) => b.current - a.current).slice(0, 8)
  const timeline: number[] = []
  for (const r of shown) {
    const s = series.find((x) => x.uid === r.key)
    for (const p of s?.points ?? []) if (!timeline.includes(p.t)) timeline.push(p.t)
  }
  timeline.sort((a, b) => a - b)
  const labels = timeline.map((t) => shortTime(t * 1000))

  let lo = Infinity
  let hi = -Infinity
  for (const r of shown) {
    const s = series.find((x) => x.uid === r.key)
    for (const p of s?.points ?? []) {
      if (p.c < lo) lo = p.c
      if (p.c > hi) hi = p.c
    }
  }
  const hasLine = shown.length > 0 && timeline.length > 0 && isFinite(lo) && isFinite(hi)
  const yBase = hasLine && hi > 0 && lo > hi * 0.15 ? Math.max(0, Math.floor(lo - (hi - lo) * 0.12)) : 0

  const balanceSeries = shown.map((r, i) => {
    const s = series.find((x) => x.uid === r.key)
    const byT = new Map<number, number>()
    for (const p of s?.points ?? []) byT.set(p.t, p.c)
    return {
      label: r.name,
      color: ['var(--accent)', 'var(--ok)', 'var(--warn)', 'var(--bad)', '#8b5cf6', '#14b8a6', '#f472b6', '#facc15'][i % 8],
      points: timeline.map((t) => {
        const v = byT.get(t)
        return typeof v === 'number' ? v - yBase : null
      }),
    }
  })

  // 每日消耗/到账：按日聚合成两条曲线（BarChart 对负值会抹平，所以不用柱）。
  const allDays: string[] = []
  for (const s of series) {
    for (const it of s.daily ?? []) if (!allDays.includes(it.day)) allDays.push(it.day)
  }
  allDays.sort()
  const dailyConsumed = allDays.map((day) => series.reduce((a, s) => a + (dayFlow(s.daily, day)?.consumed ?? 0), 0))
  const dailyGained = allDays.map((day) => series.reduce((a, s) => a + (dayFlow(s.daily, day)?.gained ?? 0), 0))

  const columns: Column<Row>[] = [
    {
      key: 'name',
      title: '账号',
      sortValue: (r) => r.name,
      render: (r) => (
        <div className="row gap1 v-center">
          <div className="truncate">
            <div className="truncate">{r.name}</div>
            <div className="tiny muted mono truncate">{r.key.slice(0, 8)}</div>
          </div>
          {r.disabled ? <Badge tone="muted">禁用</Badge> : null}
          {r.paused ? <Badge tone="warn">暂停</Badge> : null}
        </div>
      ),
    },
    {
      key: 'current',
      title: '当前',
      align: 'right',
      sortValue: (r) => r.current,
      render: (r) => <span className="num bold">{fmtCredits(r.current)}</span>,
    },
    {
      key: 'min',
      title: '最低',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => r.min,
      render: (r) => <span className="num muted">{fmtCredits(r.min)}</span>,
    },
    {
      key: 'max',
      title: '最高',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => r.max,
      render: (r) => <span className="num muted">{fmtCredits(r.max)}</span>,
    },
    {
      key: 'change',
      title: '净变化',
      align: 'right',
      sortValue: (r) => r.change,
      render: (r) => <Badge tone={signTone(r.change)}>{fmtSigned(r.change)}</Badge>,
    },
    {
      key: 'today',
      title: '今日 / 昨日',
      align: 'right',
      sortValue: (r) => r.today ?? 0,
      render: (r) => (
        <div>
          <div className="num small">{fmtSigned(r.today)}</div>
          <div className="tiny muted num">{fmtSigned(r.yesterday)}</div>
        </div>
      ),
    },
    {
      key: 'flow',
      title: '消耗 · 到账',
      align: 'right',
      hideMobile: true,
      sortValue: (r) => (r.todayFlow?.consumed ?? 0),
      render: (r) => (
        <div>
          <div className="tiny muted">今日</div>
          <div className="num small">
            {r.todayFlow ? fmtCredits(r.todayFlow.consumed) + ' / ' + fmtCredits(r.todayFlow.gained) : <span className="muted">—</span>}
          </div>
          <div className="tiny muted">本月</div>
          <div className="num small">
            {r.month === null ? (
              <span className="muted">—</span>
            ) : r.month.covered ? (
              fmtCredits(r.month.consumed) + ' / ' + fmtCredits(r.month.gained)
            ) : (
              <span
                className="dim"
                title={'窗口只覆盖到 ' + r.month.from + '，本月统计不完整；切到 30 天可看整月'}
              >
                {fmtCredits(r.month.consumed) + ' / ' + fmtCredits(r.month.gained)}
              </span>
            )}
          </div>
        </div>
      ),
    },
    {
      key: 'points',
      title: '采样',
      align: 'right',
      sortValue: (r) => r.points,
      render: (r) => <span className="num muted">{num(r.points)}</span>,
    },
  ]

  if (!q.data) {
    return (
      <div className="page">
        <div className="pagehead">
          <div>
            <h2>余额趋势</h2>
            <p className="lead">按域分段查看余额与消耗</p>
          </div>
          <div className="acts">
            <Segmented value={range} options={RANGE_OPTIONS} onChange={setRange} ariaLabel="时间范围" />
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

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>余额趋势</h2>
          <p className="lead">
            {q.data.now ? '更新于 ' + shortTime(q.data.now) : '余额采样'} · 近 {range} 天
          </p>
        </div>
        <div className="acts">
          <Segmented value={range} options={RANGE_OPTIONS} onChange={setRange} ariaLabel="时间范围" />
          <button className="btn ghost" onClick={q.reload} disabled={q.loading}>
            {q.loading ? '刷新中…' : '刷新'}
          </button>
        </div>
      </div>

      {q.error ? (
        <div className="notice bad">
          <span>读取余额趋势失败：{errorText(q.error)}</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      {realms.length > 0 ? (
        <div className="toolbar">
          <Segmented
            value={activeRealm}
            options={realms.map((r) => ({ value: r.value, label: r.label + ' · ' + num(r.count) }))}
            onChange={setRealm}
            ariaLabel="域"
          />
          <span className="tiny muted">cn 与 global 积分不同源，不跨域求和</span>
        </div>
      ) : null}

      {rows.length === 0 ? (
        <Box>
          <Empty
            title={all.length === 0 ? '还没有历史快照' : '该域暂无采样'}
            hint={
              all.length === 0
                ? '后台定时任务跑一次后出现；采集 30 分钟一轮'
                : '切到其他域，或等待下一次余额刷新'
            }
            action={
              <button className="btn" onClick={q.reload}>
                刷新
              </button>
            }
          />
        </Box>
      ) : (
        <>
          <StatsGrid>
            <Stat label="账号" value={num(rows.length)} sub={activeRealm ? '域 ' + activeRealm : '全部'} />
            <Stat
              label="可用"
              value={roster.data ? num(usable) + ' / ' + num(rows.length) : '—'}
              sub={roster.data ? undefined : '账号池读取失败'}
              tone={roster.data && usable < rows.length ? 'warn' : 'ok'}
            />
            <Stat label="当前合计" value={fmtCredits(sumCurrent)} sub="所选域内账号相加" />
            <Stat
              label="窗口净变化"
              value={fmtSigned(sumChange)}
              sub={'今日 ' + fmtSigned(sumToday) + ' · 昨日 ' + fmtSigned(sumYesterday)}
              tone={signTone(sumChange)}
            />
            <Stat label="采样点数" value={num(sumPoints)} sub={q.data.now ? '更新于 ' + shortTime(q.data.now) : undefined} />
          </StatsGrid>

          <Box
            title="余额曲线"
            sub={
              (shown.length < rows.length ? '仅画余额最高的 ' + shown.length + ' 个账号 · ' : '') +
              (yBase > 0 ? '轴不从 0 起（基准 ' + fmtCredits(yBase) + '）' : '轴从 0 起')
            }
          >
            <LineChart
              labels={labels}
              series={balanceSeries}
              height={240}
              formatY={(v) => fmtCredits(v + yBase)}
              emptyText="所选域暂无余额采样"
            />
          </Box>

          <Box title="每日消耗 / 到账" sub="按日聚合所选域全部账号">
            <LineChart
              labels={allDays.map((d) => d.slice(5))}
              series={[
                { label: '消耗', points: dailyConsumed, color: 'var(--bad)' },
                { label: '到账', points: dailyGained, color: 'var(--ok)' },
              ]}
              height={200}
              formatY={fmtCredits}
              emptyText="所选域暂无每日明细"
            />
            <div className="muted small" style={{ marginTop: 8 }}>
              消耗与到账是采样下界估计（30 分钟一轮，中途的抖动可能漏采）；不完整日不宜与完整日直接比较。
            </div>
          </Box>

          <Box
            title="账号明细"
            sub={num(rows.length) + ' 个账号 · 最低/最高为各账号自身窗口内的极值'}
            footer={
              <div className="stack gap1 small">
                <div className="row row-wrap gap2">
                  <span className="bold">合计（{activeRealm || '全部'}）</span>
                  <span className="muted">
                    可用 {roster.data ? num(usable) + '/' + num(rows.length) : '—'}
                  </span>
                  <span className="num">当前 {fmtCredits(sumCurrent)}</span>
                  <span className="muted" title="各账号低谷/高峰出现在不同时刻，不可相加">
                    最低·最高 —
                  </span>
                  <span>
                    净变化 <Badge tone={signTone(sumChange)}>{fmtSigned(sumChange)}</Badge>
                  </span>
                  <span className="muted">采样 {num(sumPoints)}</span>
                </div>
                <div className="row row-wrap gap2 tiny muted">
                  <span>今日 {fmtSigned(sumToday)} · 昨日 {fmtSigned(sumYesterday)}</span>
                  <span>
                    消耗 今日 {fmtCredits(sumConsumedToday)} / 本月 {fmtCredits(sumConsumedMonth)}
                  </span>
                  <span>
                    到账 今日 {fmtCredits(sumGainedToday)} / 本月 {fmtCredits(sumGainedMonth)}
                  </span>
                </div>
              </div>
            }
          >
            <DataTable
              columns={columns}
              rows={rows}
              rowKey={(r) => r.key}
              loading={q.loading}
              empty="该域暂无采样"
              emptyHint="切到其他域，或等待下一次余额刷新"
              initialSort={{ key: 'current', dir: 'desc' }}
            />
          </Box>
        </>
      )}
    </div>
  )
}
