import { useCallback, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useQuery, useBusy } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { ago, credits, duration, num, pct } from '../lib/format'
import { Badge, Bar, Box, KV, Modal, Skeleton, Stat, StatsGrid, type Tone } from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'
import { Donut } from '../components/charts'

type UpdateInfo = {
  available?: boolean
  latest?: string
  current?: string
  error?: string
  checked_at?: string
}

type ProxyView = {
  id: string
  name: string
  status: string
  enabled: boolean
  latency_ms: number
  bound_count: number
}

type ModelLock = {
  model: string
  realm: string
  total: number
  servable: number
  locked: number
  state: string
  unlock_at: string
  fully_unlock_at: string
  reason?: string
}

type ReloadSnapshot = {
  interval_sec: number
  delete_confirm: boolean
  at?: string
  added?: number
  updated?: number
  removed?: number
  parse_errors?: number
  skipped?: boolean
}

type OverviewResp = {
  version: string
  poll_interval_sec: number
  update: UpdateInfo | null
  uptime_sec: number
  login_required: boolean
  redis_mode: string
  sticky_sessions: number
  total: number
  healthy: number
  cooling: number
  disabled: number
  paused: number
  in_flight_full: number
  model_fallback: Record<string, string> | null
  credits_sum: number
  credits_total_sum: number
  proxy_pool: { proxies?: ProxyView[] } | null
  model_locks: ModelLock[] | null
  balance_refresh_sec: number
  balance_refreshed_at: string
  reload: ReloadSnapshot
}

const STATE_LABEL: Record<string, { text: string; tone: Tone }> = {
  locked: { text: '全锁', tone: 'bad' },
  starved: { text: '无可用号', tone: 'warn' },
  partial: { text: '部分锁', tone: 'accent' },
}

