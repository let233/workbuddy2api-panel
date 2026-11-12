import { useMemo, useState, type ReactNode } from 'react'
import { api, errorText } from '../lib/api'
import { useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { bytes, num } from '../lib/format'
import { Badge, Box, Empty, Field, KV, Select, Segmented, Skeleton, Switch } from '../components/ui'

type ConfigResp = {
  ok: boolean
  path: string
  config: Record<string, unknown>
  environment_managed_fields: string[] | null
}

type SaveResp = {
  ok: boolean
  restart_required: string[] | null
  environment_managed_fields: string[] | null
}

type Kind = 'text' | 'number' | 'float' | 'switch' | 'duration' | 'select' | 'hours' | 'list' | 'textarea'

type FieldDef = {
  path: string
  label: string
  kind: Kind
  hint?: string
  options?: { value: string; label: string }[]
  min?: number
  max?: number
  step?: number
  placeholder?: string
  /** 分域字段：cn = 国内版，global = 国际版；不写即共享配置，任何分域视图下都显示 */
  realm?: 'cn' | 'global'
}

type Group = {
  title: string
  sub?: string
  /** 组末补充说明 */
  note?: string
  fields: FieldDef[]
}

const TLS_OPTIONS = [
  { value: 'node', label: 'node（默认指纹）' },
  { value: 'go', label: 'go（原生指纹）' },
]

const GROUPS: Group[] = [
  {
    title: '服务与面板',
    sub: '监听地址与面板刷新节奏；数据库/状态文件等仅可从配置文件修改',
    fields: [
      { path: 'listen', label: '监听地址', kind: 'text', placeholder: '127.0.0.1:7863', hint: '保存后热切换监听' },
      { path: 'panel.poll_interval_sec', label: '面板自动刷新（秒）', kind: 'number', hint: '0 = 默认 5 秒；负数 = 关闭自动刷新' },
      { path: 'panel.package_detail_limit', label: '套餐明细条数', kind: 'number', hint: '≤0 回落 5 条' },
      { path: 'server.read_timeout', label: '请求读超时', kind: 'duration', placeholder: '300s', hint: 'Go 时长写法，0 = 不限制' },
      { path: 'server.shutdown_timeout', label: '优雅退出超时', kind: 'duration', placeholder: '30s' },
      { path: 'server.max_payload_bytes', label: '最大请求体（字节）', kind: 'number', hint: '64 MiB = 67108864' },
    ],
  },
  {
    title: '账号与轮询',
    fields: [
      { path: 'accounts.reload_interval', label: '账号目录扫描间隔', kind: 'duration', placeholder: '0s', hint: '0s = 关闭轮询' },
      { path: 'accounts.reload_delete_confirm', label: '删除确认次数', kind: 'number', hint: '目录里账号消失需连续确认几次才删除' },
      { path: 'schedule.include_disabled_in_tasks', label: '定时任务包含已禁用账号', kind: 'switch' },
    ],
  },
  {
    title: '定时任务',
    sub: '时段用 0–23 小时、逗号分隔；留空表示当天不执行',
    fields: [
      { path: 'schedule.checkin_enabled', label: '每日签到', kind: 'switch' },
      { path: 'schedule.checkin_hours', label: '签到时段', kind: 'hours', placeholder: '9, 21' },
      { path: 'schedule.growth_enabled', label: '成长任务队列', kind: 'switch' },
      { path: 'schedule.growth_hours', label: '成长时段', kind: 'hours', placeholder: '11' },
      { path: 'schedule.growth_concurrency', label: '成长并发', kind: 'number', min: 1, max: 4 },
      { path: 'schedule.travel_enabled', label: '猫猫旅行', kind: 'switch' },
      { path: 'schedule.travel_hours', label: '旅行时段', kind: 'hours', placeholder: '9, 21' },
      { path: 'schedule.activity_enabled', label: '点亮连登', kind: 'switch' },
      { path: 'schedule.activity_hours', label: '点亮时段', kind: 'hours', placeholder: '10' },
      { path: 'schedule.web_activity_enabled', label: '网页活动上报', kind: 'switch' },
      { path: 'schedule.keepalive_enabled', label: '保活刷新', kind: 'switch' },
      { path: 'schedule.keepalive_hours', label: '保活时段', kind: 'hours', placeholder: '22' },
      { path: 'schedule.keepalive_refresh_window', label: '保活刷新窗口', kind: 'duration', placeholder: '168h' },
      { path: 'schedule.balance_refresh_enabled', label: '余额定时刷新', kind: 'switch' },
      { path: 'schedule.balance_refresh_minutes', label: '余额刷新间隔（分）', kind: 'number', min: 1 },
      { path: 'schedule.task_sweep_enabled', label: '任务清扫', kind: 'switch' },
      { path: 'schedule.task_sweep_minutes', label: '任务清扫间隔（分）', kind: 'number', min: 1 },
      { path: 'schedule.jitter_minutes', label: '抖动窗口（分）', kind: 'number', hint: '在时段前后随机偏移，避免同一秒打满' },
      { path: 'schedule.account_spread_minutes', label: '账号间错峰（分）', kind: 'number' },
      { path: 'schedule.account_spread_min_gap_seconds', label: '错峰最小间隔（秒）', kind: 'number' },
      { path: 'schedule.balance_refresh_jitter_permill', label: '余额刷新抖动（‰）', kind: 'number', min: 0, max: 500 },
      { path: 'schedule.task_sweep_jitter_permill', label: '任务清扫抖动（‰）', kind: 'number', min: 0, max: 500 },
      { path: 'schedule.makeup_cards_per_round', label: '每轮补签上限', kind: 'number', hint: '0 = 不限' },
      { path: 'schedule.window_probe_enabled', label: '窗口探测', kind: 'switch' },
    ],
  },
  {
    title: '账号池与流量治理',
    sub: '并发、熔断、降权与选号权重；改动会立即影响转发行为',
    fields: [
      { path: 'pool.max_in_flight', label: '单账号在途上限', kind: 'number', hint: '0 = 不限' },
      { path: 'pool.max_in_flight_global', label: 'global 域在途上限', kind: 'number', hint: '0 = 回落默认 2' },
      { path: 'pool.selection_strategy', label: '选号策略', kind: 'select', options: [
        { value: 'weighted', label: 'weighted（加权）' },
        { value: 'round_robin', label: 'round_robin（轮询）' },
        { value: 'sequential', label: 'sequential（顺序）' },
      ] },
      { path: 'pool.breaker_threshold', label: '熔断阈值（连败次数）', kind: 'number', min: 1 },
      { path: 'pool.breaker_cooldown', label: '熔断基础时长', kind: 'duration', placeholder: '30m' },
      { path: 'pool.breaker_cooldown_max', label: '熔断封顶时长', kind: 'duration', placeholder: '6h' },
      { path: 'pool.degrade_threshold', label: '降权阈值（连败次数）', kind: 'number', min: 1 },
      { path: 'pool.degrade_cooldown', label: '降权时长', kind: 'duration', placeholder: '10m' },
      { path: 'pool.degrade_cooldown_max', label: '降权封顶时长', kind: 'duration', placeholder: '2h' },
      { path: 'pool.idle_weight_per_hour', label: '闲置权重补偿/小时', kind: 'float', step: 0.1 },
      { path: 'pool.idle_weight_max', label: '闲置权重封顶', kind: 'float', step: 0.1 },
      { path: 'pool.prefer_expiring', label: '优先用临期额度', kind: 'switch' },
      { path: 'pool.expiring_soon', label: '临期判定窗口', kind: 'duration', placeholder: '720h' },
      { path: 'pool.cost_explore_interval', label: '成本探索间隔', kind: 'duration', placeholder: '30m' },
      { path: 'pool.balance_window', label: '余额统计窗口', kind: 'duration', placeholder: '1h' },
      { path: 'pool.credit_floor', label: '额度下限', kind: 'number', hint: '低于该值不再派单' },
      { path: 'pool.credit_floor_max_rate', label: '下限最大抽成比例', kind: 'float', step: 0.01 },
      { path: 'pool.reserve_credits', label: '保留额度', kind: 'number', hint: '缺省 10' },
    ],
  },
  {
    title: '限额与冷却',
    fields: [
      { path: 'budget.daily_credit_limit', label: '每日额度上限', kind: 'float', step: 0.1, hint: '0 = 不限' },
      { path: 'budget.burst_credit_limit', label: '突发额度上限', kind: 'float', step: 0.1 },
      { path: 'cooldown.soft_rate', label: '软限流冷却基数', kind: 'duration', placeholder: '600s' },
      { path: 'cooldown.soft_rate_max', label: '软限流冷却上限', kind: 'duration', placeholder: '2h' },
    ],
  },
  {
    title: '上游与网络',
    fields: [
      { path: 'upstream.timeout_seconds', label: '上游超时（秒）', kind: 'number', min: 1 },
      { path: 'upstream.header_timeout_seconds', label: '首包超时（秒）', kind: 'number', hint: '0 = 回落总超时' },
      { path: 'upstream.idle_timeout_seconds', label: '空闲超时（秒）', kind: 'number', hint: '0 = 默认 300' },
      { path: 'upstream.tls_fingerprint', label: 'TLS 指纹', kind: 'select', options: TLS_OPTIONS },
      { path: 'upstream.user_agent', label: 'User-Agent', kind: 'text', hint: '空 = 使用内置默认' },
      { path: 'upstream.client_version', label: '客户端版本', kind: 'text' },
      { path: 'upstream.cli_version', label: 'CLI 版本', kind: 'text' },
      { path: 'upstream.client_name', label: '客户端名', kind: 'text' },
      { path: 'upstream.passthrough_ip', label: '透传客户端 IP', kind: 'switch' },
      { path: 'upstream.dump_dir', label: '抓包目录', kind: 'text', hint: '空 = 不落盘' },
      { path: 'upstream.dump_max_bytes', label: '单次抓包上限（字节）', kind: 'number' },
      { path: 'upstream.restriction_hold_seconds', label: '限制全池暂停（秒）', kind: 'number', hint: '0 = 关闭' },
      { path: 'dns_servers', label: 'DNS 服务器', kind: 'list', placeholder: '1.1.1.1, 8.8.8.8', hint: '逗号分隔，留空用系统默认' },
      { path: 'global.chat_base', label: '国际版对话地址', kind: 'text', realm: 'global', hint: '按账号 domain 选官方主机；空 = 内置默认' },
      { path: 'global.billing_base', label: '国际版计费地址', kind: 'text', realm: 'global', hint: '同国际版对话地址口径；空 = 内置默认' },
      { path: 'cn.chat_base', label: '国内版对话地址', kind: 'text', realm: 'cn', hint: '覆盖国内版对话 / 模型目录上游域名；空 = 内置默认' },
      { path: 'cn.billing_base', label: '国内版计费地址', kind: 'text', realm: 'cn', hint: '覆盖国内版积分 / 成长任务上游域名；空 = 内置默认' },
    ],
    note: '模型级分域在「模型」页用 cn: / global: 前缀的白名单 / 黑名单实现，这里只配域名 base。',
  },
  {
    title: '自动更新',
    sub: '定时检查 GitHub Release；镜像与代理用于受限网络',
    fields: [
      { path: 'update.auto_enabled', label: '自动检查更新', kind: 'switch' },
      { path: 'update.auto_time', label: '每日检查时间', kind: 'text', placeholder: '04:30', hint: 'HH:MM，上海时区' },
      { path: 'update.mirror', label: '资产镜像前缀', kind: 'text', placeholder: 'https://ghproxy.example.com/', hint: '空 = 直连 GitHub' },
      { path: 'update.proxy', label: '更新代理', kind: 'text', placeholder: 'socks5://127.0.0.1:1080', hint: '支持 http/https/socks5，空 = 不使用代理' },
      { path: 'update.proxy_from_env', label: '读取出站环境变量代理', kind: 'switch', hint: '关闭后完全不读 HTTP_PROXY / HTTPS_PROXY' },
    ],
  },
  {
    title: '日志与归档',
    fields: [
      { path: 'logging.request_archive_enabled', label: '请求事件归档', kind: 'switch' },
      { path: 'logging.request_retention_days', label: '请求归档保留天数', kind: 'number', min: 1 },
      { path: 'logging.request_archive_max_mb', label: '请求归档上限（MB）', kind: 'number', min: 1 },
      { path: 'logging.request_client_info', label: '记录调用来源 IP/UA', kind: 'switch' },
      { path: 'logging.request_archive_context', label: '记录会话上下文', kind: 'switch', hint: '含用户提示词，默认关闭' },
      { path: 'logging.panel_log_enabled', label: '面板日志落盘', kind: 'switch' },
      { path: 'logging.panel_log_retention_days', label: '面板日志保留天数', kind: 'number', min: 1 },
      { path: 'logging.panel_log_max_mb', label: '面板日志上限（MB）', kind: 'number', min: 1 },
    ],
  },
  {
    title: '安全与高级',
    sub: '指纹清洗、品牌遮挡、模型名单与思考档位；改动影响所有转发请求',
    fields: [
      { path: 'features.sanitize_blacklist_fingerprints', label: '清洗黑名单指纹', kind: 'switch' },
      { path: 'features.sanitize_brand_scope', label: '品牌遮挡范围', kind: 'select', options: [
        { value: 'all', label: 'all（全部）' },
        { value: 'identity', label: 'identity（仅身份）' },
      ] },
      { path: 'features.embeddings_enabled', label: '允许 embeddings', kind: 'switch' },
      { path: 'features.watermark_strip_enabled', label: '去除水印', kind: 'switch' },
      { path: 'models.hidden', label: '隐藏模型', kind: 'list', placeholder: 'model-a, model-b', hint: '逗号分隔' },
      { path: 'models.allowed', label: '白名单模型', kind: 'list', placeholder: 'model-a', hint: '留空 = 不限制' },
      { path: 'models.aliases', label: '模型别名', kind: 'list', placeholder: 'client=real', hint: '逗号分隔，形如 client=real' },
      { path: 'thinking.enabled', label: '思考档位改写', kind: 'switch' },
      { path: 'thinking.default_effort', label: '默认档位', kind: 'select', options: [
        { value: 'low', label: 'low' },
        { value: 'medium', label: 'medium' },
        { value: 'high', label: 'high' },
        { value: 'xhigh', label: 'xhigh' },
        { value: 'max', label: 'max' },
      ] },
      { path: 'thinking.client_priority', label: '客户端档位优先', kind: 'switch' },
      { path: 'thinking.override_scope', label: '改写范围', kind: 'select', options: [
        { value: 'level', label: 'level（仅档位）' },
        { value: 'all', label: 'all（全部）' },
      ] },
      { path: 'prompt.mode', label: '提示词注入模式', kind: 'select', options: [
        { value: 'passthrough', label: 'passthrough（原样）' },
        { value: 'custom', label: 'custom（替换）' },
        { value: 'append', label: 'append（追加）' },
      ] },
      { path: 'prompt.text', label: '自定义提示词', kind: 'textarea', placeholder: '留空使用内置提示词', hint: '空 = 内置默认；模式为 custom（替换）或 append（追加）时生效' },
      { path: 'prompt.keep_recent_rounds', label: '保留最近轮数', kind: 'number', hint: '0 = 全量' },
      { path: 'session_sticky.enabled', label: '会话粘性', kind: 'switch' },
      { path: 'session_sticky.ttl', label: '粘性 TTL', kind: 'duration', placeholder: '30m' },
      { path: 'session_sticky.gc_interval', label: '粘性 GC 间隔', kind: 'duration', placeholder: '5m' },
      { path: 'admin.enabled', label: '管理接口', kind: 'switch' },
      { path: 'admin.audit_enabled', label: '管理审计', kind: 'switch' },
      { path: 'metrics.enabled', label: '指标端点', kind: 'switch' },
    ],
  },
]

function getPath(obj: unknown, path: string): unknown {
  let cur: unknown = obj
  for (const key of path.split('.')) {
    if (typeof cur !== 'object' || cur === null) return undefined
    cur = (cur as Record<string, unknown>)[key]
  }
  return cur
}

function setPath(target: Record<string, unknown>, path: string, value: unknown): void {
  const keys = path.split('.')
  let cur = target
  for (let i = 0; i < keys.length - 1; i++) {
    const key = keys[i]
    const next = cur[key]
    if (typeof next !== 'object' || next === null || Array.isArray(next)) cur[key] = {}
    cur = cur[key] as Record<string, unknown>
  }
  cur[keys[keys.length - 1]] = value
}

function hoursText(v: unknown): string {
  return Array.isArray(v) ? v.join(', ') : ''
}

function parseHours(text: string): number[] {
  const out: number[] = []
  for (const part of text.split(/[,，\s]+/)) {
    if (part === '') continue
    const n = Number(part)
    if (Number.isFinite(n) && n >= 0 && n <= 23 && !out.includes(Math.trunc(n))) out.push(Math.trunc(n))
  }
  return out.sort((a, b) => a - b)
}

// 空字符串表示「回到内置默认」的字段；其余文本字段留空不下发，避免把默认值覆盖成空。
const CLEARABLE = new Set([
  'upstream.user_agent',
  'prompt.text',
  'cn.chat_base',
  'cn.billing_base',
  'global.chat_base',
  'global.billing_base',
])

function parseList(text: string): string[] {
  return text
    .split(/[,，\s]+/)
    .map((s) => s.trim())
    .filter((s) => s !== '')
}

// Go 时长：30m / 2h / 600s / 1h30m。留空表示「不修改」，交给保存逻辑跳过，不当错误。
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/

function durationOk(v: unknown): boolean {
  if (typeof v !== 'string') return true
  const s = v.trim()
  if (s === '') return true
  return DURATION_RE.test(s)
}

const FIELD_MAP = new Map<string, FieldDef>()
for (const g of GROUPS) for (const fd of g.fields) FIELD_MAP.set(fd.path, fd)

type Realm = 'all' | 'cn' | 'global'

const REALM_OPTIONS: { value: Realm; label: string }[] = [
  { value: 'all', label: '全部' },
  { value: 'cn', label: '国内版' },
  { value: 'global', label: '国际版' },
]

export default function Config() {
  const { tick } = useRefresh()
  const toast = useToast()
  const q = useQuery<ConfigResp>((signal) => api.get<ConfigResp>('/config', undefined, signal), [tick])
  const [edits, setEdits] = useState<Record<string, unknown>>({})
  const [saving, setSaving] = useState(false)
  const [restartFields, setRestartFields] = useState<string[]>([])
  // 分域视图开关：只影响显示哪些分域字段，共享配置始终显示；保存仍提交整份改动
  const [realm, setRealm] = useState<Realm>('all')

  const envManaged = useMemo(() => new Set(q.data?.environment_managed_fields ?? []), [q.data])
  const cfg = q.data?.config
  const dirtyPaths = Object.keys(edits)
  const dirty = dirtyPaths.length > 0
  const touchedEnv = dirtyPaths.filter((p) => envManaged.has(p))

  const read = (path: string): unknown => (path in edits ? edits[path] : getPath(cfg, path))
  const write = (path: string, value: unknown) => {
    setEdits((prev) => ({ ...prev, [path]: value }))
  }

  const discard = () => {
    setEdits({})
    toast.info('已放弃未保存的修改')
  }

  const save = () => {
    if (!dirty) return
    const badDurations = dirtyPaths.filter((p) => {
      const fd = FIELD_MAP.get(p)
      return fd?.kind === 'duration' && !durationOk(edits[p])
    })
    if (badDurations.length > 0) {
      toast.err(
        '时长格式不对：' +
          badDurations.map((p) => FIELD_MAP.get(p)?.label ?? p).join('、') +
          '（应写成 30m / 2h / 600s / 1h30m）',
      )
      return
    }
    const body: Record<string, unknown> = {}
    let skipped = 0
    for (const path of dirtyPaths) {
      const value = edits[path]
      if (typeof value === 'string' && value === '' && !CLEARABLE.has(path)) {
        skipped += 1
        continue
      }
      setPath(body, path, value)
    }
    if (Object.keys(body).length === 0) {
      setEdits({})
      toast.warn(skipped > 0 ? '没有可提交的改动：这些字段留空表示不修改' : '没有可提交的改动')
      return
    }
    setSaving(true)
    api
      .post<SaveResp>('/config', body)
      .then((res) => {
        const need = res.restart_required ?? []
        setRestartFields(need)
        setEdits({})
        toast.ok(need.length > 0 ? `已保存，需重启生效：${need.join('、')}` : '配置已保存并生效')
        if (skipped > 0) toast.info(`已忽略 ${skipped} 个留空字段（该字段留空表示不修改）`)
        q.reload()
      })
      .catch((e: unknown) => {
        toast.err(errorText(e))
      })
      .finally(() => {
        setSaving(false)
      })
  }

  const renderField = (f: FieldDef) => {
    const managed = envManaged.has(f.path)
    const v = read(f.path)
    const hint = managed ? `${f.hint ? f.hint + '；' : ''}环境变量托管，保存不生效` : f.hint
    let control: ReactNode
    if (f.kind === 'switch') {
      control = (
        <div className="row">
          <Switch checked={v === true} disabled={managed} onChange={(next) => write(f.path, next)} label={f.label} />
          <span className="tiny muted">{v === true ? '启用' : '关闭'}</span>
        </div>
      )
    } else if (f.kind === 'select') {
      control = (
        <Select
          ariaLabel={f.label}
          value={typeof v === 'string' ? v : ''}
          disabled={managed}
          onChange={(next) => write(f.path, next)}
          menuMaxHeight={280}
          options={(f.options ?? []).map((o) => ({ value: o.value, label: o.label }))}
        />
      )
    } else if (f.kind === 'number' || f.kind === 'float') {
      control = (
        <input
          className="input"
          type="number"
          inputMode="decimal"
          min={f.min}
          max={f.max}
          step={f.step ?? 1}
          value={typeof v === 'number' ? v : ''}
          placeholder={f.placeholder}
          disabled={managed}
          onChange={(e) => write(f.path, e.target.value === '' ? 0 : Number(e.target.value))}
        />
      )
    } else if (f.kind === 'hours') {
      control = (
        <input
          className="input"
          value={hoursText(v)}
          placeholder={f.placeholder}
          disabled={managed}
          onChange={(e) => write(f.path, parseHours(e.target.value))}
        />
      )
    } else if (f.kind === 'list') {
      control = (
        <input
          className="input"
          value={Array.isArray(v) ? (v as string[]).join(', ') : ''}
          placeholder={f.placeholder}
          disabled={managed}
          onChange={(e) => write(f.path, parseList(e.target.value))}
        />
      )
    } else if (f.kind === 'textarea') {
      control = (
        <textarea
          className="textarea"
          rows={4}
          value={typeof v === 'string' ? v : ''}
          placeholder={f.placeholder}
          disabled={managed}
          onChange={(e) => write(f.path, e.target.value)}
        />
      )
    } else {
      control = (
        <input
          className="input"
          value={typeof v === 'string' ? v : ''}
          placeholder={f.placeholder}
          disabled={managed}
          onChange={(e) => write(f.path, e.target.value)}
        />
      )
    }
    const badDuration = f.kind === 'duration' && !durationOk(v)
    const field = (
      <Field
        label={managed ? f.label + '（托管）' : f.label}
        hint={hint}
        error={badDuration ? '格式应为 Go 时长：30m / 2h / 600s / 1h30m' : undefined}
      >
        {control}
      </Field>
    )
    return f.kind === 'textarea' ? (
      <div className="span2" key={f.path}>
        {field}
      </div>
    ) : (
      <div key={f.path}>{field}</div>
    )
  }

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>配置</h2>
          <p className="lead">改动提交后立即热生效；标「需重启」的字段必须重启进程</p>
        </div>
        <div className="acts">
          <button className="btn sm" onClick={q.reload} disabled={q.loading}>
            重新读取
          </button>
        </div>
      </div>

      <div className="toolbar">
        <span className="small muted">分域视图</span>
        <div className="segmented-scroll">
          <Segmented<Realm> value={realm} options={REALM_OPTIONS} onChange={setRealm} ariaLabel="配置分域" />
        </div>
        <span className="tiny muted">
          切换只决定显示哪些分域字段（域名 base 等），共享配置始终显示；保存仍提交全部改动。
        </span>
      </div>

      {q.error ? (
        <div className="notice bad">
          <span>配置读取失败：{errorText(q.error)}</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      {restartFields.length > 0 ? (
        <div className="notice warn">
          <span>已保存，以下字段需重启进程生效：{restartFields.join('、')}</span>
        </div>
      ) : null}
      {touchedEnv.length > 0 ? (
        <div className="notice warn">
          <span>这些字段由环境变量托管，保存后仍以环境变量为准：{touchedEnv.join('、')}</span>
        </div>
      ) : null}
      {dirty ? (
        <div className="notice ok">
          <span>
            有 {num(dirtyPaths.length)} 项未保存的修改
          </span>
          <button className="btn sm primary" onClick={save} disabled={saving}>
            {saving ? '保存中…' : '保存'}
          </button>
          <button className="btn sm ghost" onClick={discard} disabled={saving}>
            重置
          </button>
        </div>
      ) : null}

      {!cfg ? (
        q.error ? (
          <Box>
            <Empty title="配置不可用" hint="请检查后端是否正常，再点上方「重试」。" />
          </Box>
        ) : (
          <Box>
            <Skeleton rows={8} />
          </Box>
        )
      ) : (
        <>
          {GROUPS.map((g) => {
            const fields = g.fields.filter((fd) => realm === 'all' || !fd.realm || fd.realm === realm)
            if (fields.length === 0) return null
            return (
              <Box key={g.title} title={g.title} sub={g.sub}>
                <div className="formgrid">{fields.map(renderField)}</div>
                {g.note ? <p className="tiny muted">{g.note}</p> : null}
              </Box>
            )
          })}

          <Box title="只读项" sub="以下配置只能改配置文件，部分需重启生效">
            <KV
              items={[
                { k: '配置文件路径', v: q.data?.path ?? '-', mono: true },
                { k: '认证目录', v: String(read('auth_dir') ?? '-'), mono: true },
                { k: '状态文件', v: String(read('state_file') ?? '-'), mono: true },
                { k: '静态目录', v: String(read('panel.public_dir') ?? '-'), mono: true },
                {
                  k: '数据库',
                  v: `${String(getPath(cfg, 'database.driver') ?? '-')}（需重启生效）`,
                  mono: true,
                },
                { k: '数据源', v: String(getPath(cfg, 'database.dsn') || '未配置（本地状态文件）'), mono: true },
              ]}
            />
            <p className="tiny muted">修改 listen 会自动切换监听，但处于灰度探针后的端口需自行放通。</p>
          </Box>
        </>
      )}

      {dirty ? (
        <div className="actionbar">
          <button className="btn" onClick={discard} disabled={saving}>
            放弃修改
          </button>
          <button className="btn primary" onClick={save} disabled={saving}>
            {saving ? '保存中…' : '保存配置'}
          </button>
        </div>
      ) : null}

      {q.data ? (
        <p className="tiny muted">
          <Badge tone="muted">已就绪</Badge> 配置大小约 {bytes(JSON.stringify(q.data.config).length)} · 共{' '}
          {num(GROUPS.reduce((n, g) => n + g.fields.length, 0))} 个可编辑字段
        </p>
      ) : null}
    </div>
  )
}
