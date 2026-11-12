// ApiKeys.tsx 客户端 API Key：列表 / 新建 / 编辑 / 删除 / 重置用量 / 批量启停，
// 以及「分组模板」的增删改和表单一键套用。
//
// 后端 apikeys/create|update 的 body 是平铺的 gatewaykeys.Key（Enabled 用 *bool 区分
// 「没传」与「传了 false」），整体覆盖语义：更新时必须把表单里没改的字段原样带回，
// 否则会顺手清掉可用模型 / 到期时间 / 提示词策略。字段以 internal/gatewaykeys 为准。
import { useEffect, useMemo, useRef, useState } from 'react'
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

type Template = {
  id: string
  name: string
  models: string[] | null
  credit_limit: number
  prompt_mode: string
  prompt_text: string
  expiry_days: number
  note: string
  created_at: number
  updated_at: number
}

type ListResp = {
  keys: GatewayKey[] | null
  templates: Template[] | null
  global?: { prompt_mode?: string; prompt_text?: string } | null
  now?: number
}

type ModelRow = { id: string; name?: string }
type ModelsResp = { models: ModelRow[] | null }

type Filter = 'all' | 'ok' | 'disabled' | 'bad'

type KeyForm = {
  id: string
  name: string
  secret: string
  enabled: boolean
  /** datetime-local 值；空 = 永不过期 */
  expires: string
  limit: string
  models: string[]
  prompt_mode: string
  prompt_text: string
  template_id: string
  note: string
  credit_used: number
  requests: number
  created_at: number
}

type TplForm = {
  id: string
  name: string
  expiry_days: string
  limit: string
  models: string[]
  prompt_mode: string
  prompt_text: string
  note: string
}

const STATUS: Record<string, { text: string; tone: Tone }> = {
  ok: { text: '正常', tone: 'ok' },
  disabled: { text: '已停用', tone: 'muted' },
  expired: { text: '已过期', tone: 'warn' },
  quota_exceeded: { text: '额度用尽', tone: 'bad' },
}

const MODE_OPTIONS = [
  { value: '', label: '跟随全局' },
  { value: 'custom', label: 'custom 替换' },
  { value: 'passthrough', label: 'passthrough 透传' },
  { value: 'append', label: 'append 追加' },
]

const MODE_TEXT: Record<string, string> = {
  '': '跟随全局',
  custom: 'custom 替换',
  passthrough: 'passthrough 透传',
  append: 'append 追加',
}

/** 打码展示：保留前缀与末 4 位，中间一律圆点。 */
function maskSecret(secret: string): string {
  const s = (secret || '').trim()
  if (!s) return '—'
  if (s.length <= 10) return s.slice(0, 2) + '••••'
  return s.slice(0, 5) + '••••••' + s.slice(-4)
}

/** Unix 秒 → datetime-local 需要的本地时间字符串。 */
function localInput(unix: number): string {
  if (!unix) return ''
  const d = new Date(unix * 1000)
  if (isNaN(d.getTime())) return ''
  const p = (n: number) => String(n).padStart(2, '0')
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes())
}

/** datetime-local 值 → Unix 秒；空或非法 = 0（永不过期）。 */
function unixOf(v: string): number {
  const s = v.trim()
  if (!s) return 0
  const ms = Date.parse(s)
  if (!isFinite(ms) || ms <= 0) return 0
  return Math.floor(ms / 1000)
}

function dateText(unix: number): string {
  return unix > 0 ? shortTime(ts(unix)) : '永不过期'
}

function modelsText(list: string[] | null): string {
  const arr = (list ?? []).filter(Boolean)
  return arr.length === 0 ? '不限' : arr.join('、')
}