export default function Overview() {
  const { tick } = useRefresh()
  const toast = useToast()
  const busy = useBusy()
  const [helpOpen, setHelpOpen] = useState(false)

  const q = useQuery<OverviewResp>((signal) => api.get('/overview', undefined, signal), [tick], { poll: 5000 })
  const d = q.data

  const reloadNow = useCallback(() => {
    void busy.run('reload', async () => {
      try {
        const res = await api.post<{ result?: ReloadSnapshot }>('/accounts/reload')
        const r = res.result
        toast.ok(
          r
            ? '已重载：新增 ' + num(r.added) + ' · 更新 ' + num(r.updated) + ' · 移除 ' + num(r.removed)
            : '已重载账号目录',
        )
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }, [busy, toast, q])

  if (!d) {
    return (
      <div className="page">
        <div className="pagehead">
          <div>
            <h2>概览</h2>
            <p className="lead">账号池运行状态与需要处理的问题</p>
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

  const total = Math.max(1, d.total)
  const proxies = d.proxy_pool?.proxies ?? []
  const healthyProxy = proxies.filter((p) => p.enabled && p.status === 'healthy').length
  const locks = d.model_locks ?? []
  const fallback = Object.entries(d.model_fallback ?? {})
  const creditPct = d.credits_total_sum > 0 ? (d.credits_sum / d.credits_total_sum) * 100 : 0
  const lockedCount = locks.filter((l) => l.state === 'locked').length

  const lockCols: Column<ModelLock>[] = [
    {
      key: 'model',
      title: '模型',
      sortValue: (r) => r.model,
      render: (r) => <span className="mono">{r.model}</span>,
    },
    {
      key: 'realm',
      title: '域',
      width: '72px',
      sortValue: (r) => r.realm,
      render: (r) => <Badge tone="muted">{r.realm || 'cn'}</Badge>,
    },
    {
      key: 'servable',
      title: '可用/总数',
      align: 'right',
      sortValue: (r) => r.servable,
      render: (r) => (
        <span className="num">
          {num(r.servable)}
          <span className="dim"> / {num(r.total)}</span>
        </span>
      ),
    },
    {
      key: 'locked',
      title: '被锁',
      align: 'right',
      sortValue: (r) => r.locked,
      render: (r) => num(r.locked),
    },
    {
      key: 'state',
      title: '状态',
      sortValue: (r) => r.state,
      render: (r) => {
        const s = STATE_LABEL[r.state] ?? { text: r.state, tone: 'muted' as Tone }
        return <Badge tone={s.tone}>{s.text}</Badge>
      },
    },
    {
      key: 'unlock',
      title: '最早解锁',
      sortValue: (r) => r.unlock_at,
      render: (r) => <span className="nowrap">{r.unlock_at ? ago(r.unlock_at) : '-'}</span>,
    },
    {
      key: 'reason',
      title: '锁因',
      hideMobile: true,
      render: (r) => (
        <span className="muted small truncate" title={r.reason || ''}>
          {r.reason || '-'}
        </span>
      ),
    },
  ]

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>概览</h2>
          <p className="lead">账号池运行状态与需要处理的问题</p>
        </div>
        <div className="acts">
          <button className="btn sm" onClick={reloadNow} disabled={busy.busy !== ''}>
            {busy.busy ? '重载中…' : '重载账号目录'}
          </button>
          <button className="btn sm ghost" onClick={() => setHelpOpen(true)}>
            指标口径
          </button>
        </div>
      </div>

      {d.update?.available && (
        <div className="notice warn">
          <span>
            发现新版本 <b>{d.update.latest}</b>（当前 {d.update.current || d.version}），可到「关于」页安装。
          </span>
        </div>
      )}
      {q.error ? (
        <div className="notice bad">
          <span>刷新失败：{errorText(q.error)}（显示的是上一次的数据）</span>
        </div>
      ) : null}

      <StatsGrid>
        <Stat
          label="账号总数"
          value={num(d.total)}
          sub={
            <>
              {num(d.healthy)} 健康
              {d.in_flight_full > 0 && <span className="warn-text"> · {num(d.in_flight_full)} 在途满</span>}
            </>
          }
        />
        <Stat label="健康" value={num(d.healthy)} tone="ok" sub={pct((d.healthy / total) * 100)} />
        <Stat label="冷却中" value={num(d.cooling)} tone={d.cooling > 0 ? 'warn' : undefined} sub="限流/失败的号" />
        <Stat
          label="禁用 / 暂停"
          value={
            <>
              {num(d.disabled)}
              <span className="dim"> / {num(d.paused)}</span>
            </>
          }
          tone={d.disabled + d.paused > 0 ? 'bad' : undefined}
          sub="不计入选号"
        />
        <Stat
          label="积分合计"
          value={credits(d.credits_sum)}
          tone="accent"
          sub={
            <>
              共 {credits(d.credits_total_sum)}
              <Bar pct={creditPct} tone={creditPct < 15 ? 'bad' : creditPct < 35 ? 'warn' : 'ok'} />
            </>
          }
        />
        {d.proxy_pool && (
          <Stat
            label="代理可用"
            value={
              <>
                {num(healthyProxy)}
                <span className="dim"> / {num(proxies.length)}</span>
              </>
            }
            tone={healthyProxy < proxies.length ? 'warn' : 'ok'}
            sub={proxies.length === 0 ? '未配置代理' : '健康节点数'}
          />
        )}
        <Stat
          label="模型锁"
          value={num(locks.length)}
          tone={lockedCount > 0 ? 'bad' : locks.length > 0 ? 'warn' : 'ok'}
          sub={locks.length ? '有模型当前不可用' : '全部模型可用'}
        />
      </StatsGrid>

      <div className="grid2">
        <Box title="账号状态构成" sub={'共 ' + num(d.total) + ' 个账号'}>
          <div className="donut-wrap">
            <Donut
              parts={[
                { label: '健康', value: d.healthy, color: 'var(--ok)' },
                { label: '冷却', value: d.cooling, color: 'var(--warn)' },
                { label: '暂停', value: d.paused, color: 'var(--accent)' },
                { label: '禁用', value: d.disabled, color: 'var(--bad)' },
              ]}
              center={
                <div className="donut-center">
                  <div className="v">{num(d.healthy)}</div>
                  <div className="k">可服务</div>
                </div>
              }
            />
          </div>
        </Box>

        <Box title="运行状态">
          <KV
            items={[
              { k: '版本', v: d.version || 'dev', mono: true },
              { k: '已运行', v: duration(d.uptime_sec) },
              { k: '状态轮询', v: d.poll_interval_sec > 0 ? d.poll_interval_sec + ' 秒' : '仅手动' },
              { k: '余额刷新', v: d.balance_refresh_sec > 0 ? '每 ' + duration(d.balance_refresh_sec) : '未开启' },
              { k: '上次余额刷新', v: d.balance_refreshed_at ? ago(d.balance_refreshed_at) : '尚未刷新' },
              { k: '存储', v: d.redis_mode || '本地 SQLite' },
              { k: '粘性会话', v: num(d.sticky_sessions) },
              { k: '登录保护', v: d.login_required ? '已开启' : '已关闭（无需登录）' },
              { k: '目录热重载', v: d.reload.interval_sec > 0 ? '每 ' + duration(d.reload.interval_sec) : '仅手动' },
              { k: '上次重载', v: d.reload.at ? ago(d.reload.at) : '尚未重载' },
            ]}
          />
        </Box>
      </div>

      <Box title="模型锁池" sub="这些模型此刻全池不可用，发请求会直接失败——等解锁或换号" flush>
        <DataTable
          columns={lockCols}
          rows={locks}
          rowKey={(r) => r.realm + '/' + r.model}
          empty="没有模型被锁"
          emptyHint="所有模型的可用号都还在"
          initialSort={{ key: 'locked', dir: 'desc' }}
        />
      </Box>

      {fallback.length > 0 && (
        <Box title="模型降级路由" sub="请求这些模型时会自动改用等价模型">
          <div className="chip-wrap">
            {fallback.map(([from, to]) => (
              <span className="chip" key={from}>
                <span className="mono">{from}</span>
                <span className="dim"> → </span>
                <span className="mono">{to}</span>
              </span>
            ))}
          </div>
        </Box>
      )}

      <Modal open={helpOpen} title="指标口径" onClose={() => setHelpOpen(false)} narrow>
        <div className="stack">
          <p className="small muted">健康：可立即参与选号的账号（未禁用、未暂停、不在冷却、未被在途占满）。</p>
          <p className="small muted">冷却：因上游限流或请求失败进入冷却的账号，冷却结束后自动恢复。</p>
          <p className="small muted">积分合计：所有账号剩余积分之和；「共」是上游给出的额度上限之和，用来看整体余量。</p>
          <p className="small muted">模型锁：某模型在当前域下所有可用号都被限流，请求会直接失败，这是最该处理的告警。</p>
          <p className="small muted">数据每 5 秒刷新；余额与到期信息只随后台刷新任务更新。</p>
        </div>
      </Modal>
    </div>
  )
}
