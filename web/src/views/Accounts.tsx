// Accounts.tsx 账号池：指标概览、分页筛选、批量动作、导入导出与详情弹层。
import { useEffect, useMemo, useRef, useState } from 'react'
import { ApiError, api, errorText } from '../lib/api'
import { useBusy, useDebounced, useQuery, useSelection } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { ago, clock, credits, cstDate, duration, isZeroTime, num, shortTime } from '../lib/format'
import {
  Badge,
  Bar,
  Box,
  ConfirmDialog,
  CopyText,
  Field,
  KV,
  Menu,
  Modal,
  Pager,
  Pre,
  Segmented,
  Select,
  Skeleton,
  Stat,
  StatsGrid,
  Tabs,
  type MenuItem,
  type Tone,
} from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'

type Bucket = { date: string; expires_at?: string; remain: number }

type Account = {
  uid: string
  nickname?: string
  credits?: number
  credits_total?: number
  reserve_blocked?: boolean
  session_dead_fails?: number
  credits_expiring?: number
  credits_earliest_expiry?: string
  credits_earliest_remaining?: number
  credits_expiry_buckets?: Bucket[]
  cooling?: boolean
  cool_kind?: string
  cool_remaining_sec?: number
  until?: string
  reason?: string
  realm?: string
  enterprise?: boolean
  api_key?: boolean
  disabled?: boolean
  disabled_reason?: string
  paused?: boolean
  group?: string
  success_count?: number
  err_total?: number
  last_success?: string
  last_err?: string
  checkin_done?: boolean
  consecutive_fails?: number
  in_flight?: number
}

type AccountsResp = {
  accounts?: Account[]
  total: number
  page: number
  size: number
  pages: number
  realm?: string
  group?: string
  counts?: { all?: number; cn?: number; global?: number }
  groups?: { name: string; count: number }[]
  source?: string
}

type OverviewResp = {
  total?: number
  healthy?: number
  cooling?: number
  disabled?: number
  paused?: number
  credits_sum?: number
  sticky_sessions?: number
}

type ProxyView = { id: string; name: string; enabled?: boolean; status?: string; latency_ms?: number }
type Binding = { mode?: string; proxy_id?: string }
type ProxySnap = { proxies?: ProxyView[]; bindings?: Record<string, Binding> }

type ProbeResp = { verdict?: string; detail?: string; advice?: string }
type BalanceResp = { ok?: boolean; credits?: number; credits_total?: number }

type CheckinResp = {
  ok?: boolean
  realm?: string
  skipped?: boolean
  skip_reason?: string
  checkin_done?: boolean
  checkin_message?: string
  checkin_error?: string
  enterprise?: boolean
  api_key?: boolean
}

type LoginStart = { url?: string; state?: string; realm?: string; site?: string }
type LoginPoll = {
  done?: boolean
  fatal?: boolean
  message?: string
  uid?: string
  nickname?: string
  realm?: string
  credits?: number
  credits_total?: number
  balance_error?: string
  checkin_message?: string
}

type RosterItem = {
  uid: string
  nickname?: string
  realm?: string
  group?: string
  paused?: boolean
  disabled?: boolean
  enterprise?: boolean
  api_key?: boolean
}
type RosterResp = { accounts?: RosterItem[]; total?: number }

type Filter = 'all' | 'healthy' | 'cooling' | 'disabled' | 'paused'
type ProxyMode = 'random' | 'direct' | 'fixed'
type SimpleAct = 'pause' | 'resume' | 'revive' | 'disable'
type AddTab = 'login' | 'import' | 'apikey'
type Site = 'cn' | 'workbuddy'
type AllActKind = 'checkin_all' | 'travel_all' | 'activity_all' | 'keepalive_all' | 'balance_all'

type ConfirmState =
  | { kind: 'remove' | 'disable' | 'revive' | 'group'; uids: string[]; name: string; group?: string }
  | { kind: 'all'; act: AllActKind; name: string }
  | { kind: 'export'; href: string; name: string; count: number }

function confirmTitle(c: ConfirmState | null): string {
  if (!c) return ''
  if (c.kind === 'remove') return '删除账号'
  if (c.kind === 'disable') return '禁用账号'
  if (c.kind === 'revive') return '解冻账号'
  if (c.kind === 'group') return '更新分组'
  if (c.kind === 'export') return '导出账号 JSON'
  return c.name
}

function confirmMessage(c: ConfirmState | null): string {
  if (!c) return ''
  if (c.kind === 'export') return '将导出 ' + c.count + ' 个账号，文件里是明文 accessToken / refreshToken，请妥善保管。'
  if (c.kind === 'all') return ALL_ACT_HINT[c.act]
  if (c.kind === 'remove') return '将从账号目录删除 ' + c.name + '，并尽量清掉对应授权文件。此操作不可撤销。'
  if (c.kind === 'disable') return '将禁用 ' + c.name + '，它们不再参与选号；随时可以解冻。'
  if (c.kind === 'revive') return '将解冻 ' + c.name + '，恢复参与选号。'
  return '将把 ' + c.name + ' 的分组改为「' + (c.group || '未分组') + '」。'
}

function confirmAction(c: ConfirmState | null): string {
  if (!c) return '确认'
  if (c.kind === 'all') return '开始'
  if (c.kind === 'export') return '下载'
  if (c.kind === 'remove') return '删除'
  if (c.kind === 'disable') return '禁用'
  if (c.kind === 'revive') return '解冻'
  return '保存'
}

const FILTERS: { value: Filter; label: string }[] = [
  { value: 'all', label: '全部状态' },
  { value: 'healthy', label: '健康' },
  { value: 'cooling', label: '冷却' },
  { value: 'disabled', label: '禁用' },
  { value: 'paused', label: '暂停' },
]

const PROXY_OPTIONS: { value: ProxyMode; label: string }[] = [
  { value: 'random', label: '随机池' },
  { value: 'direct', label: '直连' },
  { value: 'fixed', label: '固定节点' },
]

const ADD_TABS: { value: AddTab; label: string }[] = [
  { value: 'login', label: '浏览器登录' },
  { value: 'import', label: '导入 JSON' },
  { value: 'apikey', label: 'API Key' },
]

const SITE_OPTIONS = [
  { value: 'cn' as Site, label: '国内版 CN' },
  { value: 'workbuddy' as Site, label: '国际版 Global' },
]

const ALL_ACTS: { act: AllActKind; label: string }[] = [
  { act: 'checkin_all', label: '全部签到' },
  { act: 'travel_all', label: '旅行巡检' },
  { act: 'activity_all', label: '活跃上报' },
  { act: 'keepalive_all', label: '全部保活' },
  { act: 'balance_all', label: '全部刷新余额' },
]

const ALL_ACT_HINT: Record<AllActKind, string> = {
  checkin_all: '对全部国内版账号逐个签到，后台异步执行，耗时较长。',
  travel_all: '对全部账号做一次旅行巡检（校对注册地区），后台异步执行。',
  activity_all: '对全部账号做一次活跃上报，后台异步执行。',
  keepalive_all: '对全部账号做一次保活续期，后台异步执行。',
  balance_all: '从上游重新拉取全部账号的积分余额，会同步昵称与解冻状态，可能耗时几十秒。',
}

const VERDICT: Record<string, { tone: Tone; text: string }> = {
  lifted: { tone: 'ok', text: '上游已解封，可以点「解冻」恢复使用' },
  still_banned: { tone: 'warn', text: '仍被封禁，继续等待；勿反复解冻，可能加重风控' },
  blocked: { tone: 'warn', text: '会话仍有效，但被上游风控拦截（可能仍是封禁），继续观察' },
  session_dead: { tone: 'warn', text: '不是封禁问题：登录态失效，需要重新登录' },
  inconclusive: { tone: 'warn', text: '探测无明确结论（网络/限流等），稍后再试' },
  unknown: { tone: 'bad', text: '无法探测：上游客户端未配置' },
}

function acctName(s: Account): string {
  const n = (s.nickname || '').trim()
  return n || s.uid.slice(0, 8)
}

