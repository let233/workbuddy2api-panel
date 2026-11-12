// ApiKeys.tsx 客户端 API Key：列表 + 新建/编辑 + 一次性展示明文 + 删除/重置用量 + 批量启停。
// 后端 apikeys 接口是整体覆盖语义（详见 updateBody 的注释），所以任何一次更新都把
// 未在表单里编辑的字段原样带回去，避免顺手清掉可用模型 / 到期时间 / 提示词策略。
import { useMemo, useState } from 'react'
import { ApiError, api, errorText } from '../lib/api'
import { ago, credits, shortTime, ts } from '../lib/format'
import { useBusy, useDebounced, useQuery, useSelection } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { DataTable, type Column } from '../components/DataTable'
import {
  Badge,
  Bar,
  Box,
  ConfirmDialog,
  CopyText,
  Field,
  Modal,
  Segmented,
  Skeleton,
  Stat,
  StatsGrid,
  Switch,
  type Tone,
} from '../components/ui'

type GatewayKey = {
  id: string
  name: string
  secret: string
  enabled: boolean
  expires_at: number
  credit_limit: number
  credit_used: number
  models: string[] | null
  prompt_mode: string
  prompt_text: string
  template_id: string
  note: string
  created_at: number
  updated_at: number
  last_used_at: number
  requests: number
  tokens: number
  status: string
}

type ListResp = { keys: GatewayKey[] | null; templates: unknown[] | null }

type FormState = {
  id: string
  name: string
  credit_limit: string
  note: string
  enabled: boolean
}

type Filter = 'all' | 'ok' | 'disabled' | 'bad'

const STATUS: Record<string, { text: string; tone: Tone }> = {
  ok: { text: '正常', tone: 'ok' },
  disabled: { text: '已停用', tone: 'muted' },
  expired: { text: '已过期', tone: 'warn' },
  quota_exceeded: { text: '额度用尽', tone: 'bad' },
}

/** 打码展示：保留前缀与末 4 位，中间一律圆点。 */
function maskSecret(secret: string): string {
  const s = (secret || '').trim()
  if (!s) return '—'
  if (s.length <= 10) return s.slice(0, 2) + '••••'
  return s.slice(0, 5) + '••••••' + s.slice(-4)
}

/** update 需要的完整旧值（不含用量计数，服务端自行保留）。 */
function updateBody(k: GatewayKey) {
  return {
    id: k.id,
    enabled: k.enabled,
    expires_at: k.expires_at,
    credit_limit: k.credit_limit,
    models: k.models ?? [],
    prompt_mode: k.prompt_mode,
    prompt_text: k.prompt_text,
    template_id: k.template_id,
    note: k.note,
  }
}

