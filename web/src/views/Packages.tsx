import { useState } from 'react'
import { api, errorText } from '../lib/api'
import { useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { credits as fmtCredits, num, shortTime } from '../lib/format'
import { Badge, Box, Empty, Pager, Segmented, Skeleton, Stat, StatsGrid, Switch, type Tone } from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'
import { Donut, PALETTE, StackBar } from '../components/charts'

// 字段口径：internal/panel/panel.go 的 packages handler + internal/upstream/client.go 的 CreditPackage。
type Pkg = {
  name: string
  remain: number
  used: number
  size: number
  end_time?: string
  expires_at?: number
  created_at?: string
  package_code?: string
  sub_product_code?: string
  sub_product_name?: string
  cycle?: boolean
}

type Acc = {
  uid: string
  nickname?: string
  realm?: string
  remain: number
  size: number
  packages: Pkg[] | null
  plan: string
  plan_evidence?: string
  error?: string
}

type Resp = {
  accounts: Acc[] | null
}

const PLAN_LABEL: Record<string, string> = {
  free: '免费版',
  trial: '体验版',
  activity: '活动包',
  standard: '订阅版',
  unknown: '未知',
}

const PLAN_TONE: Record<string, Tone> = {
  free: 'muted',
  trial: 'accent',
  activity: 'warn',
  standard: 'ok',
  unknown: 'muted',
}

type Source = {
  key: string
  label: string
  size: number
  remain: number
  used: number
  count: number
}

type AccRow = {
  uid: string
  name: string
  realm: string
  plan: string
  remain: number
  size: number
  used: number
  count: number
  expiry?: number
  sources: Source[]
}

type DayRow = {
  days: number
  parts: { label: string; value: number; color: string }[]
  total: number
}

type PkgRow = {
  key: string
  account: string
  realm: string
  label: string
  sub: string
  size: number
  remain: number
  used: number
  created?: string
  expiresAt?: number
}

function sourceKey(p: Pkg): string {
  return (p.package_code || '') + '|' + (p.name || '')
}

function sourceLabel(p: Pkg): string {
  return p.name || p.package_code || '未命名积分包'
}

function accountName(a: Acc): string {
  if (a.nickname) return a.nickname
  return a.uid ? a.uid.slice(0, 8) : '未知账号'
}

function realmOf(a: Acc): string {
  return a.realm || 'unknown'
}

/** 到期时间用「剩余 N 天」表达，绝对值放 title，窄屏也不挤。 */
function expiryText(ms?: number): string {
  if (!ms) return '无到期时间'
  const diff = ms - Date.now()
  if (diff <= 0) return '已到期'
  const mins = Math.ceil(diff / 60000)
  if (mins < 60) return '剩余 ' + mins + ' 分钟'
  const hours = Math.ceil(diff / 3600000)
  if (hours < 48) return '剩余 ' + hours + ' 小时'
  return '剩余 ' + Math.ceil(diff / 86400000) + ' 天'
}

function dateText(v?: string): string {
  if (!v) return '—'
  const ms = Date.parse(v)
  return isFinite(ms) ? shortTime(ms) : v
}

export default function Packages() {
  const { tick } = useRefresh()
  const [realmPick, setRealmPick] = useState('all')
  const [onlyActive, setOnlyActive] = useState(true)
  const [page, setPage] = useState(1)
  const pageSize = 20

  const q = useQuery<Resp>((signal) => api.get<Resp>('/packages', undefined, signal), [tick])

  const d = q.data

  if (!d) {
    return (
      <div className="page">
        <div className="pagehead">
          <div>
            <h2>积分构成</h2>
            <p className="lead">逐账号实时查询上游积分包</p>
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

  const accounts = d.accounts ?? []
  const realms: string[] = []
  for (const a of accounts) {
    const r = realmOf(a)
    if (realms.indexOf(r) < 0) realms.push(r)
  }
  const realm = realmPick === 'all' || realms.indexOf(realmPick) >= 0 ? realmPick : 'all'

  const filtered = accounts.filter((a) => realm === 'all' || realmOf(a) === realm)
  const okAccounts = filtered.filter((a) => !a.error)
  const failedAccounts = filtered.filter((a) => a.error)

  // 来源聚合：以 package_code|name 为键，面额/剩余/已用都口径内累加。
  const srcMap: Record<string, Source> = {}
  for (const a of okAccounts) {
    for (const p of a.packages ?? []) {
      const k = sourceKey(p)
      const cur = srcMap[k] ?? { key: k, label: sourceLabel(p), size: 0, remain: 0, used: 0, count: 0 }
      cur.size += p.size
      cur.remain += p.remain
      cur.used += p.used
      cur.count += 1
      srcMap[k] = cur
    }
  }
  const allSources = Object.keys(srcMap)
    .map((k) => srcMap[k])
    .sort((a, b) => b.size - a.size)

  const totalSize = okAccounts.reduce((s, a) => s + a.size, 0)
  const totalRemain = okAccounts.reduce((s, a) => s + a.remain, 0)
  const totalUsed = Math.max(0, totalSize - totalRemain)
  const totalPkgs = okAccounts.reduce((s, a) => s + (a.packages ?? []).length, 0)

  const donutParts = allSources.slice(0, 6).map((s) => ({ label: s.label, value: s.size }))
  const restSize = allSources.slice(6).reduce((s, x) => s + x.size, 0)
  if (restSize > 0) donutParts.push({ label: '其他 ' + (allSources.length - 6) + ' 种', value: restSize })

  const accRows: AccRow[] = okAccounts
    .map((a) => {
      const local: Record<string, Source> = {}
      for (const p of a.packages ?? []) {
        const k = sourceKey(p)
        const cur = local[k] ?? { key: k, label: sourceLabel(p), size: 0, remain: 0, used: 0, count: 0 }
        cur.size += p.size
        cur.remain += p.remain
        cur.used += p.used
        cur.count += 1
        local[k] = cur
      }
      const sources = Object.keys(local)
        .map((k) => local[k])
        .sort((x, y) => y.size - x.size)
      const withExpiry = (a.packages ?? []).filter((p) => p.expires_at && p.remain > 0 && (p.expires_at ?? 0) > Date.now())
      const expiry = withExpiry.length > 0 ? Math.min(...withExpiry.map((p) => p.expires_at ?? 0)) : undefined
      return {
        uid: a.uid,
        name: accountName(a),
        realm: realmOf(a),
        plan: a.plan,
        remain: a.remain,
        size: a.size,
        used: Math.max(0, a.size - a.remain),
        count: (a.packages ?? []).length,
        expiry,
        sources,
      }
    })
    .sort((x, y) => y.remain - x.remain)

  // 到期分布：账号内按「最早到期优先」把余额逐包裁剪，避免上游重复记账把金额算大；
  // 没有可解析到期时间的余额进不了图，单独计数（旧面板同口径）。
  const nameByUid: Record<string, string> = {}
  const colorByUid: Record<string, string> = {}
  for (let i = 0; i < accRows.length; i++) {
    nameByUid[accRows[i].uid] = accRows[i].name
    colorByUid[accRows[i].uid] = PALETTE[i % PALETTE.length]
  }
  const nowMs = Date.now()
  const bucketMap: Record<string, Record<string, number>> = {}
  let unavailable = 0
  for (const a of okAccounts) {
    const pkgs = (a.packages ?? []).slice().sort((x, y) => {
      const ax = x.expires_at ?? Number.MAX_SAFE_INTEGER
      const ay = y.expires_at ?? Number.MAX_SAFE_INTEGER
      return ax - ay
    })
    let budget = Math.max(0, a.remain)
    for (const pk of pkgs) {
      if (budget <= 0) break
      const take = Math.min(Math.max(0, pk.remain), budget)
      if (take <= 0) continue
      budget -= take
      if (!pk.expires_at) {
        unavailable += take
        continue
      }
      const days = Math.max(0, Math.ceil((pk.expires_at - nowMs) / 86400000))
      const key = String(days)
      const row = bucketMap[key] ?? {}
      row[a.uid] = (row[a.uid] ?? 0) + take
      bucketMap[key] = row
    }
    if (budget > 0) unavailable += budget
  }
  const dayRows: DayRow[] = Object.keys(bucketMap)
    .map((k) => {
      const per = bucketMap[k]
      const parts = Object.keys(per)
        .map((uid) => ({ label: nameByUid[uid] || uid.slice(0, 8), value: per[uid], color: colorByUid[uid] || PALETTE[0] }))
        .sort((x, y) => y.value - x.value)
      return { days: Number(k), parts, total: parts.reduce((s, x) => s + x.value, 0) }
    })
    .filter((r) => r.total > 0)
    .sort((x, y) => x.days - y.days)
  const dayLegend = accRows.filter((a) => dayRows.some((r) => r.parts.some((x) => x.label === a.name)))
  const dayRest = dayRows.slice(20)
  const dayShown = dayRows.slice(0, 20)

  // 明细分组：每账号最早到期的 5 个未用完包直接展示，其余折叠成一行合计（旧面板同口径）。
  let restActiveCount = 0
  let restActiveSize = 0
  let restActiveRemain = 0
  let doneCount = 0
  let doneSize = 0
  for (const a of okAccounts) {
    const act = (a.packages ?? [])
      .filter((pk) => pk.remain > 0)
      .sort((x, y) => {
        const ax = x.expires_at ?? Number.MAX_SAFE_INTEGER
        const ay = y.expires_at ?? Number.MAX_SAFE_INTEGER
        return ax - ay
      })
    for (const pk of act.slice(5)) {
      restActiveCount += 1
      restActiveSize += pk.size
      restActiveRemain += pk.remain
    }
    for (const pk of a.packages ?? []) {
      if (pk.remain > 0) continue
      doneCount += 1
      doneSize += pk.size
    }
  }

  const allPkgRows: PkgRow[] = []
  for (const a of okAccounts) {
    const pkgs = a.packages ?? []
    for (let i = 0; i < pkgs.length; i++) {
      const p = pkgs[i]
      if (onlyActive && p.remain <= 0) continue
      allPkgRows.push({
        key: a.uid + '|' + String(i),
        account: accountName(a),
        realm: realmOf(a),
        label: sourceLabel(p) + (p.cycle ? '（周期包）' : ''),
        sub: p.sub_product_name || p.sub_product_code || p.package_code || '',
        size: p.size,
        remain: p.remain,
        used: p.used,
        created: p.created_at,
        expiresAt: p.expires_at,
      })
    }
  }
  allPkgRows.sort((x, y) => {
    const ax = x.expiresAt ?? Number.MAX_SAFE_INTEGER
    const ay = y.expiresAt ?? Number.MAX_SAFE_INTEGER
    if (ax !== ay) return ax - ay
    return y.size - x.size
  })

  const pages = Math.max(1, Math.ceil(allPkgRows.length / pageSize))
  const cur = Math.min(page, pages)
  const pagedPkgs = allPkgRows.slice((cur - 1) * pageSize, cur * pageSize)

  const maxRemain = accRows.reduce((m, r) => Math.max(m, r.remain), 0)

  const accColumns: Column<AccRow>[] = [
    {
      key: 'name',
      title: '账号',
      sortValue: (r) => r.name,
      render: (r) => (
        <div>
          <div className="truncate">{r.name}</div>
          <div className="tiny muted">
            {r.realm} · {r.count} 个包
          </div>
        </div>
      ),
    },
    {
      key: 'plan',
      title: '套餐',
      width: '96px',
      sortValue: (r) => r.plan,
      render: (r) => <Badge tone={PLAN_TONE[r.plan] ?? 'muted'}>{PLAN_LABEL[r.plan] ?? r.plan}</Badge>,
    },
    { key: 'remain', title: '余额', align: 'right', sortValue: (r) => r.remain, render: (r) => fmtCredits(r.remain) },
    { key: 'size', title: '总额', align: 'right', sortValue: (r) => r.size, render: (r) => fmtCredits(r.size) },
    { key: 'used', title: '已用', align: 'right', sortValue: (r) => r.used, render: (r) => fmtCredits(r.used) },
    {
      key: 'mix',
      title: '构成',
      width: '160px',
      render: (r) =>
        r.size > 0 ? (
          <StackBar
            parts={r.sources
              .slice(0, 5)
              .map((s, i) => ({ label: s.label, value: s.size, color: PALETTE[i % PALETTE.length] }))}
            height={8}
            showLegend={false}
          />
        ) : (
          <span className="muted">—</span>
        ),
      hideMobile: true,
    },
    {
      key: 'share',
      title: '占最高',
      align: 'right',
      width: '88px',
      sortValue: (r) => (maxRemain > 0 ? r.remain / maxRemain : 0),
      render: (r) => {
        if (maxRemain <= 0) return <span className="muted">—</span>
        const share = Math.round((r.remain / maxRemain) * 100)
        return <span className="num small">{share}%</span>
      },
      hideMobile: true,
    },
    {
      key: 'sources',
      title: '来源',
      hideMobile: true,
      render: (r) =>
        r.sources.length === 0 ? (
          <span className="muted">—</span>
        ) : (
          <div>
            {r.sources.slice(0, 3).map((s) => (
              <div className="tiny muted truncate" key={s.key}>
                {s.label + ' x' + s.count + ' · ' + fmtCredits(s.size)}
              </div>
            ))}
            {r.sources.length > 3 ? <div className="tiny dim">＋{r.sources.length - 3} 种</div> : null}
          </div>
        ),
    },
    {
      key: 'expiry',
      title: '最近到期',
      align: 'right',
      sortValue: (r) => r.expiry ?? Number.MAX_SAFE_INTEGER,
      render: (r) =>
        r.expiry ? (
          <span title={shortTime(r.expiry)}>{expiryText(r.expiry)}</span>
        ) : (
          <span className="muted">—</span>
        ),
    },
  ]

  const pkgColumns: Column<PkgRow>[] = [
    {
      key: 'account',
      title: '账号',
      sortValue: (r) => r.account,
      render: (r) => (
        <div>
          <div className="truncate">{r.account}</div>
          <div className="tiny muted">{r.realm}</div>
        </div>
      ),
    },
    {
      key: 'label',
      title: '积分包',
      render: (r) => (
        <div>
          <div className="truncate">{r.label}</div>
          {r.sub ? <div className="tiny muted truncate">{r.sub}</div> : null}
        </div>
      ),
    },
    { key: 'size', title: '面额', align: 'right', sortValue: (r) => r.size, render: (r) => fmtCredits(r.size) },
    { key: 'remain', title: '剩余', align: 'right', sortValue: (r) => r.remain, render: (r) => fmtCredits(r.remain) },
    { key: 'used', title: '已用', align: 'right', sortValue: (r) => r.used, render: (r) => fmtCredits(r.used) },
    { key: 'created', title: '发放', align: 'right', sortValue: (r) => r.created ?? '', render: (r) => dateText(r.created), hideMobile: true },
    {
      key: 'expiry',
      title: '到期',
      align: 'right',
      sortValue: (r) => r.expiresAt ?? Number.MAX_SAFE_INTEGER,
      render: (r) => (r.expiresAt ? <span title={shortTime(r.expiresAt)}>{expiryText(r.expiresAt)}</span> : <span className="muted">—</span>),
    },
  ]

  const dayColumns: Column<DayRow>[] = [
    {
      key: 'days',
      title: '剩余天数',
      width: '96px',
      sortValue: (r) => r.days,
      render: (r) =>
        r.days === 0 ? <Badge tone="bad">已到期</Badge> : <span className="num small">{r.days} 天</span>,
    },
    {
      key: 'parts',
      title: '各账号该批剩余',
      render: (r) => <StackBar parts={r.parts} height={10} showLegend={false} />,
    },
    {
      key: 'total',
      title: '剩余积分',
      align: 'right',
      width: '120px',
      sortValue: (r) => r.total,
      render: (r) => <span className="num bold">{fmtCredits(r.total)}</span>,
    },
  ]

  const dayRestSum = dayRest.reduce((s, r) => s + r.total, 0)

  const realmOptions = [{ value: 'all', label: '全部域' }].concat(realms.map((r) => ({ value: r, label: r })))

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>积分构成</h2>
          <p className="lead">
            逐账号实时查询上游积分包 · {num(okAccounts.length)} 个账号 · {num(totalPkgs)} 个包
          </p>
        </div>
        <div className="acts">
          <button className="btn" onClick={q.reload} disabled={q.loading}>
            {q.loading ? '查询中…' : '重新查询'}
          </button>
        </div>
      </div>

      {realms.length > 1 ? (
        <div className="toolbar">
          <Segmented value={realm} options={realmOptions} onChange={setRealmPick} ariaLabel="积分域" />
          {realm === 'all' ? <span className="muted small">cn 与 global 积分不同源，合计只表示总体规模</span> : null}
        </div>
      ) : null}

      {q.error ? (
        <div className="notice bad">
          <span>{errorText(q.error)}</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      {failedAccounts.length > 0 ? (
        <div className="notice warn">
          {num(failedAccounts.length)} 个账号查询失败：
          {failedAccounts
            .slice(0, 5)
            .map((a) => ' ' + accountName(a) + '（' + (a.error ?? '未知错误') + '）')
            .join('；')}
          {failedAccounts.length > 5 ? ' 等' : ''}
        </div>
      ) : null}

      {accounts.length === 0 ? (
        <Box>
          <Empty title="还没有账号的积分信息" hint="先在账号页添加并授权账号，这里才有积分包可看" />
        </Box>
      ) : (
        <>
          <StatsGrid>
            <Stat label="总额度" value={fmtCredits(totalSize)} sub={num(totalPkgs) + ' 个积分包'} />
            <Stat label="已用" value={fmtCredits(totalUsed)} sub={totalSize > 0 ? Math.round((totalUsed / totalSize) * 100) + '% 已消耗' : ''} tone="warn" />
            <Stat label="剩余" value={fmtCredits(totalRemain)} sub={num(okAccounts.length) + ' 个账号'} tone="accent" />
            <Stat label="查询失败" value={num(failedAccounts.length)} sub={failedAccounts.length > 0 ? '见上方提示' : '全部成功'} tone={failedAccounts.length > 0 ? 'bad' : 'ok'} />
          </StatsGrid>

          <div className="grid2">
            <Box title="来源构成" sub="按积分包来源汇总面额">
              <Donut
                parts={donutParts}
                size={180}
                center={<div className="big">{fmtCredits(totalSize)}</div>}
                emptyText="没有可用积分包"
              />
            </Box>
            <Box title="已用 / 剩余" sub="合计口径，跨域仅供参考">
              <StackBar
                parts={[
                  { label: '已用', value: totalUsed, color: 'var(--warn)' },
                  { label: '剩余', value: totalRemain, color: 'var(--ok)' },
                ]}
                height={14}
                showLegend
              />
              <div className="stack">
                {allSources.slice(0, 8).map((s) => (
                  <div className="row" key={s.key}>
                    <span className="grow truncate small">{s.label}</span>
                    <span className="tiny muted">{s.count} 个</span>
                    <span className="mono small">{fmtCredits(s.size)}</span>
                  </div>
                ))}
              </div>
            </Box>
          </div>

          <Box
            title="到期分布"
            sub="先把账号余额按最早到期逐包裁剪，再按剩余天数分桶；无到期时间的余额不计入"
            footer={
              <div className="row row-wrap gap2 tiny muted">
                <span>{num(dayLegend.length)} 个账号可汇总</span>
                <span>{num(unavailable)} 积分无到期时间，未计入</span>
                {dayRest.length > 0 ? (
                  <span>
                    另有 {num(dayRest.length)} 档较晚到期未展示（合计 {fmtCredits(dayRestSum)}）
                  </span>
                ) : null}
              </div>
            }
          >
            <DataTable
              columns={dayColumns}
              rows={dayShown}
              rowKey={(r) => String(r.days)}
              loading={q.loading}
              empty="暂无可汇总积分"
              emptyHint="等账号拿到带到期时间的积分包后再看"
              initialSort={{ key: 'days', dir: 'asc' }}
            />
            {dayLegend.length > 0 ? (
              <div className="chip-wrap" style={{ marginTop: 10 }}>
                {dayLegend.map((a) => (
                  <span className="chip" key={a.uid}>
                    <i
                      aria-hidden="true"
                      style={{
                        display: 'inline-block',
                        width: 8,
                        height: 8,
                        marginRight: 5,
                        borderRadius: 2,
                        background: colorByUid[a.uid],
                      }}
                    />
                    {a.name}
                  </span>
                ))}
              </div>
            ) : null}
          </Box>

          <Box title="逐账号" sub={'按余额降序 · 占最高以本域余额最高的账号为 100% · 来源按积分包面额汇总'}>
            <DataTable
              columns={accColumns}
              rows={accRows}
              rowKey={(r) => r.uid}
              loading={q.loading}
              empty="该域没有可用的积分信息"
              emptyHint="切到其他域，或稍后重试"
              initialSort={{ key: 'remain', dir: 'desc' }}
            />
          </Box>

          <Box
            title="积分包明细"
            sub={'共 ' + num(allPkgRows.length) + ' 个包 · 按到期时间升序'}
            actions={<Switch checked={onlyActive} onChange={setOnlyActive} label="只看未用完" />}
            footer={
              <div className="stack gap1">
                <div className="row row-wrap gap2 tiny muted">
                  <span>
                    其余未用完 {num(restActiveCount)} 个包（面额合计 {fmtCredits(restActiveSize)} · 剩余{' '}
                    {fmtCredits(restActiveRemain)}）
                  </span>
                  <span>
                    已用完 {num(doneCount)} 个包（面额合计 {fmtCredits(doneSize)}）
                  </span>
                </div>
                <Pager page={cur} pageSize={pageSize} total={allPkgRows.length} onPage={setPage} />
              </div>
            }
          >
            <DataTable
              columns={pkgColumns}
              rows={pagedPkgs}
              rowKey={(r) => r.key}
              loading={q.loading}
              empty={onlyActive ? '没有未用完的积分包' : '没有积分包'}
              emptyHint={onlyActive ? '关掉「只看未用完」看历史包' : '等上游发放新包后再看'}
              initialSort={{ key: 'expiry', dir: 'asc' }}
            />
          </Box>
        </>
      )}
    </div>
  )
}
