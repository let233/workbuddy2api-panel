// Models.tsx 模型与档位：目录 + 思考档位映射 + 下游可见开关 + 模型锁池 / 回落映射。
// 两个可写动作都走后端既有接口：POST /model_hidden（进/出 config models.hidden）、
// POST /model_effort（写 config thinking.model_defaults），均热生效。
import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { duration, num } from '../lib/format'
import { useBusy, useDebounced, useNow, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { DataTable, type Column } from '../components/DataTable'
import {
  Badge,
  Box,
  Empty,
  KV,
  Segmented,
  Skeleton,
  Stat,
  StatsGrid,
  Switch,
  type Tone,
} from '../components/ui'

type ModelEntry = {
  id: string
  name: string
  vendor?: string
  tier?: string
  credits?: string
  description?: string
  tags?: string[] | null
  is_default?: boolean
  supports_reasoning?: boolean
  supports_images?: boolean
  supports_tool_call?: boolean
  only_reasoning?: boolean
  can_disable_thinking?: boolean
  context_length?: number
  max_output_tokens?: number
  default_effort?: string
  supported_efforts?: string[] | null
  /** 运行期标记：每次响应现算 */
  hidden: boolean
  gateway_policy_enabled: boolean
  gateway_effort?: string
  gateway_effort_wanted?: string
  busy?: string
}

type ModelsResp = {
  ok: boolean
  models: ModelEntry[] | null
  fetch_errors?: string[] | null
  busy_errs?: string[] | null
  global_source?: string
  cached?: boolean
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

type OverviewResp = {
  model_locks: ModelLock[] | null
  model_fallback: Record<string, string> | null
}

type RealmFilter = 'all' | 'cn' | 'global'
type VisFilter = 'all' | 'shown' | 'hidden'

const LOCK_STATE: Record<string, { text: string; tone: Tone }> = {
  locked: { text: '全部锁定', tone: 'bad' },
  starved: { text: '无可用号', tone: 'warn' },
  partial: { text: '部分锁定', tone: 'warn' },
}

const BUSY: Record<string, { text: string; tone: Tone }> = {
  idle: { text: '空闲', tone: 'ok' },
  normal: { text: '正常', tone: 'ok' },
  busy: { text: '繁忙', tone: 'warn' },
  congested: { text: '拥塞', tone: 'bad' },
  unavailable: { text: '不可用', tone: 'bad' },
  unknown: { text: '未知', tone: 'muted' },
}

const EFFORT_LABEL: Record<string, string> = {
  max: 'max',
  xhigh: 'xhigh',
  high: 'high',
  medium: 'medium',
  low: 'low',
  minimal: 'minimal',
  off: '关闭思考',
}

const FALLBACK_EFFORTS = ['max', 'xhigh', 'high', 'medium', 'low', 'minimal']

function splitId(id: string): { realm: string; bare: string } {
  const i = id.indexOf(':')
  if (i < 0) return { realm: '', bare: id }
  return { realm: id.slice(0, i), bare: id.slice(i + 1) }
}

function remainText(iso: string | undefined, nowMs: number): string {
  if (!iso) return '-'
  const at = Date.parse(iso)
  if (!isFinite(at) || at <= 0) return '-'
  const sec = Math.round((at - nowMs) / 1000)
  if (sec <= 0) return '即将解锁'
  return duration(sec)
}

export default function Models() {
  const { tick } = useRefresh()
  const toast = useToast()
  const { busy, run } = useBusy()
  const nowMs = useNow(5000)

  const q = useQuery<ModelsResp>((signal) => api.get<ModelsResp>('/models', undefined, signal), [tick])
  const ov = useQuery<OverviewResp>(
    (signal) => api.get<OverviewResp>('/overview', undefined, signal),
    [tick, 'models'],
    { poll: 10000 },
  )

  const [search, setSearch] = useState('')
  const debounced = useDebounced(search, 200)
  const [realm, setRealm] = useState<RealmFilter>('all')
  const [vis, setVis] = useState<VisFilter>('all')

  const models = useMemo(() => q.data?.models ?? [], [q.data])
  const locks = useMemo(() => ov.data?.model_locks ?? [], [ov.data])
  const fallback = useMemo(() => ov.data?.model_fallback ?? {}, [ov.data])

  const lockMap = useMemo(() => {
    const m = new Map<string, ModelLock>()
    for (const l of locks) m.set(l.realm + ':' + l.model, l)
    return m
  }, [locks])

  const rows = useMemo(() => {
    const needle = debounced.trim().toLowerCase()
    return models.filter((m) => {
      const { realm: r } = splitId(m.id)
      if (realm !== 'all' && r !== realm) return false
      if (vis === 'shown' && m.hidden) return false
      if (vis === 'hidden' && !m.hidden) return false
      if (!needle) return true
      return m.id.toLowerCase().includes(needle) || (m.name || '').toLowerCase().includes(needle)
    })
  }, [models, debounced, realm, vis])

  const stats = useMemo(() => {
    let hidden = 0
    for (const m of models) if (m.hidden) hidden++
    return {
      total: models.length,
      shown: models.length - hidden,
      hidden,
      lockedModels: locks.length,
    }
  }, [models, locks])

  const setHidden = (m: ModelEntry, hidden: boolean) => {
    void run('hidden:' + m.id, async () => {
      try {
        await api.post('/model_hidden', { id: m.id, hidden })
        toast.ok(hidden ? '已对下游隐藏 ' + m.name : '已对下游开放 ' + m.name)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const setEffort = (m: ModelEntry, effort: string) => {
    void run('effort:' + m.id, async () => {
      try {
        await api.post('/model_effort', { id: m.id, effort })
        toast.ok('已设置 ' + m.name + ' 的网关档位为 ' + (EFFORT_LABEL[effort] ?? effort))
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const reprobe = () => {
    void run('reprobe', async () => {
      try {
        const res = await api.get<ModelsResp>('/models', { refresh: 1 })
        q.setData(res)
        toast.ok('已重新探测，共 ' + (res.models?.length ?? 0) + ' 个模型')
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const lockColumns: Column<ModelLock>[] = [
    {
      key: 'model',
      title: '模型',
      render: (l) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="bold small">{l.model}</span>
          <span className="muted tiny">{l.realm}</span>
        </div>
      ),
    },
    {
      key: 'state',
      title: '状态',
      width: '96px',
      render: (l) => {
        const s = LOCK_STATE[l.state] ?? { text: l.state || '未知', tone: 'muted' as Tone }
        return <Badge tone={s.tone}>{s.text}</Badge>
      },
    },
    {
      key: 'locked',
      title: '被锁号数',
      align: 'right',
      width: '96px',
      render: (l) => (
        <span className="small">
          {l.locked} / {l.total}
        </span>
      ),
    },
    {
      key: 'servable',
      title: '可服务',
      align: 'right',
      width: '80px',
      render: (l) => <span className="small">{l.servable}</span>,
    },
    {
      key: 'unlock',
      title: '最早解锁',
      render: (l) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="small">{remainText(l.unlock_at, nowMs)}</span>
          <span className="muted tiny">全部解锁 {remainText(l.fully_unlock_at, nowMs)}</span>
        </div>
      ),
    },
  ]

  const columns: Column<ModelEntry>[] = [
    {
      key: 'name',
      title: '模型',
      width: '26%',
      sortValue: (m) => m.name || m.id,
      render: (m) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="bold">{m.name || splitId(m.id).bare}</span>
          <span className="muted tiny mono">{m.id}</span>
          <div className="row gap1">
            {splitId(m.id).realm ? <span className="chip">{splitId(m.id).realm}</span> : null}
            {m.vendor ? <span className="chip">{m.vendor}</span> : null}
            {m.tier ? <span className="chip">{m.tier}</span> : null}
            {m.is_default ? <span className="chip">默认</span> : null}
            {m.supports_images ? <span className="chip">图片</span> : null}
          </div>
        </div>
      ),
    },
    {
      key: 'mapping',
      title: '档位映射',
      render: (m) => {
        const wanted = m.gateway_effort_wanted || m.default_effort || ''
        const applied = m.gateway_effort || ''
        const downgraded = Boolean(wanted && applied && wanted !== applied)
        const supports = m.supported_efforts ?? []
        return (
          <div className="stack" style={{ gap: 2 }}>
            <span className="small mono">
              {EFFORT_LABEL[wanted] ?? (wanted || '—')} → {EFFORT_LABEL[applied] ?? (applied || '—')}
            </span>
            <div className="row gap1">
              {downgraded ? <Badge tone="warn">降级</Badge> : null}
              {!m.gateway_policy_enabled ? <Badge tone="muted">策略关闭</Badge> : null}
              {m.only_reasoning ? <Badge tone="muted">仅思考</Badge> : null}
            </div>
            <span className="muted tiny truncate">
              支持：{supports.length > 0 ? supports.join(' / ') : '未知'}
            </span>
          </div>
        )
      },
    },
    {
      key: 'effort',
      title: '目标档位',
      width: '128px',
      render: (m) => {
        const supports = m.supported_efforts && m.supported_efforts.length > 0 ? m.supported_efforts : FALLBACK_EFFORTS
        const value = m.gateway_effort_wanted || m.default_effort || ''
        const off = Boolean(m.can_disable_thinking)
        return (
          <select
            className="select sm"
            aria-label={'设置 ' + m.name + ' 的网关档位'}
            value={value}
            disabled={!m.gateway_policy_enabled || busy === 'effort:' + m.id}
            title={m.gateway_policy_enabled ? '写入 config thinking.model_defaults，下一次出站即生效' : '思考档位策略未开启，先在 Config 打开'}
            onChange={(e) => {
              if (e.target.value !== value) setEffort(m, e.target.value)
            }}
          >
            {value === '' ? <option value="">跟随全局</option> : null}
            {supports.map((s) => (
              <option key={s} value={s}>
                {EFFORT_LABEL[s] ?? s}
              </option>
            ))}
            {off ? <option value="off">{EFFORT_LABEL.off}</option> : null}
          </select>
        )
      },
    },
    {
      key: 'hidden',
      title: '下游可见',
      width: '104px',
      sortValue: (m) => (m.hidden ? 0 : 1),
      render: (m) => (
        <div className="row gap2 v-center">
          <Switch
            checked={!m.hidden}
            label={(m.hidden ? '放开' : '隐藏') + ' ' + m.name}
            disabled={busy === 'hidden:' + m.id}
            onChange={(next) => setHidden(m, !next)}
          />
          <span className="tiny muted">{m.hidden ? '隐藏' : '可见'}</span>
        </div>
      ),
    },
    {
      key: 'lock',
      title: '限流 / 锁定',
      width: '152px',
      render: (m) => {
        const { realm: r, bare } = splitId(m.id)
        const l = lockMap.get(r + ':' + bare)
        const b = m.busy ? (BUSY[m.busy] ?? { text: m.busy, tone: 'muted' as Tone }) : null
        return (
          <div className="stack" style={{ gap: 2 }}>
            {l ? (
              <>
                <Badge tone={(LOCK_STATE[l.state] ?? { tone: 'muted' as Tone }).tone}>
                  {(LOCK_STATE[l.state] ?? { text: '锁定' }).text}
                </Badge>
                <span className="tiny muted">
                  {l.locked}/{l.total} 号 · {remainText(l.unlock_at, nowMs)}后解锁
                </span>
              </>
            ) : (
              <Badge tone="ok">未锁定</Badge>
            )}
            {b ? (
              <span className="tiny muted">
                繁忙度：{b.text}
              </span>
            ) : null}
          </div>
        )
      },
    },
    {
      key: 'limits',
      title: '上下文 / 输出上限',
      width: '140px',
      render: (m) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="small muted">上下文 {m.context_length ? num(m.context_length) : '未知'}</span>
          <span className="small muted">输出 {m.max_output_tokens ? num(m.max_output_tokens) : '未知'}</span>
        </div>
      ),
    },
  ]

  const fetchErrors = q.data?.fetch_errors ?? []
  const busyErrs = q.data?.busy_errs ?? []
  const failed = Boolean(q.error)

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>模型与档位</h2>
          <div className="lead">上游目录、网关思考档位映射，以及当前的模型锁池</div>
        </div>
        <div className="acts">
          <button type="button" className="btn ghost" onClick={() => q.reload()} disabled={q.loading}>
            {q.loading ? '刷新中…' : '刷新'}
          </button>
          <button type="button" className="btn ghost" onClick={reprobe} disabled={busy === 'reprobe'}>
            {busy === 'reprobe' ? '探测中…' : '重新探测'}
          </button>
        </div>
      </div>

      {failed && (
        <div className="notice bad">
          <span>加载失败：{errorText(q.error)}</span>
          <button
            type="button"
            className="btn ghost sm"
            onClick={() => {
              q.reload()
              ov.reload()
            }}
          >
            重试
          </button>
        </div>
      )}

      {fetchErrors.length > 0 && (
        <div className="notice warn">部分域取数失败：{fetchErrors.join('；')}</div>
      )}

      {busyErrs.length > 0 && (
        <div className="notice warn">繁忙度观测失败（不影响目录）：{busyErrs.join('；')}</div>
      )}

      <StatsGrid>
        <Stat label="模型总数" value={q.first ? '—' : stats.total} sub={q.data?.cached ? '来自缓存' : '实时目录'} />
        <Stat label="下游可见" value={q.first ? '—' : stats.shown} tone="ok" />
        <Stat label="已隐藏" value={q.first ? '—' : stats.hidden} tone={stats.hidden > 0 ? 'warn' : undefined} sub="不对外暴露" />
        <Stat
          label="锁池模型"
          value={ov.first ? '—' : stats.lockedModels}
          tone={stats.lockedModels > 0 ? 'bad' : 'ok'}
          sub={q.data?.global_source ? 'global 来源 ' + q.data.global_source : '模型级限流'}
        />
      </StatsGrid>

      <div className="grid2">
        <Box
          title="模型锁池"
          sub="被模型级限流挡住的号 · 每 10 秒刷新"
          actions={
            <button type="button" className="btn ghost sm" onClick={() => ov.reload()} disabled={ov.loading}>
              刷新
            </button>
          }
          flush
        >
          {ov.first ? (
            <div className="tbl-state">
              <Skeleton rows={3} />
            </div>
          ) : (
            <DataTable<ModelLock>
              columns={lockColumns}
              rows={locks}
              rowKey={(l) => l.realm + ':' + l.model}
              empty="当前没有模型被锁"
              emptyHint="所有参与选号的账号都能服务各自模型，无需等待解锁。"
            />
          )}
        </Box>

        <Box title="回落映射" sub="上游模型不可用时的替代模型（config models.fallback）">
          {ov.first ? (
            <Skeleton rows={3} />
          ) : Object.keys(fallback).length === 0 ? (
            <Empty title="未配置回落映射" hint="在 Config 里设置 models.fallback 后，这里会列出「原模型 → 替代模型」。" />
          ) : (
            <KV items={Object.entries(fallback).map(([k, v]) => ({ k, v, mono: true }))} />
          )}
        </Box>
      </div>

      <Box
        title="模型列表"
        sub={models.length > 0 ? '共 ' + models.length + ' 个模型 · 档位改动即时生效' : undefined}
        actions={
          <>
            <input
              className="input"
              placeholder="搜索模型名 / ID"
              aria-label="搜索模型"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              style={{ maxWidth: 200 }}
            />
            <Segmented<RealmFilter>
              ariaLabel="按域筛选"
              value={realm}
              onChange={setRealm}
              options={[
                { value: 'all', label: '全部域' },
                { value: 'cn', label: 'cn' },
                { value: 'global', label: 'global' },
              ]}
            />
            <Segmented<VisFilter>
              ariaLabel="按可见性筛选"
              value={vis}
              onChange={setVis}
              options={[
                { value: 'all', label: '全部' },
                { value: 'shown', label: '可见' },
                { value: 'hidden', label: '隐藏' },
              ]}
            />
          </>
        }
        flush
      >
        {q.first ? (
          <div className="tbl-state">
            <Skeleton rows={6} />
          </div>
        ) : (
          <DataTable<ModelEntry>
            columns={columns}
            rows={rows}
            rowKey={(m) => m.id}
            loading={q.loading}
            empty={models.length === 0 ? '上游没有返回任何模型' : '没有匹配的模型'}
            emptyHint={
              models.length === 0 ? (
                <>
                  确认账号池里有可用账号后点「重新探测」；仍为空说明上游目录接口异常。
                  <button type="button" className="btn ghost sm" onClick={reprobe}>
                    重新探测
                  </button>
                </>
              ) : (
                '换个关键词，或把域 / 可见性筛选切回「全部」。'
              )
            }
          />
        )}
      </Box>

      <div className="muted tiny">
        档位映射左值是发给上游的目标档、右值是实际注入档（不支持时会自动降级）；「目标档位」写入 config
        thinking.model_defaults，与 Config 页同源。
      </div>
    </div>
  )
}