export default function ApiKeys() {
  const { tick } = useRefresh()
  const toast = useToast()
  const { busy, run } = useBusy()
  const sel = useSelection<string>()

  const q = useQuery<ListResp>((signal) => api.get<ListResp>('/apikeys', undefined, signal), [tick])

  const [search, setSearch] = useState('')
  const debounced = useDebounced(search, 200)
  const [filter, setFilter] = useState<Filter>('all')
  const [form, setForm] = useState<FormState | null>(null)
  const [created, setCreated] = useState<GatewayKey | null>(null)
  const [delTarget, setDelTarget] = useState<GatewayKey | null>(null)
  const [resetTarget, setResetTarget] = useState<GatewayKey | null>(null)

  const keys = useMemo(() => q.data?.keys ?? [], [q.data])
  const unavailable = q.error instanceof ApiError && q.error.status === 501

  const rows = useMemo(() => {
    const needle = debounced.trim().toLowerCase()
    return keys.filter((k) => {
      if (filter === 'ok' && k.status !== 'ok') return false
      if (filter === 'disabled' && k.status !== 'disabled') return false
      if (filter === 'bad' && k.status !== 'expired' && k.status !== 'quota_exceeded') return false
      if (!needle) return true
      return (
        k.name.toLowerCase().includes(needle) ||
        k.note.toLowerCase().includes(needle) ||
        k.id.toLowerCase().includes(needle) ||
        k.secret.toLowerCase().includes(needle)
      )
    })
  }, [keys, debounced, filter])

  const stats = useMemo(() => {
    let enabled = 0
    let bad = 0
    let used = 0
    for (const k of keys) {
      if (k.enabled) enabled++
      if (k.status === 'expired' || k.status === 'quota_exceeded') bad++
      used += k.credit_used || 0
    }
    return { total: keys.length, enabled, bad, used }
  }, [keys])

  const openCreate = () => setForm({ id: '', name: '', credit_limit: '', note: '', enabled: true })

  const openEdit = (k: GatewayKey) => {
    setForm({
      id: k.id,
      name: k.name,
      credit_limit: k.credit_limit > 0 ? String(k.credit_limit) : '',
      note: k.note,
      enabled: k.enabled,
    })
  }

  const submit = () => {
    if (!form) return
    const name = form.name.trim()
    if (!name) {
      toast.err('名称不能为空')
      return
    }
    const raw = form.credit_limit.trim()
    const limit = raw === '' ? 0 : Number(raw)
    if (!isFinite(limit) || limit < 0) {
      toast.err('限额必须是不小于 0 的数字（0 = 不限）')
      return
    }
    void run('save', async () => {
      try {
        if (form.id) {
          const base = keys.find((k) => k.id === form.id)
          if (!base) {
            toast.err('该 Key 已不存在，刷新列表后重试')
            return
          }
          await api.post('/apikeys/update', {
            ...updateBody(base),
            name,
            credit_limit: limit,
            note: form.note.trim(),
            enabled: form.enabled,
          })
          toast.ok('已保存')
          setForm(null)
        } else {
          const res = await api.post<{ ok: boolean; key: GatewayKey }>('/apikeys/create', {
            name,
            credit_limit: limit,
            note: form.note.trim(),
            enabled: form.enabled,
          })
          setForm(null)
          setCreated(res.key)
          toast.ok('已创建 API Key')
        }
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const toggle = (k: GatewayKey, next: boolean) => {
    void run('toggle:' + k.id, async () => {
      try {
        await api.post('/apikeys/update', { ...updateBody(k), enabled: next })
        toast.ok(next ? '已启用' : '已停用')
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const setMany = (next: boolean) => {
    void run('batch', async () => {
      const targets = keys.filter((k) => sel.set.has(k.id))
      if (targets.length === 0) return
      try {
        for (const k of targets) {
          await api.post('/apikeys/update', { ...updateBody(k), enabled: next })
        }
        toast.ok((next ? '已启用 ' : '已停用 ') + targets.length + ' 把 Key')
        sel.clear()
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const remove = () => {
    const target = delTarget
    if (!target) return
    void run('delete', async () => {
      try {
        await api.post('/apikeys/delete', { id: target.id })
        toast.ok('已删除「' + target.name + '」')
        setDelTarget(null)
        sel.clear()
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const resetUsage = () => {
    const target = resetTarget
    if (!target) return
    void run('reset', async () => {
      try {
        await api.post('/apikeys/reset_usage', { id: target.id })
        toast.ok('已重置「' + target.name + '」的用量')
        setResetTarget(null)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const columns: Column<GatewayKey>[] = [
    {
      key: 'name',
      title: '名称',
      width: '22%',
      sortValue: (k) => k.name,
      render: (k) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="bold">{k.name}</span>
          {k.note ? <span className="muted tiny">{k.note}</span> : null}
          <span className="muted tiny mono">{k.id}</span>
        </div>
      ),
    },
    {
      key: 'secret',
      title: 'Key',
      render: (k) => (
        <div className="row gap2 v-center">
          <span className="mono small truncate">{maskSecret(k.secret)}</span>
          <CopyText value={k.secret} />
        </div>
      ),
    },
    {
      key: 'status',
      title: '状态',
      width: '148px',
      sortValue: (k) => k.status,
      render: (k) => {
        const s = STATUS[k.status] ?? { text: k.status || '未知', tone: 'muted' as Tone }
        return (
          <div className="row gap2 v-center">
            <Badge tone={s.tone}>{s.text}</Badge>
            <Switch
              checked={k.enabled}
              label={'启用 ' + k.name}
              disabled={busy === 'toggle:' + k.id}
              onChange={(next) => toggle(k, next)}
            />
          </div>
        )
      },
    },
    {
      key: 'used',
      title: '限额 / 已用',
      sortValue: (k) => k.credit_used,
      render: (k) => {
        if (!(k.credit_limit > 0)) {
          return (
            <div className="stack" style={{ gap: 2 }}>
              <span className="muted small">不限额度</span>
              <span className="tiny muted">已用 {credits(k.credit_used)}</span>
            </div>
          )
        }
        const ratio = (k.credit_used / k.credit_limit) * 100
        return (
          <div className="stack" style={{ gap: 2 }}>
            <span className="small">
              {credits(k.credit_used)} / {credits(k.credit_limit)}
            </span>
            <Bar pct={ratio} tone={ratio >= 100 ? 'bad' : ratio >= 80 ? 'warn' : 'ok'} />
          </div>
        )
      },
    },
    {
      key: 'created_at',
      title: '创建',
      width: '110px',
      sortValue: (k) => k.created_at,
      render: (k) => <span className="small muted">{shortTime(ts(k.created_at))}</span>,
    },
    {
      key: 'last_used_at',
      title: '最近使用',
      width: '110px',
      sortValue: (k) => k.last_used_at,
      render: (k) =>
        k.last_used_at ? (
          <span className="small muted">{ago(ts(k.last_used_at))}</span>
        ) : (
          <span className="small muted">从未使用</span>
        ),
    },
    {
      key: 'ops',
      title: '操作',
      width: '196px',
      render: (k) => (
        <div className="row gap1">
          <button type="button" className="btn ghost sm" onClick={() => openEdit(k)}>
            编辑
          </button>
          <button type="button" className="btn ghost sm" onClick={() => setResetTarget(k)}>
            重置用量
          </button>
          <button type="button" className="btn ghost sm" onClick={() => setDelTarget(k)}>
            删除
          </button>
        </div>
      ),
    },
  ]

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>API Key</h2>
          <div className="lead">签发给客户端的准入凭证，可限定额度与启停状态</div>
        </div>
        <div className="acts">
          <button type="button" className="btn ghost" onClick={() => q.reload()} disabled={q.loading}>
            {q.loading ? '刷新中…' : '刷新'}
          </button>
          <button type="button" className="btn primary" onClick={openCreate}>
            新建 Key
          </button>
        </div>
      </div>

      {unavailable && (
        <div className="notice warn">
          网关 Key 功能未启用，先在配置文件里打开 gateway_keys 后再回到本页。
        </div>
      )}

      {q.error && !unavailable ? (
        <div className="notice bad">
          <span>加载失败：{errorText(q.error)}</span>
          <button type="button" className="btn ghost sm" onClick={() => q.reload()}>
            重试
          </button>
        </div>
      ) : null}

      <StatsGrid>
        <Stat label="Key 总数" value={q.first ? '—' : stats.total} sub="含停用与过期" />
        <Stat label="启用中" value={q.first ? '—' : stats.enabled} tone="ok" sub="可正常调用" />
        <Stat label="异常" value={q.first ? '—' : stats.bad} tone={stats.bad > 0 ? 'bad' : undefined} sub="过期或额度用尽" />
        <Stat label="已用积分" value={q.first ? '—' : credits(stats.used)} sub="所有 Key 累计" />
      </StatsGrid>

      <Box
        title="Key 列表"
        sub={keys.length > 0 ? '共 ' + keys.length + ' 把' : undefined}
        actions={
          <>
            <input
              className="input"
              placeholder="搜索名称 / 备注 / Key"
              aria-label="搜索 API Key"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              style={{ maxWidth: 220 }}
            />
            <Segmented<Filter>
              ariaLabel="按状态筛选"
              value={filter}
              onChange={setFilter}
              options={[
                { value: 'all', label: '全部' },
                { value: 'ok', label: '正常' },
                { value: 'disabled', label: '已停用' },
                { value: 'bad', label: '异常' },
              ]}
            />
          </>
        }
        flush
      >
        {q.first ? (
          <div className="tbl-state">
            <Skeleton rows={5} />
          </div>
        ) : (
          <>
            {sel.size > 0 && (
              <div className="toolbar" style={{ padding: 'var(--sp-3) var(--sp-4)', borderBottom: '1px solid var(--line)' }}>
                <span className="small">已选 {sel.size} 把</span>
                <button
                  type="button"
                  className="btn ghost sm"
                  disabled={busy === 'batch'}
                  onClick={() => setMany(true)}
                >
                  批量启用
                </button>
                <button
                  type="button"
                  className="btn ghost sm"
                  disabled={busy === 'batch'}
                  onClick={() => setMany(false)}
                >
                  批量停用
                </button>
                <button type="button" className="btn ghost sm" onClick={() => sel.clear()}>
                  取消选择
                </button>
              </div>
            )}
            <DataTable<GatewayKey>
              columns={columns}
              rows={rows}
              rowKey={(k) => k.id}
              loading={q.loading}
              selectable
              selected={sel.set}
              onToggle={(key) => sel.toggle(key)}
              onToggleAll={(next, ks) => {
                if (next) sel.addMany(ks)
                else sel.clear()
              }}
              empty={keys.length === 0 ? '还没有 API Key' : '没有匹配的 Key'}
              emptyHint={
                keys.length === 0 ? (
                  <>
                    点击右上角「新建 Key」创建第一把，创建后完整 Key 只展示一次。
                    <button type="button" className="btn primary sm" onClick={openCreate}>
                      新建 Key
                    </button>
                  </>
                ) : (
                  '换个关键词或把状态筛选切回「全部」。'
                )
              }
            />
          </>
        )}
      </Box>

      <Modal
        open={form !== null}
        title={form?.id ? '编辑 API Key' : '新建 API Key'}
        onClose={() => setForm(null)}
        labelId="apikey-form-title"
        footer={
          <>
            <button type="button" className="btn ghost" onClick={() => setForm(null)} disabled={busy === 'save'}>
              取消
            </button>
            <button type="button" className="btn primary" onClick={submit} disabled={busy === 'save'}>
              {busy === 'save' ? '保存中…' : form?.id ? '保存' : '创建'}
            </button>
          </>
        }
      >
        {form && (
          <div className="formgrid">
            <Field label="名称" htmlFor="ak-name">
              <input
                id="ak-name"
                className="input"
                placeholder="例如：张三-测试"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </Field>
            <Field label="积分限额" hint="填 0 或留空表示不限额度" htmlFor="ak-limit">
              <input
                id="ak-limit"
                className="input"
                inputMode="decimal"
                placeholder="0"
                value={form.credit_limit}
                onChange={(e) => setForm({ ...form, credit_limit: e.target.value })}
              />
            </Field>
            <Field label="备注" htmlFor="ak-note" hint="给自己看的说明，不会展示给调用方">
              <input
                id="ak-note"
                className="input"
                placeholder="可选"
                value={form.note}
                onChange={(e) => setForm({ ...form, note: e.target.value })}
              />
            </Field>
            <Field label="状态" hint={form.enabled ? '启用的 Key 可立即调用' : '停用后调用会被拒绝'}>
              <div className="row gap2 v-center">
                <Switch
                  checked={form.enabled}
                  label="启用该 Key"
                  onChange={(next) => setForm({ ...form, enabled: next })}
                />
                <span className="small">{form.enabled ? '启用' : '停用'}</span>
              </div>
            </Field>
          </div>
        )}
      </Modal>

      <Modal
        open={created !== null}
        title="Key 已创建"
        onClose={() => setCreated(null)}
        labelId="apikey-created-title"
        footer={
          <button type="button" className="btn primary" onClick={() => setCreated(null)}>
            我已保存
          </button>
        }
      >
        {created && (
          <div className="stack">
            <div className="notice warn">
              完整 Key 只在这里显示一次，关闭后无法再次查看；请现在复制并妥善保存。
            </div>
            <div className="row gap2 v-center">
              <code className="mono" style={{ overflowWrap: 'anywhere' }}>
                {created.secret}
              </code>
              <CopyText value={created.secret} label="复制 Key" />
            </div>
            <div className="small muted">
              名称：{created.name} · 状态：{created.enabled ? '启用' : '停用'} · 限额：
              {created.credit_limit > 0 ? credits(created.credit_limit) : '不限'}
            </div>
          </div>
        )}
      </Modal>

      <ConfirmDialog
        open={delTarget !== null}
        title="删除 API Key"
        danger
        confirmText="删除"
        busy={busy === 'delete'}
        onClose={() => setDelTarget(null)}
        onConfirm={remove}
        message={
          <>
            确定删除「{delTarget?.name}」？删除后该 Key 立即失效，调用方会收到鉴权失败，且无法恢复。
          </>
        }
      />

      <ConfirmDialog
        open={resetTarget !== null}
        title="重置用量"
        confirmText="重置"
        busy={busy === 'reset'}
        onClose={() => setResetTarget(null)}
        onConfirm={resetUsage}
        message={
          <>
            把「{resetTarget?.name}」的已用积分与请求计数清零？重置后额度重新可用，历史用量统计不再保留。
          </>
        }
      />

      {keys.length === 0 && !q.first && !q.error && (
        <div className="muted small">
          提示：第一把 Key 通常用于自测，建议先设一个小额度。
        </div>
      )}

      {keys.length > 0 && (
        <div className="muted tiny">
          完整 Key 只在创建时返回一次；列表里的 Key 均为打码展示，复制按钮会复制后端返回的完整值。
        </div>
      )}
    </div>
  )
}