function statusOf(s: Account): { text: string; tone: Tone } {
  if (s.disabled) return { text: '禁用', tone: 'bad' }
  if (s.paused) return { text: '暂停', tone: 'accent' }
  if (s.cooling) return { text: '冷却', tone: 'warn' }
  if (s.reserve_blocked) return { text: '保留停牌', tone: 'warn' }
  return { text: '健康', tone: 'ok' }
}

function matchFilter(s: Account, f: Filter): boolean {
  if (f === 'all') return true
  if (f === 'disabled') return !!s.disabled
  if (f === 'paused') return !!s.paused
  if (f === 'cooling') return !!s.cooling
  return !s.disabled && !s.paused && !s.cooling
}

function msOf(v?: string): number {
  if (!v) return 0
  const t = Date.parse(v)
  return Number.isNaN(t) ? 0 : t
}

/** 本地时间戳，供导出文件名用（不能 toISOString：那是 UTC）。 */
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

/** 签到结果文案，对齐旧面板 checkinResultNote 的口径。 */
function checkinNote(r: CheckinResp): { tone: 'ok' | 'warn' | 'bad'; text: string } {
  if (r.skipped) return { tone: 'warn', text: r.skip_reason || '该账号不适用签到' }
  if (!r.checkin_done) {
    return { tone: 'warn', text: '签到未完成：' + (r.checkin_error || r.checkin_message || '请稍后重试') }
  }
  return { tone: 'ok', text: '签到已完成' + (r.checkin_message ? '：' + r.checkin_message : '') }
}

/** 备注只存本机 localStorage，键是 uid 前 8 位（与旧面板同一份数据）。 */
const LS_REMARKS = 'wb2api.remarks.v1'

function remarkKey(uid: string): string {
  return uid.slice(0, 8)
}

function readRemarks(): Record<string, string> {
  try {
    const raw = localStorage.getItem(LS_REMARKS)
    if (!raw) return {}
    const parsed: unknown = JSON.parse(raw)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
    const out: Record<string, string> = {}
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (typeof v === 'string' && v) out[k] = v
    }
    return out
  } catch {
    return {}
  }
}

function writeRemarks(m: Record<string, string>): boolean {
  try {
    localStorage.setItem(LS_REMARKS, JSON.stringify(m))
    return true
  } catch {
    return false
  }
}

type ImportResult = {
  ok?: boolean
  dry_run?: boolean
  total?: number
  imported?: number
  skipped?: number
  creates?: number
  overwrites?: number
  errors?: string[]
  async?: boolean
  job_id?: string
}

type ImportProgress = {
  state?: string
  total?: number
  processed?: number
  imported?: number
  skipped?: number
  creates?: number
  overwrites?: number
  errors?: string[]
  current?: string
  error?: string
}