/** 模型多选：可选 chips + 关键字过滤 + 允许手输自定义模型名。 */
function ModelPicker({
  value,
  onChange,
  catalog,
  loading,
  error,
}: {
  value: string[]
  onChange: (next: string[]) => void
  catalog: ModelRow[]
  loading: boolean
  error: string | null
}) {
  const [kw, setKw] = useState('')
  const [open, setOpen] = useState(false)
  const wrapRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    const onDoc = (e: MouseEvent) => {
      if (wrapRef.current && !wrapRef.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDoc)
    return () => document.removeEventListener('mousedown', onDoc)
  }, [])

  const q = kw.trim().toLowerCase()
  const rows = catalog.filter((m) => {
    if (!q) return true
    return m.id.toLowerCase().includes(q) || (m.name ?? '').toLowerCase().includes(q)
  })
  const known = catalog.some((m) => m.id.toLowerCase() === q || (m.name ?? '').toLowerCase() === q)
  const custom = q !== '' && !known

  const add = (id: string) => {
    const v = id.trim()
    if (!v) return
    if (value.some((x) => x.toLowerCase() === v.toLowerCase())) return
    onChange([...value, v])
  }

  const toggle = (id: string) => {
    if (value.some((x) => x.toLowerCase() === id.toLowerCase())) {
      onChange(value.filter((x) => x.toLowerCase() !== id.toLowerCase()))
    } else {
      onChange([...value, id])
    }
  }

  return (
    <div className="stack" ref={wrapRef} style={{ position: 'relative' }}>
      <div className="row gap1" style={{ flexWrap: 'wrap' }}>
        {value.length === 0 ? (
          <span className="muted small">全部模型（不限）</span>
        ) : (
          value.map((id) => (
            <span key={id} className="chip">
              <span className="mono tiny">{id}</span>
              <button
                type="button"
                className="btn ghost sm"
                aria-label={'移除 ' + id}
                onClick={() => onChange(value.filter((x) => x !== id))}
              >
                ×
              </button>
            </span>
          ))
        )}
      </div>
      <input
        className="input"
        placeholder="搜索模型，回车加入自定义模型名"
        aria-label="搜索可用模型"
        value={kw}
        onFocus={() => setOpen(true)}
        onChange={(e) => {
          setKw(e.target.value)
          setOpen(true)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            if (custom) {
              add(kw)
              setKw('')
            }
          } else if (e.key === 'Escape') {
            setOpen(false)
          } else if (e.key === 'Backspace' && kw === '' && value.length > 0) {
            onChange(value.slice(0, -1))
          }
        }}
      />
      {open ? (
        <div
          className="box"
          style={{ position: 'absolute', top: '100%', left: 0, right: 0, zIndex: 20, maxHeight: 240, overflow: 'auto', marginTop: 4 }}
        >
          <div className="muted tiny" style={{ padding: '6px 10px' }}>
            {loading
              ? '正在读取上游模型目录…'
              : error
                ? '模型目录读取失败：' + error + '，可直接输入模型名回车添加'
                : rows.length === 0
                  ? '没有匹配的模型，回车把「' + kw.trim() + '」作为自定义模型加进去'
                  : '点选加入；输入 custom 模型名如 cn:xxx 后回车也可加入'}
          </div>
          {rows.slice(0, 200).map((m) => {
            const on = value.some((x) => x.toLowerCase() === m.id.toLowerCase())
            return (
              <label key={m.id} className="row gap2 v-center" style={{ padding: '6px 10px', cursor: 'pointer' }}>
                <input type="checkbox" checked={on} onChange={() => toggle(m.id)} />
                <span className="small truncate">{m.name || m.id}</span>
                <span className="muted tiny mono truncate">{m.id}</span>
              </label>
            )
          })}
          {value
            .filter((v) => !catalog.some((m) => m.id.toLowerCase() === v.toLowerCase()))
            .map((v) => (
              <div key={'custom:' + v} className="row gap2 v-center" style={{ padding: '6px 10px' }}>
                <Badge tone="accent">自定义</Badge>
                <span className="small mono truncate">{v}</span>
                <button type="button" className="btn ghost sm" onClick={() => onChange(value.filter((x) => x !== v))}>
                  移除
                </button>
              </div>
            ))}
        </div>
      ) : null}
    </div>
  )
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
  const [form, setForm] = useState<KeyForm | null>(null)
  const [created, setCreated] = useState<GatewayKey | null>(null)
  const [delTarget, setDelTarget] = useState<GatewayKey | null>(null)
  const [resetTarget, setResetTarget] = useState<GatewayKey | null>(null)
  const [tplForm, setTplForm] = useState<TplForm | null>(null)
  const [tplDel, setTplDel] = useState<Template | null>(null)

  const keys = useMemo(() => q.data?.keys ?? [], [q.data])
  const templates = useMemo(() => q.data?.templates ?? [], [q.data])
  const global = q.data?.global ?? null
  const unavailable = q.error instanceof ApiError && q.error.status === 501

  // 模型目录只在打开弹层时拉一次，和旧面板的「全站复用一份目录」一致。
  const catalogQ = useQuery<ModelsResp>(
    (signal) => api.get<ModelsResp>('/models', undefined, signal),
    [tick],
    { enabled: form !== null || tplForm !== null },
  )
  const catalog = catalogQ.data?.models ?? []
  const catalogErr = catalogQ.error ? errorText(catalogQ.error) : null

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
    let requests = 0
    for (const k of keys) {
      if (k.enabled) enabled++
      if (k.status === 'expired' || k.status === 'quota_exceeded') bad++
      used += k.credit_used || 0
      requests += k.requests || 0
    }
    return { total: keys.length, enabled, bad, used, requests }
  }, [keys])

  const openCreate = () =>
    setForm({
      id: '',
      name: '',
      secret: '',
      enabled: true,
      expires: '',
      limit: '',
      models: [],
      prompt_mode: '',
      prompt_text: '',
      template_id: '',
      note: '',
      credit_used: 0,
      requests: 0,
      created_at: 0,
    })

  const openEdit = (k: GatewayKey) => {
    setForm({
      id: k.id,
      name: k.name,
      secret: k.secret,
      enabled: k.enabled,
      expires: localInput(k.expires_at),
      limit: k.credit_limit > 0 ? String(k.credit_limit) : '',
      models: (k.models ?? []).slice(),
      prompt_mode: k.prompt_mode ?? '',
      prompt_text: k.prompt_text ?? '',
      template_id: k.template_id ?? '',
      note: k.note ?? '',
      credit_used: k.credit_used || 0,
      requests: k.requests || 0,
      created_at: k.created_at || 0,
    })
  }

  /** 套用模板：覆盖模型 / 额度 / 提示词；到期天数只在新建时按天推算。 */
  const applyTemplate = (tplId: string) => {
    if (!form) return
    const tpl = templates.find((t) => t.id === tplId)
    if (!tpl) {
      setForm({ ...form, template_id: tplId })
      return
    }
    const next: KeyForm = {
      ...form,
      template_id: tpl.id,
      models: (tpl.models ?? []).slice(),
      limit: tpl.credit_limit > 0 ? String(tpl.credit_limit) : '',
      prompt_mode: tpl.prompt_mode ?? '',
      prompt_text: tpl.prompt_text ?? '',
    }
    if (!form.id && tpl.expiry_days > 0) {
      next.expires = localInput(Math.floor(Date.now() / 1000) + tpl.expiry_days * 86400)
    }
    setForm(next)
    toast.ok('已套用模板「' + tpl.name + '」，保存前可继续改')
  }

  const submit = () => {
    if (!form) return
    const name = form.name.trim()
    if (!name) {
      toast.err('名称不能为空')
      return
    }
    const raw = form.limit.trim()
    const limit = raw === '' ? 0 : Number(raw)
    if (!isFinite(limit) || limit < 0) {
      toast.err('积分限额必须是不小于 0 的数字（0 = 不限）')
      return
    }
    const secret = form.secret.trim()
    if (/\s/.test(secret)) {
      toast.err('自定义 Key 不能包含空白字符')
      return
    }
    const body = {
      id: form.id,
      name,
      secret,
      enabled: form.enabled,
      expires_at: unixOf(form.expires),
      credit_limit: limit,
      models: form.models,
      prompt_mode: form.prompt_mode,
      prompt_text: form.prompt_text,
      template_id: form.template_id,
      note: form.note.trim(),
    }
    void run('save', async () => {
      try {
        if (form.id) {
          await api.post('/apikeys/update', body)
          toast.ok('已保存')
          setForm(null)
        } else {
          const res = await api.post<{ ok: boolean; key: GatewayKey }>('/apikeys/create', body)
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
        await api.post('/apikeys/update', {
          id: k.id,
          name: k.name,
          secret: k.secret,
          enabled: next,
          expires_at: k.expires_at,
          credit_limit: k.credit_limit,
          models: k.models ?? [],
          prompt_mode: k.prompt_mode,
          prompt_text: k.prompt_text,
          template_id: k.template_id,
          note: k.note,
        })
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
          await api.post('/apikeys/update', {
            id: k.id,
            name: k.name,
            secret: k.secret,
            enabled: next,
            expires_at: k.expires_at,
            credit_limit: k.credit_limit,
            models: k.models ?? [],
            prompt_mode: k.prompt_mode,
            prompt_text: k.prompt_text,
            template_id: k.template_id,
            note: k.note,
          })
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

  const openTplCreate = () =>
    setTplForm({ id: '', name: '', expiry_days: '', limit: '', models: [], prompt_mode: '', prompt_text: '', note: '' })

  const openTplEdit = (t: Template) =>
    setTplForm({
      id: t.id,
      name: t.name,
      expiry_days: t.expiry_days > 0 ? String(t.expiry_days) : '',
      limit: t.credit_limit > 0 ? String(t.credit_limit) : '',
      models: (t.models ?? []).slice(),
      prompt_mode: t.prompt_mode ?? '',
      prompt_text: t.prompt_text ?? '',
      note: t.note ?? '',
    })

  const submitTpl = () => {
    if (!tplForm) return
    const name = tplForm.name.trim()
    if (!name) {
      toast.err('模板名称必填')
      return
    }
    const rawLimit = tplForm.limit.trim()
    const limit = rawLimit === '' ? 0 : Number(rawLimit)
    if (!isFinite(limit) || limit < 0) {
      toast.err('积分限额必须是不小于 0 的数字（0 = 不限）')
      return
    }
    const rawDays = tplForm.expiry_days.trim()
    const days = rawDays === '' ? 0 : Number(rawDays)
    if (!Number.isInteger(days) || days < 0) {
      toast.err('有效天数必须是不小于 0 的整数（0 = 不设到期）')
      return
    }
    void run('tplSave', async () => {
      try {
        await api.post('/apikey_templates/save', {
          id: tplForm.id,
          name,
          credit_limit: limit,
          expiry_days: days,
          models: tplForm.models,
          prompt_mode: tplForm.prompt_mode,
          prompt_text: tplForm.prompt_text,
          note: tplForm.note.trim(),
        })
        toast.ok(tplForm.id ? '模板已保存' : '模板已创建')
        setTplForm(null)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const removeTpl = () => {
    const target = tplDel
    if (!target) return
    void run('tplDel', async () => {
      try {
        await api.post('/apikey_templates/delete', { id: target.id })
        toast.ok('已删除模板「' + target.name + '」')
        setTplDel(null)
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
      width: '20%',
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
      width: '150px',
      sortValue: (k) => k.status,
      render: (k) => {
        const s = STATUS[k.status] ?? { text: k.status || '未知', tone: 'muted' as Tone }
        return (
          <div className="stack" style={{ gap: 2 }}>
            <div className="row gap2 v-center">
              <Badge tone={s.tone}>{s.text}</Badge>
              <Switch
                checked={k.enabled}
                label={'启用 ' + k.name}
                disabled={busy === 'toggle:' + k.id}
                onChange={(next) => toggle(k, next)}
              />
            </div>
            <span className="muted tiny">{dateText(k.expires_at)}</span>
          </div>
        )
      },
    },
    {
      key: 'models',
      title: '可用模型',
      render: (k) => (
        <span className="small truncate" title={modelsText(k.models)} style={{ maxWidth: 200, display: 'inline-block' }}>
          {modelsText(k.models)}
        </span>
      ),
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
      key: 'prompt',
      title: '提示词',
      render: (k) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="small">{MODE_TEXT[k.prompt_mode ?? ''] ?? k.prompt_mode}</span>
          {k.prompt_text ? (
            <span className="muted tiny truncate" title={k.prompt_text} style={{ maxWidth: 180, display: 'inline-block' }}>
              {k.prompt_text}
            </span>
          ) : null}
        </div>
      ),
    },
    {
      key: 'last_used_at',
      title: '最近使用',
      width: '120px',
      sortValue: (k) => k.last_used_at,
      render: (k) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="small muted">{k.last_used_at ? ago(ts(k.last_used_at)) : '从未使用'}</span>
          <span className="tiny muted">创建 {shortTime(ts(k.created_at))}</span>
        </div>
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

  const tplColumns: Column<Template>[] = [
    {
      key: 'name',
      title: '名称',
      render: (t) => (
        <div className="stack" style={{ gap: 2 }}>
          <span className="bold small">{t.name}</span>
          {t.note ? <span className="muted tiny">{t.note}</span> : null}
        </div>
      ),
    },
    {
      key: 'models',
      title: '可用模型',
      render: (t) => (
        <span className="small truncate" title={modelsText(t.models)} style={{ maxWidth: 220, display: 'inline-block' }}>
          {modelsText(t.models)}
        </span>
      ),
    },
    {
      key: 'limit',
      title: '额度',
      width: '96px',
      render: (t) => <span className="small">{t.credit_limit > 0 ? credits(t.credit_limit) : '不限'}</span>,
    },
    {
      key: 'mode',
      title: '提示词',
      width: '130px',
      render: (t) => <span className="small">{MODE_TEXT[t.prompt_mode ?? ''] ?? t.prompt_mode}</span>,
    },
    {
      key: 'days',
      title: '有效天数',
      width: '100px',
      render: (t) => <span className="small">{t.expiry_days > 0 ? t.expiry_days + ' 天' : '不设到期'}</span>,
    },
    {
      key: 'ops',
      title: '操作',
      width: '140px',
      render: (t) => (
        <div className="row gap1">
          <button type="button" className="btn ghost sm" onClick={() => openTplEdit(t)}>
            编辑
          </button>
          <button type="button" className="btn ghost sm" onClick={() => setTplDel(t)}>
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
          <div className="lead">签发给客户端的准入凭证：额度、到期、可用模型与提示词策略</div>
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
        <Stat
          label="异常"
          value={q.first ? '—' : stats.bad}
          tone={stats.bad > 0 ? 'bad' : undefined}
          sub="过期或额度用尽"
        />
        <Stat
          label="已用积分"
          value={q.first ? '—' : credits(stats.used)}
          sub={stats.requests > 0 ? stats.requests + ' 次请求' : '所有 Key 累计'}
        />
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
                <button type="button" className="btn ghost sm" disabled={busy === 'batch'} onClick={() => setMany(true)}>
                  批量启用
                </button>
                <button type="button" className="btn ghost sm" disabled={busy === 'batch'} onClick={() => setMany(false)}>
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
                    点右上「新建 Key」签发第一把，或先在「分组模板」里把常用配置存成一包。
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

      <Box
        title="分组模板"
        sub={templates.length > 0 ? '共 ' + templates.length + ' 个 · 新建 Key 时可一键套用' : undefined}
        actions={
          <button type="button" className="btn primary sm" onClick={openTplCreate}>
            新建模板
          </button>
        }
        flush
      >
        {q.first ? (
          <div className="tbl-state">
            <Skeleton rows={3} />
          </div>
        ) : (
          <DataTable<Template>
            columns={tplColumns}
            rows={templates}
            rowKey={(t) => t.id}
            empty="还没有模板"
            emptyHint={
              <>
                把「试用 7 天 / 指定模型 + 额度」这类常用配置存下来，新建 Key 时一键套用。
                <button type="button" className="btn primary sm" onClick={openTplCreate}>
                  新建模板
                </button>
              </>
            }
          />
        )}
      </Box>

      <Modal
        open={form !== null}
        title={form?.id ? '编辑 API Key' : '新建 API Key'}
        onClose={() => setForm(null)}
        labelId="apikey-form-title"
        wide
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
          <div className="stack">
            <Field
              label="套用模板"
              hint={
                form.id
                  ? '套用会覆盖可用模型 / 额度 / 提示词；到期时间不会自动改'
                  : '套用会覆盖可用模型 / 额度 / 提示词，并按模板天数设置到期'
              }
            >
              <select
                className="select"
                aria-label="套用分组模板"
                value={form.template_id}
                onChange={(e) => applyTemplate(e.target.value)}
              >
                <option value="">不套用模板</option>
                {templates.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
                {form.template_id && !templates.some((t) => t.id === form.template_id) ? (
                  <option value={form.template_id}>（模板已删除）</option>
                ) : null}
              </select>
            </Field>

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
              <Field label="积分限额" hint="留空或 0 = 不限额度" htmlFor="ak-limit">
                <input
                  id="ak-limit"
                  className="input"
                  inputMode="decimal"
                  placeholder="0"
                  value={form.limit}
                  onChange={(e) => setForm({ ...form, limit: e.target.value })}
                />
              </Field>
              <Field
                label="到期时间"
                hint="留空 = 永不过期"
                htmlFor="ak-expires"
              >
                <input
                  id="ak-expires"
                  className="input"
                  type="datetime-local"
                  value={form.expires}
                  onChange={(e) => setForm({ ...form, expires: e.target.value })}
                />
              </Field>
              <Field
                label="自定义 Key"
                hint={form.id ? 'Key 创建后不可修改' : '留空自动生成 sk- 开头的 24 位随机串'}
                htmlFor="ak-secret"
              >
                <input
                  id="ak-secret"
                  className="input mono"
                  placeholder={form.id ? '（已创建，不可改）' : '留空自动生成'}
                  value={form.secret}
                  readOnly={Boolean(form.id)}
                  onChange={(e) => setForm({ ...form, secret: e.target.value })}
                />
              </Field>
            </div>

            <Field label="可用模型" hint="空 = 全部模型；可用 cn: / global: 前缀限定域">
              <ModelPicker
                value={form.models}
                onChange={(next) => setForm({ ...form, models: next })}
                catalog={catalog}
                loading={catalogQ.first}
                error={catalogErr}
              />
            </Field>

            <div className="formgrid">
              <Field label="提示词模式" htmlFor="ak-mode">
                <select
                  id="ak-mode"
                  className="select"
                  value={form.prompt_mode}
                  onChange={(e) => setForm({ ...form, prompt_mode: e.target.value })}
                >
                  {MODE_OPTIONS.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
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

            <Field label="提示词正文" hint="custom 整体替换 / append 追加到系统提示词 / passthrough 透传调用方提示词">
              <textarea
                className="textarea"
                rows={3}
                placeholder="留空表示不使用"
                value={form.prompt_text}
                onChange={(e) => setForm({ ...form, prompt_text: e.target.value })}
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

            {form.id ? (
              <div className="muted tiny">
                已用 {credits(form.credit_used)} · 请求 {form.requests} 次 · 创建 {form.created_at ? shortTime(ts(form.created_at)) : '—'}
              </div>
            ) : null}
          </div>
        )}
      </Modal>

      <Modal
        open={tplForm !== null}
        title={tplForm?.id ? '编辑模板' : '新建模板'}
        onClose={() => setTplForm(null)}
        labelId="apikey-tpl-title"
        wide
        footer={
          <>
            <button type="button" className="btn ghost" onClick={() => setTplForm(null)} disabled={busy === 'tplSave'}>
              取消
            </button>
            <button type="button" className="btn primary" onClick={submitTpl} disabled={busy === 'tplSave'}>
              {busy === 'tplSave' ? '保存中…' : '保存'}
            </button>
          </>
        }
      >
        {tplForm ? (
          <div className="stack">
            <div className="formgrid">
              <Field label="模板名称" htmlFor="tpl-name">
                <input
                  id="tpl-name"
                  className="input"
                  placeholder="例如：试用 7 天"
                  value={tplForm.name}
                  onChange={(e) => setTplForm({ ...tplForm, name: e.target.value })}
                />
              </Field>
              <Field label="有效天数" hint="0 或留空 = 不设到期" htmlFor="tpl-days">
                <input
                  id="tpl-days"
                  className="input"
                  inputMode="numeric"
                  placeholder="0"
                  value={tplForm.expiry_days}
                  onChange={(e) => setTplForm({ ...tplForm, expiry_days: e.target.value })}
                />
              </Field>
              <Field label="积分限额" hint="0 或留空 = 不限" htmlFor="tpl-limit">
                <input
                  id="tpl-limit"
                  className="input"
                  inputMode="decimal"
                  placeholder="0"
                  value={tplForm.limit}
                  onChange={(e) => setTplForm({ ...tplForm, limit: e.target.value })}
                />
              </Field>
              <Field label="提示词模式" htmlFor="tpl-mode">
                <select
                  id="tpl-mode"
                  className="select"
                  value={tplForm.prompt_mode}
                  onChange={(e) => setTplForm({ ...tplForm, prompt_mode: e.target.value })}
                >
                  {MODE_OPTIONS.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </Field>
            </div>

            <Field label="可用模型" hint="空 = 全部模型">
              <ModelPicker
                value={tplForm.models}
                onChange={(next) => setTplForm({ ...tplForm, models: next })}
                catalog={catalog}
                loading={catalogQ.first}
                error={catalogErr}
              />
            </Field>

            <Field label="提示词正文">
              <textarea
                className="textarea"
                rows={3}
                placeholder="留空表示不使用"
                value={tplForm.prompt_text}
                onChange={(e) => setTplForm({ ...tplForm, prompt_text: e.target.value })}
              />
            </Field>

            <Field label="备注" htmlFor="tpl-note">
              <input
                id="tpl-note"
                className="input"
                placeholder="可选"
                value={tplForm.note}
                onChange={(e) => setTplForm({ ...tplForm, note: e.target.value })}
              />
            </Field>
          </div>
        ) : null}
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
              {created.credit_limit > 0 ? credits(created.credit_limit) : '不限'} · 到期：
              {dateText(created.expires_at)}
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

      <ConfirmDialog
        open={tplDel !== null}
        title="删除模板"
        danger
        confirmText="删除"
        busy={busy === 'tplDel'}
        onClose={() => setTplDel(null)}
        onConfirm={removeTpl}
        message={
          <>
            删除模板「{tplDel?.name}」？已套用它的 Key 不受影响（配置已复制到 Key 上）。
          </>
        }
      />

      <div className="muted tiny">
        全局提示词：{global ? (MODE_TEXT[global.prompt_mode ?? ''] ?? '未设置（内置默认）') : '—'} · 模板{' '}
        {templates.length} 个 · 完整 Key 只在创建时返回一次
      </div>
    </div>
  )
}