export default function Accounts() {
  const { tick } = useRefresh()
  const toast = useToast()
  const { busy, run } = useBusy()

  const [realm, setRealm] = useState<'all' | 'cn' | 'global'>('all')
  const [group, setGroup] = useState('')
  const [statusFilter, setStatusFilter] = useState<Filter>('all')
  const [keyword, setKeyword] = useState('')
  const [page, setPage] = useState(1)
  const [size, setSize] = useState(20)

  const [detailUid, setDetailUid] = useState<string | null>(null)
  const [rawOpen, setRawOpen] = useState(false)
  const [probe, setProbe] = useState<ProbeResp | null>(null)
  const [proxyMode, setProxyMode] = useState<ProxyMode>('random')
  const [proxyId, setProxyId] = useState('')

  const [confirm, setConfirm] = useState<ConfirmState | null>(null)

  // 添加账号弹层：授权链接 / 导入 JSON / API Key
  const [addOpen, setAddOpen] = useState(false)
  const [addTab, setAddTab] = useState<AddTab>('login')
  const [site, setSite] = useState<Site>('cn')
  const [login, setLogin] = useState<LoginStart | null>(null)
  const [loginErr, setLoginErr] = useState('')
  const [loginNote, setLoginNote] = useState('')
  const [loginDone, setLoginDone] = useState('')
  const [keyToken, setKeyToken] = useState('')
  const [keyNick, setKeyNick] = useState('')
  const [keyUid, setKeyUid] = useState('')

  const [paste, setPaste] = useState('')
  const [importFile, setImportFile] = useState<File | null>(null)
  const [fileKey, setFileKey] = useState(0)
  const [preview, setPreview] = useState<ImportResult | null>(null)
  const [job, setJob] = useState<ImportProgress | null>(null)
  const [importErr, setImportErr] = useState('')

  // 备注（本机 localStorage）与分组（服务端字段）
  const [remarks, setRemarks] = useState<Record<string, string>>(() => readRemarks())
  const [remarkOpen, setRemarkOpen] = useState(false)
  const [rmText, setRmText] = useState<Record<string, string>>({})
  const [gpText, setGpText] = useState<Record<string, string>>({})
  const [bulkGroup, setBulkGroup] = useState('')
  const [rmKw, setRmKw] = useState('')

  const loginTimer = useRef<number | null>(null)
  const loginStateRef = useRef('')
  const loginFails = useRef(0)
  const loginStartAt = useRef(0)

  const q = useQuery<AccountsResp>(
    (signal) =>
      api.get(
        '/accounts',
        {
          page,
          size,
          realm: realm === 'all' ? undefined : realm,
          group: group || undefined,
        },
        signal,
      ),
    [page, size, realm, group, tick],
    { poll: 5000 },
  )
  const ov = useQuery<OverviewResp>((signal) => api.get('/overview', undefined, signal), [tick], {
    poll: 15000,
  })
  const px = useQuery<ProxySnap>((signal) => api.get('/proxies', undefined, signal), [tick], {
    poll: 30000,
  })
  const roster = useQuery<RosterResp>((signal) => api.get('/accounts/roster', undefined, signal), [tick])

  useEffect(() => {
    return () => {
      if (loginTimer.current !== null) window.clearInterval(loginTimer.current)
    }
  }, [])

  const sel = useSelection<string>()
  const data = q.data
  const kw = useDebounced(keyword, 250).trim().toLowerCase()

  const rows = useMemo(() => {
    const list = data?.accounts ?? []
    return list.filter((s) => {
      if (!matchFilter(s, statusFilter)) return false
      if (!kw) return true
      return (s.nickname || '').toLowerCase().indexOf(kw) >= 0 || s.uid.toLowerCase().indexOf(kw) >= 0
    })
  }, [data, statusFilter, kw])

  const rosterMap = useMemo(() => {
    const m = new Map<string, RosterItem>()
    for (const it of roster.data?.accounts ?? []) m.set(it.uid, it)
    return m
  }, [roster.data])

  const groupUids = useMemo(
    () => (roster.data?.accounts ?? []).filter((a) => a.group === group).map((a) => a.uid),
    [roster.data, group],
  )

  const proxies = px.data?.proxies ?? []
  const bindings = px.data?.bindings ?? {}
  const proxyName = (id?: string): string => {
    if (!id) return ''
    const hit = proxies.find((p) => p.id === id)
    return hit ? hit.name : id
  }
  const proxyText = (uid: string): string => {
    if (!px.data) return '—'
    const b = bindings[uid]
    if (!b || !b.mode || b.mode === 'random') return '随机池'
    if (b.mode === 'direct') return '直连'
    return proxyName(b.proxy_id) || '固定节点'
  }

  const call = (uid: string, action: string, body?: unknown) =>
    api.post('/accounts/' + encodeURIComponent(uid) + '/' + action, body)

  const doAct = (uid: string, action: SimpleAct, okMsg: string) => {
    void run(action + ':' + uid, async () => {
      try {
        await call(uid, action, action === 'disable' ? { reason: '面板手动禁用' } : undefined)
        toast.ok(okMsg)
        q.reload()
        ov.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const doBalance = (uid: string) => {
    void run('balance:' + uid, async () => {
      try {
        const r = await api.post<BalanceResp>('/accounts/' + encodeURIComponent(uid) + '/balance')
        toast.ok(typeof r.credits === 'number' ? '余额已更新：' + credits(r.credits) : '余额已更新')
        q.reload()
        ov.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const doProbe = (uid: string) => {
    void run('probe:' + uid, async () => {
      try {
        const r = await api.post<ProbeResp>('/accounts/' + encodeURIComponent(uid) + '/probe_ban')
        setProbe(r)
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  // 行内探测：详情弹层里是就地显示结果，列表行没有位置放结果面板，
  // 所以把结论文本直接弹成 toast（verdict 六态映射见 VERDICT）。
  const doProbeRow = (uid: string) => {
    void run('probe:' + uid, async () => {
      try {
        const r = await api.post<ProbeResp>('/accounts/' + encodeURIComponent(uid) + '/probe_ban')
        const v = VERDICT[r.verdict || '']
        const text = '探测结果：' + (v ? v.text : '未知结果（' + (r.verdict || '') + '）') + (r.detail ? ' · ' + r.detail : '')
        if (v?.tone === 'ok') toast.ok(text)
        else if (v?.tone === 'bad') toast.err(text)
        else toast.warn(text)
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const applyProxy = (uid: string) => {
    if (proxyMode === 'fixed' && !proxyId) {
      toast.warn('先选一个代理节点')
      return
    }
    void run('proxy:' + uid, async () => {
      try {
        const body: Binding = proxyMode === 'fixed' ? { mode: 'fixed', proxy_id: proxyId } : { mode: proxyMode }
        await api.put('/accounts/' + encodeURIComponent(uid) + '/proxy', body)
        toast.ok(
          proxyMode === 'random' ? '已改为随机池' : proxyMode === 'direct' ? '已改为直连' : '已绑定固定节点',
        )
        px.reload()
        q.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const doCheckin = (uid: string) => {
    void run('checkin:' + uid, async () => {
      try {
        const r = await api.post<CheckinResp>('/accounts/' + encodeURIComponent(uid) + '/checkin')
        const note = checkinNote(r)
        if (note.tone === 'ok') toast.ok(note.text)
        else toast.warn(note.text)
        q.reload()
        ov.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  type BatchAct = 'checkin' | 'balance' | 'pause' | 'resume' | 'revive'

  /** 与旧面板 BULK_ACT 一致：不满足条件的号直接跳过，不算失败。 */
  const batchSkip = (act: BatchAct, r?: RosterItem): boolean => {
    if (!r) return false
    if (act === 'checkin') return !!r.enterprise || !!r.api_key || !!r.disabled
    if (act === 'pause') return !!r.paused || !!r.disabled
    if (act === 'resume') return !r.paused
    if (act === 'revive') return !r.disabled
    return false
  }

  const runBatch = (act: BatchAct, label: string) => {
    const uids = sel.list
    if (!uids.length) {
      toast.warn('先勾选账号')
      return
    }
    void run('batch:' + act, async () => {
      let okCount = 0
      let skipCount = 0
      let firstErr = ''
      for (const uid of uids) {
        if (batchSkip(act, rosterMap.get(uid))) {
          skipCount++
          continue
        }
        try {
          if (act === 'checkin') {
            const r = await api.post<CheckinResp>('/accounts/' + encodeURIComponent(uid) + '/checkin')
            if (r.skipped) skipCount++
            else okCount++
          } else {
            await call(uid, act)
            okCount++
          }
        } catch (e) {
          if (!firstErr) firstErr = errorText(e)
        }
      }
      const bits = [label + '：成功 ' + okCount + ' 个']
      if (uids.length - okCount - skipCount > 0) bits.push('失败 ' + (uids.length - okCount - skipCount) + ' 个')
      if (skipCount) bits.push('跳过 ' + skipCount + ' 个')
      if (firstErr) toast.warn(bits.join('，') + '（' + firstErr + '）')
      else if (!okCount) toast.warn(bits.join('，'))
      else toast.ok(bits.join('，'))
      q.reload()
      ov.reload()
    })
  }

  /** 组内批量：作用域是该分组全部账号（来自 roster），不是当前页。 */
  const runGroupAct = (act: 'checkin' | 'pause' | 'resume') => {
    const uids = groupUids
    if (!uids.length) {
      toast.warn('这个分组里没有账号')
      return
    }
    void run('group:' + act, async () => {
      let okCount = 0
      let firstErr = ''
      for (const uid of uids) {
        if (batchSkip(act, rosterMap.get(uid))) continue
        try {
          await call(uid, act)
          okCount++
        } catch (e) {
          if (!firstErr) firstErr = errorText(e)
        }
      }
      if (okCount) toast.ok('组内' + (act === 'checkin' ? '签到' : act === 'pause' ? '暂停' : '恢复') + '：' + okCount + ' 个')
      if (firstErr) toast.warn(firstErr)
      q.reload()
      ov.reload()
    })
  }

  /** 全量动作：checkin/travel/activity/keepalive 异步执行，balance_all 同步返回。 */
  const doAllAct = (act: AllActKind) => {
    void run('all:' + act, async () => {
      try {
        await api.post<{ ok?: boolean; started?: boolean }>('/' + act)
        toast.ok(act === 'balance_all' ? '余额已从上游刷新' : '已开始，后台执行中，稍后自动刷新')
        q.reload()
        ov.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const applyGroup = (uids: string[], name: string) => {
    if (!uids.length) {
      toast.warn('先勾选账号')
      return
    }
    void run('group', async () => {
      let okCount = 0
      let firstErr = ''
      for (const uid of uids) {
        try {
          await call(uid, 'group', { group: name })
          okCount++
        } catch (e) {
          if (!firstErr) firstErr = errorText(e)
        }
      }
      if (okCount) toast.ok('分组已更新 ' + okCount + ' 个' + (firstErr ? '，失败 ' + (uids.length - okCount) + ' 个' : ''))
      if (firstErr) toast.err(firstErr)
      setBulkGroup('')
      q.reload()
      roster.reload()
    })
  }

  const runConfirm = () => {
    const c = confirm
    if (!c) return
    if (c.kind === 'all') {
      setConfirm(null)
      doAllAct(c.act)
      return
    }
    if (c.kind === 'export') {
      setConfirm(null)
      const a = document.createElement('a')
      a.href = c.href
      a.download = c.name
      document.body.appendChild(a)
      a.click()
      a.remove()
      toast.ok('正在下载 ' + c.count + ' 个账号的 JSON（含明文 token，注意保管）')
      return
    }
    const uids = c.uids
    const groupName = c.group ?? ''
    void run('confirm', async () => {
      let okCount = 0
      let firstErr = ''
      for (const uid of uids) {
        try {
          if (c.kind === 'remove') await call(uid, 'remove')
          else if (c.kind === 'disable') await call(uid, 'disable', { reason: '面板手动禁用' })
          else if (c.kind === 'revive') await call(uid, 'revive')
          else await call(uid, 'group', { group: groupName })
          okCount++
        } catch (e) {
          if (!firstErr) firstErr = errorText(e)
        }
      }
      if (okCount) {
        const verb =
          c.kind === 'remove' ? '已删除 ' : c.kind === 'disable' ? '已禁用 ' : c.kind === 'revive' ? '已解冻 ' : '分组已更新 '
        toast.ok(verb + okCount + ' 个' + (uids.length > okCount ? '，失败 ' + (uids.length - okCount) + ' 个' : ''))
      }
      if (firstErr) toast.err(firstErr)
      if (uids.length === 1 && detailUid === uids[0] && c.kind !== 'group') setDetailUid(null)
      setConfirm(null)
      if (c.kind !== 'group') sel.clear()
      q.reload()
      ov.reload()
      roster.reload()
    })
  }

  // ---------- 添加账号：授权链接 ----------
  const stopLoginPoll = () => {
    if (loginTimer.current !== null) {
      window.clearInterval(loginTimer.current)
      loginTimer.current = null
    }
  }

  const pollLogin = async () => {
    const state = loginStateRef.current
    if (!state) return
    try {
      const r = await api.get<LoginPoll>('/login/poll', { state })
      loginFails.current = 0
      if (r.fatal) {
        stopLoginPoll()
        loginStateRef.current = ''
        setLoginErr(r.message || '登录被上游拒绝')
        return
      }
      if (r.done) {
        stopLoginPoll()
        loginStateRef.current = ''
        const who = (r.nickname || r.uid || '账号') + (r.realm === 'global' ? '（国际版）' : '')
        const money =
          typeof r.credits === 'number' && r.credits >= 0
            ? ' · 积分 ' + credits(r.credits) + '/' + credits(r.credits_total)
            : ''
        setLogin(null)
        setLoginDone(
          '已添加 ' + who + money + (r.checkin_message ? ' · ' + r.checkin_message : '') + '，账号已载入池中',
        )
        if (r.balance_error) toast.warn('账号已添加，但余额校验失败：' + r.balance_error)
        else toast.ok('已添加 ' + who)
        q.reload()
        ov.reload()
        return
      }
      if (Date.now() - loginStartAt.current > 90000) {
        setLoginNote('仍在等待浏览器完成登录。若浏览器已显示「登录失败」，请关闭本窗口重新获取授权链接（链接 15 分钟内有效）。')
      } else if (r.message) {
        setLoginNote(r.message)
      }
    } catch (e) {
      const status = e instanceof ApiError ? e.status : 0
      if (status === 404 || status === 410) {
        stopLoginPoll()
        loginStateRef.current = ''
        setLogin(null)
        setLoginErr('登录会话已过期，请关闭后重新添加')
        return
      }
      loginFails.current += 1
      if (loginFails.current >= 20) {
        stopLoginPoll()
        loginStateRef.current = ''
        setLoginErr('连续 20 次查询失败，已停止轮询：' + errorText(e) + '（授权可能已完成，可关闭后重新添加）')
        return
      }
      setLoginNote('网络中断，自动重试中（' + loginFails.current + '/20）：' + errorText(e))
    }
  }

  const startLogin = () => {
    void run('login:start', async () => {
      setLoginErr('')
      setLoginNote('')
      setLoginDone('')
      setLogin(null)
      try {
        const r = await api.post<LoginStart>('/login/start', { site })
        if (!r.url || !r.state) {
          setLoginErr('上游没有返回授权链接，请稍后重试')
          return
        }
        setLogin(r)
        loginStateRef.current = r.state
        loginFails.current = 0
        loginStartAt.current = Date.now()
        stopLoginPoll()
        loginTimer.current = window.setInterval(() => {
          void pollLogin()
        }, 3000)
      } catch (e) {
        setLoginErr(errorText(e))
      }
    })
  }

  const addApiKey = () => {
    const token = keyToken.trim()
    if (!token) {
      toast.warn('请填写 API Key')
      return
    }
    void run('apikey', async () => {
      try {
        const r = await api.post<{ uid?: string; nickname?: string }>('/accounts/add_apikey', {
          access_token: token,
          nickname: keyNick.trim(),
          uid: keyUid.trim(),
        })
        toast.ok('已添加 ' + (r.nickname || r.uid || 'API Key 账号') + '（API Key 账号），账号已载入池中')
        setKeyToken('')
        setKeyNick('')
        setKeyUid('')
        q.reload()
        ov.reload()
      } catch (e) {
        toast.err(errorText(e))
      }
    })
  }

  const openAdd = (tab: AddTab) => {
    setAddTab(tab)
    setAddOpen(true)
  }

  const closeAdd = () => {
    stopLoginPoll()
    loginStateRef.current = ''
    setAddOpen(false)
    setLogin(null)
    setLoginErr('')
    setLoginNote('')
    setLoginDone('')
    setPaste('')
    setImportFile(null)
    setFileKey(fileKey + 1)
    setPreview(null)
    setJob(null)
    setImportErr('')
    setKeyToken('')
    setKeyNick('')
    setKeyUid('')
  }

  // ---------- 备注 / 分组 ----------
  const openRemarks = (only?: Account) => {
    const nextRm: Record<string, string> = {}
    const nextGp: Record<string, string> = {}
    for (const it of roster.data?.accounts ?? []) {
      nextRm[it.uid] = remarks[remarkKey(it.uid)] ?? ''
      nextGp[it.uid] = it.group ?? ''
    }
    if (only) {
      nextRm[only.uid] = remarks[remarkKey(only.uid)] ?? ''
      nextGp[only.uid] = only.group ?? ''
      setRmKw(acctName(only))
    } else {
      setRmKw('')
    }
    setRmText(nextRm)
    setGpText(nextGp)
    setRemarkOpen(true)
  }

  const saveRemarks = () => {
    const next: Record<string, string> = { ...remarks }
    for (const [uid, v] of Object.entries(rmText)) {
      const key = remarkKey(uid)
      const t = v.trim().slice(0, 24)
      if (t) next[key] = t
      else delete next[key]
    }
    const wrote = writeRemarks(next)
    setRemarks(next)
    if (!wrote) toast.warn('备注未能写入浏览器存储（隐私模式或空间已满），刷新后会丢失')

    const changed: { uid: string; group: string }[] = []
    for (const [uid, v] of Object.entries(gpText)) {
      const orig = rosterMap.get(uid)?.group ?? ''
      const t = v.trim().slice(0, 24)
      if (t !== orig) changed.push({ uid, group: t })
    }
    if (!changed.length) {
      toast.ok('已保存备注')
      setRemarkOpen(false)
      q.reload()
      return
    }
    void run('remarks', async () => {
      let okCount = 0
      let firstErr = ''
      for (const it of changed) {
        try {
          await call(it.uid, 'group', { group: it.group })
          okCount++
        } catch (e) {
          if (!firstErr) firstErr = errorText(e)
        }
      }
      if (okCount) toast.ok('备注已保存，分组更新 ' + okCount + ' 个')
      if (firstErr) toast.err(firstErr)
      setRemarkOpen(false)
      q.reload()
      roster.reload()
    })
  }

  const clearRemarks = () => {
    writeRemarks({})
    setRemarks({})
    setRmText({})
    toast.ok('已清空本机备注')
  }

  const openDetail = (s: Account) => {
    setProbe(null)
    setRawOpen(false)
    setDetailUid(s.uid)
    const b = bindings[s.uid]
    setProxyMode(b?.mode === 'fixed' ? 'fixed' : b?.mode === 'direct' ? 'direct' : 'random')
    setProxyId(b?.proxy_id ?? '')
  }

  const buildForm = (): FormData => {
    if (!importFile && !paste.trim()) throw new Error('请粘贴 JSON 或选择一个文件')
    const fd = new FormData()
    if (importFile) {
      fd.append('file', importFile)
    } else {
      fd.append('file', new File([paste], 'pasted.json', { type: 'application/json' }))
    }
    return fd
  }

  const doPreview = () => {
    void run('preview', async () => {
      setImportErr('')
      setPreview(null)
      setJob(null)
      try {
        const fd = buildForm()
        fd.append('dryRun', '1')
        setPreview(await api.form<ImportResult>('/import/cockpit', fd))
      } catch (e) {
        setImportErr(errorText(e))
      }
    })
  }

  const doImport = () => {
    void run('import', async () => {
      setImportErr('')
      setPreview(null)
      setJob(null)
      try {
        const fd = buildForm()
        fd.append('async', '1')
        const r = await api.form<ImportResult>('/import/cockpit', fd)
        if (!r.job_id) {
          toast.ok('导入完成：新增 ' + num(r.creates) + '，覆盖 ' + num(r.overwrites) + '，跳过 ' + num(r.skipped))
          q.reload()
          return
        }
        setJob({ state: 'running', total: r.total, processed: 0 })
        let last: ImportProgress | null = null
        for (let i = 0; i < 400; i++) {
          await new Promise((resolve) => setTimeout(resolve, 900))
          last = await api.get<ImportProgress>('/import/cockpit/jobs/' + encodeURIComponent(r.job_id))
          setJob(last)
          if (last.state !== 'running') break
        }
        if (last && last.state === 'done') {
          toast.ok('导入完成：新增 ' + num(last.creates) + '，覆盖 ' + num(last.overwrites) + '，跳过 ' + num(last.skipped))
          q.reload()
          ov.reload()
        } else if (last && last.state === 'failed') {
          toast.err(last.error || '导入失败')
        } else {
          toast.warn('导入仍在后台进行，稍后刷新列表查看')
        }
      } catch (e) {
        setImportErr(errorText(e))
      }
    })
  }

  const counts: { all?: number; cn?: number; global?: number } = data?.counts ?? {}

  /** 与旧面板 accExportPath 一致：有勾选就按 uid 导出，否则按当前域/分组导出。 */
  const askExport = (picked: boolean) => {
    const href = api.downloadUrl(
      '/accounts/export',
      picked
        ? { uids: sel.list.join(',') }
        : { realm: realm === 'all' ? undefined : realm, group: group || undefined },
    )
    const n = picked ? sel.size : data?.total ?? 0
    setConfirm({
      kind: 'export',
      href,
      name: 'workbuddy-accounts-' + (picked ? 'picked' + sel.size : 'all') + '-' + stamp() + '.json',
      count: n,
    })
  }

  const ovv = (v?: number): string => (ov.data ? num(v ?? 0) : '—')

  const head = (
    <div className="pagehead">
      <div>
        <h2>账号池</h2>
        <p className="lead">
          {data
            ? '共 ' + num(data.total) + ' 个账号 · 第 ' + data.page + '/' + Math.max(data.pages, 1) + ' 页 · 每 5 秒自动刷新'
            : '账号目录、健康状态与额度'}
        </p>
      </div>
      <div className="acts">
        <button className="btn sm primary" onClick={() => openAdd('login')}>
          添加账号
        </button>
        <button className="btn sm" onClick={() => askExport(false)} title="导出当前域 / 分组的全部账号">
          导出全部
        </button>
        <button
          className="btn sm"
          disabled={!sel.size}
          onClick={() => askExport(true)}
          title={sel.size ? '导出已勾选的 ' + sel.size + ' 个账号' : '先勾选账号'}
        >
          导出所选
        </button>
        <button className="btn sm ghost" disabled={!!busy} onClick={() => doAllAct('balance_all')}>
          {busy === 'all:balance_all' ? '刷新中…' : '刷新余额'}
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
            <Skeleton rows={8} />
          </Box>
        )}
      </div>
    )
  }

  const detail = detailUid ? (data.accounts ?? []).find((s) => s.uid === detailUid) : undefined

  const columns: Column<Account>[] = [
    {
      key: 'account',
      title: '账号',
      sortValue: (s) => (remarks[remarkKey(s.uid)] || acctName(s)).toLowerCase(),
      render: (s) => {
        const rm = remarks[remarkKey(s.uid)]
        return (
          <div className="entity">
            <span className="avatar-sq">{acctName(s).slice(0, 1).toUpperCase()}</span>
            <div className="meta">
              <div className="n truncate">{rm || acctName(s)}</div>
              <div className="s tiny mono truncate">
                {rm ? acctName(s) + ' · ' : ''}
                {s.uid.slice(0, 8)}
                {s.group ? ' · ' + s.group : ''}
              </div>
            </div>
          </div>
        )
      },
    },
    {
      key: 'status',
      title: '状态',
      width: '116px',
      sortValue: (s) => {
        const st = statusOf(s).text
        return st === '健康' ? 0 : st === '冷却' ? 1 : st === '暂停' ? 2 : 3
      },
      render: (s) => {
        const st = statusOf(s)
        return (
          <span title={s.reason || s.disabled_reason || ''}>
            <Badge tone={st.tone}>{st.text}</Badge>
            {s.cooling && s.cool_remaining_sec ? (
              <span className="tiny muted"> {duration(s.cool_remaining_sec)}</span>
            ) : null}
          </span>
        )
      },
    },
    {
      key: 'credits',
      title: '积分/额度',
      align: 'right',
      mono: true,
      width: '130px',
      sortValue: (s) => s.credits ?? 0,
      render: (s) => (
        <span>
          {credits(s.credits)}
          <span className="tiny muted"> / {credits(s.credits_total)}</span>
        </span>
      ),
    },
    {
      key: 'expiry',
      title: '到期',
      width: '132px',
      sortValue: (s) => msOf(s.credits_earliest_expiry),
      render: (s) => {
        // 零值时间（0001-01-01T00:00:00Z）与空串同义：都没有待到期批次。
        // 只判空串的话零值会被算成「很久以前」，列表显示「已过期」，与真实状态相反。
        if (!s.credits_earliest_expiry || isZeroTime(s.credits_earliest_expiry))
          return <span className="muted">—</span>
        const days = Math.floor((msOf(s.credits_earliest_expiry) - Date.now()) / 86400000)
        return (
          <span>
            <span className={days <= 7 ? 'warn-text' : ''}>{cstDate(s.credits_earliest_expiry)}</span>
            {days <= 7 ? (
              <span className="tiny muted"> {days <= 0 ? '已过期' : days + ' 天后'}</span>
            ) : null}
          </span>
        )
      },
    },
    {
      key: 'last',
      title: '最后刷新',
      width: '108px',
      hideMobile: true,
      sortValue: (s) => msOf(s.last_success),
      render: (s) =>
        s.last_success ? (
          <span title={cstDate(s.last_success) + ' ' + clock(s.last_success)}>{ago(s.last_success)}</span>
        ) : (
          <span className="muted">—</span>
        ),
    },
    {
      key: 'proxy',
      title: '代理',
      width: '116px',
      hideMobile: true,
      render: (s) => <span className="small">{proxyText(s.uid)}</span>,
    },
    {
      key: 'act',
      title: '操作',
      align: 'right',
      className: 'c-acts',
      render: (s) => {
        // 详细弹层里的动作全部提到行内：日常操作（签到/余额/暂停恢复）直接摆按钮，
        // 低频且带风险的动作（探测/禁用/解冻/删除/备注/代理）收进「更多」菜单。
        const noCheckin = !!s.enterprise || !!s.api_key
        const done = !!s.checkin_done
        const items: MenuItem[] = [
          { label: '探测封禁', onSelect: () => doProbeRow(s.uid), disabled: !!busy },
          { label: '备注 / 分组', onSelect: () => openRemarks(s) },
          { label: '代理绑定', onSelect: () => openDetail(s), sep: true },
        ]
        if (s.disabled) {
          items.push({
            label: '解冻',
            onSelect: () => doAct(s.uid, 'revive', '已解冻，恢复可用'),
            disabled: !!busy,
            sep: true,
          })
        } else {
          items.push({
            label: '禁用',
            onSelect: () => setConfirm({ kind: 'disable', uids: [s.uid], name: acctName(s) }),
            disabled: !!busy,
            sep: true,
          })
        }
        items.push({
          label: '删除',
          danger: true,
          onSelect: () => setConfirm({ kind: 'remove', uids: [s.uid], name: acctName(s) }),
          disabled: !!busy,
        })
        return (
          <span className="row gap1 act-row" style={{ justifyContent: 'flex-end' }}>
            {noCheckin ? (
              <span className="tiny muted" title="企业版 / API Key 账号没有签到体系">
                不可签
              </span>
            ) : done ? (
              <button className="btn sm ok" disabled title="今天已经签到过了（UTC+8 零点重置）">
                已签
              </button>
            ) : (
              <button
                className="btn sm ghost"
                disabled={!!busy}
                title="立即签到"
                onClick={(e) => {
                  e.stopPropagation()
                  doCheckin(s.uid)
                }}
              >
                签到
              </button>
            )}
            <button
              className="btn sm ghost"
              disabled={!!busy}
              title="刷新积分余额"
              onClick={(e) => {
                e.stopPropagation()
                doBalance(s.uid)
              }}
            >
              余额
            </button>
            {s.paused ? (
              <button
                className="btn sm ghost"
                disabled={!!busy}
                title="恢复参与选号"
                onClick={(e) => {
                  e.stopPropagation()
                  doAct(s.uid, 'resume', '已恢复参与选号')
                }}
              >
                恢复
              </button>
            ) : (
              <button
                className="btn sm ghost"
                disabled={!!busy || !!s.disabled}
                title="暂停参与选号"
                onClick={(e) => {
                  e.stopPropagation()
                  doAct(s.uid, 'pause', '已暂停，选号会跳过它')
                }}
              >
                暂停
              </button>
            )}
            <span onClick={(e) => e.stopPropagation()}>
              <Menu items={items} ariaLabel={'更多操作：' + acctName(s)} title="更多操作" />
            </span>
            <button
              className="btn sm"
              onClick={(e) => {
                e.stopPropagation()
                openDetail(s)
              }}
            >
              详情
            </button>
          </span>
        )
      },
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
        <Stat label="账号总数" value={num(data.total)} sub={data.source === 'memory' ? '内存池，重启会丢' : '来自账号目录'} />
        <Stat label="健康" value={ovv(ov.data?.healthy)} tone="ok" sub="可参与选号" />
        <Stat label="冷却" value={ovv(ov.data?.cooling)} tone="warn" sub="临时限流" />
        <Stat label="禁用" value={ovv(ov.data?.disabled)} tone="bad" sub="需解冻" />
        <Stat label="暂停" value={ovv(ov.data?.paused)} tone="accent" sub="保留但不选号" />
        <Stat label="积分合计" value={ov.data ? credits(ov.data.credits_sum) : '—'} sub="池内可用" />
        <Stat label="粘性会话" value={ovv(ov.data?.sticky_sessions)} sub="绑定了账号的会话" />
      </StatsGrid>

      <Box
        title="账号列表"
        sub={
          statusFilter !== 'all' || kw
            ? '本页 ' + (data.accounts ? data.accounts.length : 0) + ' 条中匹配 ' + rows.length + ' 条'
            : '点行打开详情，可刷新余额、探测、暂停、绑定代理'
        }
      >
        <div className="toolbar">
          {ALL_ACTS.map((a) => (
            <button
              key={a.act}
              className="btn sm"
              disabled={!!busy}
              title={ALL_ACT_HINT[a.act]}
              onClick={() => setConfirm({ kind: 'all', act: a.act, name: a.label })}
            >
              {a.label}
            </button>
          ))}
          <span className="sep" />
          <button className="btn sm ghost" onClick={() => openRemarks()}>
            备注 / 分组
          </button>
          {group ? (
            <>
              <button className="btn sm" disabled={!!busy} onClick={() => runGroupAct('checkin')}>
                组内签到
              </button>
              <button className="btn sm" disabled={!!busy} onClick={() => runGroupAct('pause')}>
                组内暂停
              </button>
              <button className="btn sm" disabled={!!busy} onClick={() => runGroupAct('resume')}>
                组内恢复
              </button>
            </>
          ) : null}
        </div>

        <div className="filters">
          <Select
            ariaLabel="域"
            value={realm}
            onChange={(v) => {
              setRealm(v as 'all' | 'cn' | 'global')
              setPage(1)
            }}
            options={[
              { value: 'all', label: '全部域 · ' + num(counts.all) },
              { value: 'cn', label: 'CN 域 · ' + num(counts.cn) },
              { value: 'global', label: 'Global 域 · ' + num(counts.global) },
            ]}
          />
          <Select
            ariaLabel="分组"
            value={group}
            onChange={(v) => {
              setGroup(v)
              setPage(1)
            }}
            options={[
              { value: '', label: '全部分组' },
              ...(data.groups ?? []).map((g) => ({ value: g.name, label: g.name + ' · ' + g.count })),
            ]}
          />
          <Select
            ariaLabel="状态"
            value={statusFilter}
            onChange={(v) => {
              setStatusFilter(v as Filter)
              setPage(1)
            }}
            options={FILTERS.map((f) => ({ value: f.value, label: f.label }))}
          />
          <input
            className="input"
            placeholder="搜索昵称 / UID"
            aria-label="搜索账号"
            value={keyword}
            onChange={(e) => {
              setKeyword(e.target.value)
              setPage(1)
            }}
          />
          <Select
            className="xs"
            ariaLabel="每页条数"
            value={String(size)}
            onChange={(v) => {
              setSize(Number(v))
              setPage(1)
            }}
            menuMaxHeight={220}
            options={[10, 20, 50, 100].map((n) => ({ value: String(n), label: n + ' 条/页' }))}
          />
        </div>

        {sel.size > 0 ? (
          <div className="actionbar">
            <span className="small muted">已选 {sel.size} 个</span>
            <button className="btn sm" disabled={!!busy} onClick={() => runBatch('checkin', '签到')}>
              签到
            </button>
            <button className="btn sm" disabled={!!busy} onClick={() => runBatch('balance', '刷新余额')}>
              刷新余额
            </button>
            <button className="btn sm" disabled={!!busy} onClick={() => runBatch('pause', '暂停')}>
              暂停
            </button>
            <button className="btn sm" disabled={!!busy} onClick={() => runBatch('resume', '恢复选号')}>
              恢复选号
            </button>
            <button className="btn sm ok" disabled={!!busy} onClick={() => runBatch('revive', '解冻')}>
              解冻
            </button>
            <button
              className="btn sm danger"
              disabled={!!busy}
              onClick={() => setConfirm({ kind: 'disable', uids: sel.list, name: sel.size + ' 个账号' })}
            >
              禁用
            </button>
            <button
              className="btn sm danger"
              disabled={!!busy}
              onClick={() => setConfirm({ kind: 'remove', uids: sel.list, name: sel.size + ' 个账号' })}
            >
              删除
            </button>
            <input
              className="input xs"
              placeholder="分组名（留空=清除）"
              aria-label="批量分组"
              value={bulkGroup}
              onChange={(e) => setBulkGroup(e.target.value)}
            />
            <button
              className="btn sm"
              disabled={!!busy}
              onClick={() => applyGroup(sel.list, bulkGroup.trim())}
              title="给已勾选的账号设置分组；留空即清除分组"
            >
              应用分组
            </button>
            <button className="btn sm ghost ml-auto" disabled={!!busy} onClick={sel.clear}>
              清空勾选
            </button>
          </div>
        ) : null}

        <DataTable<Account>
          columns={columns}
          rows={rows}
          rowKey={(s) => s.uid}
          loading={q.loading}
          selectable
          selected={sel.set}
          onToggle={(key, next) => {
            if (next !== sel.has(key)) sel.toggle(key)
          }}
          onToggleAll={(next, keys) => {
            if (next) sel.addMany(keys)
            else keys.forEach((k) => { if (sel.has(k)) sel.toggle(k) })
          }}
          onRowClick={openDetail}
          empty={data.total === 0 ? '账号池是空的' : '没有匹配的账号'}
          emptyHint={
            data.total === 0
              ? '点右上角「导入」，粘贴 cockpit 账号 JSON 或选择文件。'
              : '换一个状态，或清空关键字；状态与关键字只在本页内过滤。'
          }
          initialSort={{ key: 'last', dir: 'desc' }}
          caption={
            <span className="tiny muted">
              状态与关键字只在当前页内过滤（服务端只支持域/分组/分页）；导出会带上已勾选的账号。
            </span>
          }
        />

        {data.total > 0 ? (
          <Pager
            page={data.page}
            pageSize={data.size}
            total={data.total}
            onPage={setPage}
            onPageSize={(n) => {
              setSize(n)
              setPage(1)
            }}
          />
        ) : null}
      </Box>

      <Modal
        open={!!detail}
        title={detail ? '账号详情 · ' + acctName(detail) : '账号详情'}
        onClose={() => setDetailUid(null)}
        wide
        footer={
          <button className="btn ghost" onClick={() => setDetailUid(null)}>
            关闭
          </button>
        }
      >
        {detail ? (
          <div className="stack">
            <div className="row v-center gap2 row-wrap">
              <div className="entity">
                <span className="avatar-sq">{acctName(detail).slice(0, 1).toUpperCase()}</span>
                <div className="meta">
                  <div className="n">{acctName(detail)}</div>
                  <div className="s tiny mono">{detail.uid}</div>
                </div>
              </div>
              <CopyText value={detail.uid} label="复制 UID" />
            </div>

            <KV
              items={[
                { k: '状态', v: statusOf(detail).text },
                { k: '备注', v: remarks[remarkKey(detail.uid)] || '—' },
                { k: '域 / 分组', v: (detail.realm || 'cn') + (detail.group ? ' · ' + detail.group : ' · 未分组') },
                { k: '积分 / 总额度', v: credits(detail.credits) + ' / ' + credits(detail.credits_total), mono: true },
                {
                  k: '最近到期',
                  v:
                    detail.credits_earliest_expiry && !isZeroTime(detail.credits_earliest_expiry)
                      ? cstDate(detail.credits_earliest_expiry) + ' · 剩 ' + credits(detail.credits_earliest_remaining)
                      : '—',
                },
                {
                  k: '最后成功',
                  v: detail.last_success ? shortTime(detail.last_success) + ' · ' + ago(detail.last_success) : '—',
                },
                {
                  k: '最后错误',
                  // last_err 是 RFC3339 字符串；零值（0001-01-01）在 isZeroTime 里判掉，
                  // 否则会原样显示成「0001-01-01T00:00:00Z」。
                  v:
                    detail.last_err && !isZeroTime(detail.last_err)
                      ? shortTime(detail.last_err) + ' · ' + ago(detail.last_err)
                      : '—',
                },
                { k: '成功 / 失败', v: num(detail.success_count) + ' / ' + num(detail.err_total), mono: true },
                { k: '代理', v: proxyText(detail.uid) },
                ...(detail.disabled ? [{ k: '禁用原因', v: detail.disabled_reason || '未记录' }] : []),
                ...(detail.cooling
                  ? [{
                      k: '冷却原因',
                      v:
                        (detail.reason || detail.cool_kind || '限流') +
                        (detail.cool_remaining_sec ? ' · 剩 ' + duration(detail.cool_remaining_sec) : ''),
                    }]
                  : []),
                ...(detail.enterprise ? [{ k: '账号类型', v: '企业版' }] : []),
                ...(detail.api_key ? [{ k: '账号类型', v: 'API Key（不能签到）' }] : []),
              ]}
            />

            <div className="grid2">
              <div className="stack">
                <p className="small muted">账号动作</p>
                <div className="row row-wrap gap2">
                  <button className="btn sm primary" disabled={!!busy} onClick={() => doBalance(detail.uid)}>
                    刷新余额
                  </button>
                  {detail.enterprise || detail.api_key ? (
                    <button className="btn sm" disabled title="企业版 / API Key 账号没有签到体系">
                      签到（不适用）
                    </button>
                  ) : (
                    <button className="btn sm" disabled={!!busy} onClick={() => doCheckin(detail.uid)}>
                      签到
                    </button>
                  )}
                  <button className="btn sm" disabled={!!busy} onClick={() => doProbe(detail.uid)}>
                    探测封禁
                  </button>
                  {detail.paused ? (
                    <button className="btn sm" disabled={!!busy} onClick={() => doAct(detail.uid, 'resume', '已恢复参与选号')}>
                      恢复选号
                    </button>
                  ) : (
                    <button
                      className="btn sm"
                      disabled={!!busy || !!detail.disabled}
                      onClick={() => doAct(detail.uid, 'pause', '已暂停，选号会跳过它')}
                    >
                      暂停选号
                    </button>
                  )}
                  {detail.disabled ? (
                    <button className="btn sm ok" disabled={!!busy} onClick={() => doAct(detail.uid, 'revive', '已解冻，恢复可用')}>
                      解冻
                    </button>
                  ) : (
                    <button
                      className="btn sm danger"
                      disabled={!!busy}
                      onClick={() => setConfirm({ kind: 'disable', uids: [detail.uid], name: acctName(detail) })}
                    >
                      禁用
                    </button>
                  )}
                  <button
                    className="btn sm danger"
                    disabled={!!busy}
                    onClick={() => setConfirm({ kind: 'remove', uids: [detail.uid], name: acctName(detail) })}
                  >
                    删除
                  </button>
                  <button className="btn sm ghost" onClick={() => openRemarks(detail)}>
                    备注 / 分组
                  </button>
                  <button className="btn sm ghost" onClick={() => setRawOpen(!rawOpen)}>
                    {rawOpen ? '隐藏原始 JSON' : '原始 JSON'}
                  </button>
                </div>
              </div>

              <div className="stack">
                <p className="small muted">代理绑定</p>
                {px.data ? (
                  <div className="stack">
                    <Segmented value={proxyMode} options={PROXY_OPTIONS} onChange={setProxyMode} ariaLabel="代理模式" />
                    {proxyMode === 'fixed' ? (
                      <Select
                        ariaLabel="选择代理节点"
                        value={proxyId}
                        onChange={setProxyId}
                        placeholder="选择节点…"
                        options={proxies.map((p) => ({
                          value: p.id,
                          label: p.name + (p.enabled === false ? '（停用）' : ''),
                        }))}
                      />
                    ) : null}
                    <button
                      className="btn sm"
                      disabled={!!busy || (proxyMode === 'fixed' && !proxyId)}
                      onClick={() => applyProxy(detail.uid)}
                    >
                      保存代理
                    </button>
                  </div>
                ) : (
                  <p className="small muted">
                    代理池不可用{px.error ? '：' + errorText(px.error) : ''}
                  </p>
                )}
              </div>
            </div>

            {probe ? (
              <div className={'notice ' + (VERDICT[probe.verdict || '']?.tone ?? 'warn')}>
                <span>
                  探测结果：{VERDICT[probe.verdict || '']?.text ?? '未知结果（' + (probe.verdict || '') + '）'}
                  {probe.detail ? ' · ' + probe.detail : ''}
                </span>
              </div>
            ) : null}

            {rawOpen ? <Pre maxHeight={280}>{JSON.stringify(detail, null, 2)}</Pre> : null}
          </div>
        ) : null}
      </Modal>

      <Modal
        open={addOpen}
        title="添加账号"
        onClose={closeAdd}
        wide
        footer={
          <div className="row gap2">
            <button className="btn ghost" onClick={closeAdd}>
              关闭
            </button>
            {addTab === 'login' ? (
              <button className="btn primary" disabled={!!busy || !!login} onClick={startLogin}>
                {login ? '等待授权中…' : '获取授权链接'}
              </button>
            ) : addTab === 'import' ? (
              <>
                <button className="btn" disabled={!!busy} onClick={doPreview}>
                  预览
                </button>
                <button className="btn primary" disabled={!!busy} onClick={doImport}>
                  开始导入
                </button>
              </>
            ) : (
              <button className="btn primary" disabled={!!busy} onClick={addApiKey}>
                添加 API Key
              </button>
            )}
          </div>
        }
      >
        <Tabs value={addTab} options={ADD_TABS} onChange={setAddTab} />
        {addTab === 'login' ? (
          <div className="stack">
            <p className="small muted">
              点「获取授权链接」，在下面的链接里完成浏览器登录，这里会自动检测并写入凭据。国际版登录后网关会自动完成注册地区、激活与试用额度领取。
            </p>
            {login ? null : (
              <Field label="站点" hint="国内版走 codebuddy.cn；国际版走 workbuddy.ai 桌面端口径。">
                <Segmented value={site} options={SITE_OPTIONS} onChange={setSite} ariaLabel="站点" />
              </Field>
            )}
            {busy === 'login:start' ? <p className="small muted">正在获取授权链接…</p> : null}
            {loginErr ? (
              <div className="notice bad">
                <span>{loginErr}</span>
              </div>
            ) : null}
            {login ? (
              <div className="stack">
                <p className="small muted">等待授权完成，自动检测中；链接 15 分钟内有效。</p>
                <div className="row v-center gap2 row-wrap">
                  <input className="input grow mono" readOnly value={login.url ?? ''} aria-label="授权链接" />
                  <CopyText value={login.url ?? ''} label="复制链接" />
                  <a className="btn sm primary" href={login.url} target="_blank" rel="noreferrer">
                    在浏览器打开
                  </a>
                </div>
                {loginNote ? <p className="small muted">{loginNote}</p> : null}
                <div className="row gap2">
                  <button
                    className="btn sm ghost"
                    onClick={() => {
                      stopLoginPoll()
                      loginStateRef.current = ''
                      setLogin(null)
                      setLoginErr('')
                      setLoginNote('')
                    }}
                  >
                    换一个授权链接
                  </button>
                </div>
              </div>
            ) : null}
            {loginDone ? (
              <div className="notice ok">
                <span>{loginDone}</span>
              </div>
            ) : null}
          </div>
        ) : addTab === 'import' ? (
          <div className="stack">
          <Field label="粘贴 JSON" hint="支持数组、单个对象、{account} 嵌套、{accounts|data|list|items} 信封或 {uid: {...}} 字典。">
            <textarea
              className="textarea"
              rows={8}
              placeholder='[{"uid":"...","account":{...},"auth":{...}}]'
              value={paste}
              onChange={(e) => setPaste(e.target.value)}
              disabled={!!importFile}
            />
          </Field>
          <Field label="或选择文件（.json）" hint="选了文件就忽略上面的粘贴内容；多个文件请分次导入。">
            <div className="row v-center gap2 row-wrap">
              <input
                key={fileKey}
                className="input grow"
                type="file"
                accept=".json,application/json"
                onChange={(e) => setImportFile(e.target.files && e.target.files[0] ? e.target.files[0] : null)}
              />
              {importFile ? (
                <button
                  className="btn sm ghost"
                  onClick={() => {
                    setImportFile(null)
                    setFileKey(fileKey + 1)
                  }}
                >
                  清除文件
                </button>
              ) : null}
            </div>
          </Field>

          {importErr ? (
            <div className="notice bad">
              <span>{importErr}</span>
            </div>
          ) : null}

          {preview ? (
            <div className="stack">
              <div className={'notice ' + (preview.errors && preview.errors.length ? 'warn' : 'ok')}>
                <span>
                  预览：共 {num(preview.total)} 条，将导入 {num(preview.imported)}，跳过 {num(preview.skipped)}；新增{' '}
                  {num(preview.creates)}，覆盖 {num(preview.overwrites)}
                </span>
              </div>
              {preview.errors && preview.errors.length ? (
                <Pre maxHeight={140}>{preview.errors.join('\n')}</Pre>
              ) : null}
            </div>
          ) : null}

          {job ? (
            <div className="stack">
              <div className="row v-center gap2">
                <span className="small muted grow">
                  {job.state === 'running'
                    ? '导入中 ' + num(job.processed) + '/' + num(job.total)
                    : job.state === 'done'
                      ? '导入完成'
                      : '导入失败'}
                </span>
                <span className="small mono">
                  {job.total ? Math.round(((job.processed ?? 0) / job.total) * 100) : 0}%
                </span>
              </div>
              <Bar
                pct={job.total ? Math.min(100, Math.round(((job.processed ?? 0) / job.total) * 100)) : 0}
                tone={job.state === 'failed' ? 'bad' : job.state === 'done' ? 'ok' : 'accent'}
              />
              {job.current ? <p className="tiny muted truncate">当前：{job.current}</p> : null}
              {job.error ? (
                <div className="notice bad">
                  <span>{job.error}</span>
                </div>
              ) : null}
              {job.state === 'done' ? (
                <KV
                  items={[
                    { k: '新增', v: num(job.creates), mono: true },
                    { k: '覆盖', v: num(job.overwrites), mono: true },
                    { k: '跳过', v: num(job.skipped), mono: true },
                    { k: '失败', v: num(job.errors ? job.errors.length : 0), mono: true },
                  ]}
                />
              ) : null}
              {job.errors && job.errors.length ? <Pre maxHeight={140}>{job.errors.join('\n')}</Pre> : null}
            </div>
          ) : null}
          </div>
        ) : (
          <div className="stack">
            <p className="small muted">
              粘贴 CodeBuddy 平台签发的静态 API Key（ck_/pt_ 前缀）。添加后直接可用、无需浏览器登录；该账号没有签到 / 成长任务体系，额度走企业口径。
            </p>
            <Field label="API Key" hint="必填，ck_ 或 pt_ 开头。">
              <input
                className="input mono"
                placeholder="ck_..."
                aria-label="API Key"
                value={keyToken}
                onChange={(e) => setKeyToken(e.target.value)}
              />
            </Field>
            <div className="formgrid">
              <Field label="昵称（可选）">
                <input
                  className="input"
                  placeholder="例如 备用号 1"
                  value={keyNick}
                  onChange={(e) => setKeyNick(e.target.value)}
                />
              </Field>
              <Field label="UID（可选）" hint="留空由 key 派生（apikey-<id>）。">
                <input
                  className="input mono"
                  placeholder="留空自动派生"
                  value={keyUid}
                  onChange={(e) => setKeyUid(e.target.value)}
                />
              </Field>
            </div>
          </div>
        )}
      </Modal>

      <Modal
        open={remarkOpen}
        title="备注 / 分组"
        onClose={() => setRemarkOpen(false)}
        wide
        footer={
          <div className="row gap2">
            <button className="btn ghost" onClick={clearRemarks}>
              清空备注
            </button>
            <button className="btn ghost" onClick={() => setRemarkOpen(false)}>
              取消
            </button>
            <button className="btn primary" disabled={!!busy} onClick={saveRemarks}>
              保存
            </button>
          </div>
        }
      >
        <div className="stack">
          <p className="small muted">
            备注只存在这台浏览器（localStorage），不写服务端、不参与选号与计费；分组是服务端字段，改完保存即生效。
          </p>
          <input
            className="input"
            placeholder="搜索昵称 / UID"
            aria-label="搜索账号"
            value={rmKw}
            onChange={(e) => setRmKw(e.target.value)}
          />
          {!roster.data ? (
            roster.error ? (
              <div className="notice bad">
                <span>账号名录加载失败：{errorText(roster.error)}</span>
                <button className="btn sm" onClick={roster.reload}>
                  重试
                </button>
              </div>
            ) : (
              <Skeleton rows={4} />
            )
          ) : (
            <div className="stack">
              {(roster.data.accounts ?? [])
                .filter((it) => {
                  const k = rmKw.trim().toLowerCase()
                  if (!k) return true
                  return (
                    (it.nickname || '').toLowerCase().indexOf(k) >= 0 ||
                    it.uid.toLowerCase().indexOf(k) >= 0 ||
                    (it.group || '').toLowerCase().indexOf(k) >= 0
                  )
                })
                .map((it) => (
                  <div key={it.uid} className="row v-center gap2 row-wrap">
                    <div className="meta grow" style={{ minWidth: '140px' }}>
                      <div className="n truncate">{it.nickname || it.uid.slice(0, 8)}</div>
                      <div className="s tiny mono muted">
                        {it.uid.slice(0, 8)}
                        {it.disabled ? ' · 禁用' : it.paused ? ' · 暂停' : ''}
                      </div>
                    </div>
                    <input
                      className="input xs"
                      maxLength={24}
                      placeholder="备注（留空即清除）"
                      aria-label="备注"
                      value={rmText[it.uid] ?? ''}
                      onChange={(e) => setRmText({ ...rmText, [it.uid]: e.target.value })}
                    />
                    <input
                      className="input xs"
                      maxLength={24}
                      placeholder="分组（留空即未分组）"
                      aria-label="分组"
                      value={gpText[it.uid] ?? ''}
                      onChange={(e) => setGpText({ ...gpText, [it.uid]: e.target.value })}
                    />
                  </div>
                ))}
            </div>
          )}
        </div>
      </Modal>

      <ConfirmDialog
        open={!!confirm}
        title={confirmTitle(confirm)}
        message={confirmMessage(confirm)}
        confirmText={confirmAction(confirm)}
        danger={!!confirm && (confirm.kind === 'remove' || confirm.kind === 'disable')}
        busy={!!busy}
        onConfirm={runConfirm}
        onClose={() => setConfirm(null)}
      />
    </div>
  )
}
