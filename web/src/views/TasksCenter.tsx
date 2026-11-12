import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { ago, clamp, isZeroTime, num, shortTime } from '../lib/format'
import {
  Badge,
  Box,
  ConfirmDialog,
  Empty,
  Field,
  KV,
  Modal,
  Skeleton,
  Stat,
  StatsGrid,
  Switch,
} from '../components/ui'
import { DataTable, type Column } from '../components/DataTable'

type QueueStatus = 'pending' | 'running' | 'done' | 'skipped' | 'error'
type JobStatus = 'pending' | 'running' | 'finished' | 'interrupted' | 'error'

type QueueItem = {
  uid: string
  nickname: string
  kind: string
  code: string
  title?: string
  status: QueueStatus
  message?: string
  credit?: number
  energy?: number
}

type QueueResp = {
  running: boolean
  total: number
  conc: number
  started: boolean
  started_at: string
  seq: number
  items: QueueItem[] | null
}

type TaskJob = {
  id: string
  uid: string
  nickname: string
  realm?: string
  trigger: string
  status: JobStatus
  started_at: string
  updated_at: string
  finished_at?: string
  current_task?: string
  completed: number
  total: number
  results?: unknown[]
  message?: string
  error?: string
}

type JobsResp = {
  ok: boolean
  auto_enabled: boolean
  jobs: TaskJob[] | null
}

type Schedule = {
  checkin_enabled?: boolean
  checkin_hours?: number[]
  growth_enabled?: boolean
  growth_hours?: number[]
  growth_concurrency?: number
  travel_enabled?: boolean
  travel_hours?: number[]
  activity_enabled?: boolean
  activity_hours?: number[]
  web_activity_enabled?: boolean
  keepalive_enabled?: boolean
  keepalive_hours?: number[]
  balance_refresh_enabled?: boolean
  balance_refresh_minutes?: number
  include_disabled_in_tasks?: boolean
}

type ConfigResp = {
  ok: boolean
  path: string
  config: { schedule?: Schedule } & Record<string, unknown>
  environment_managed_fields: string[] | null
}

type BatchAction = {
  label: string
  path: string
  desc: string
  sync?: boolean
}

const BATCH_ACTIONS: BatchAction[] = [
  { label: '全部签到', path: '/checkin_all', desc: '对全部启用账号执行签到（含猫猫旅行）' },
  { label: '猫猫旅行', path: '/travel_all', desc: '对全部启用账号执行猫猫旅行' },
  { label: '点亮连登', path: '/activity_all', desc: '对全部启用账号点亮连登活动' },
  { label: '保活刷新', path: '/keepalive_all', desc: '对全部启用账号执行保活刷新' },
  { label: '刷新余额', path: '/balance_all', desc: '同步刷新全部账号余额，可能需要数十秒', sync: true },
]

const QUEUE_TONE: Record<QueueStatus, 'ok' | 'warn' | 'bad' | 'accent' | 'muted'> = {
  pending: 'muted',
  running: 'accent',
  done: 'ok',
  skipped: 'muted',
  error: 'bad',
}

const QUEUE_LABEL: Record<QueueStatus, string> = {
  pending: '待执行',
  running: '执行中',
  done: '完成',
  skipped: '跳过',
  error: '失败',
}

const JOB_TONE: Record<JobStatus, 'ok' | 'warn' | 'bad' | 'accent' | 'muted'> = {
  pending: 'muted',
  running: 'accent',
  finished: 'ok',
  interrupted: 'warn',
  error: 'bad',
}

const JOB_LABEL: Record<JobStatus, string> = {
  pending: '排队',
  running: '执行中',
  finished: '完成',
  interrupted: '中断',
  error: '失败',
}

type Pending = { kind: 'batch'; action: BatchAction } | { kind: 'queue' }

function hoursText(v: number[] | undefined): string {
  if (!v || v.length === 0) return ''
  return v.join(', ')
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

function jobSeconds(job: TaskJob): number {
  const from = new Date(job.started_at).getTime()
  const to = new Date(job.finished_at ?? job.updated_at).getTime()
  if (isNaN(from) || isNaN(to) || to <= from) return 0
  return Math.round((to - from) / 1000)
}

export default function TasksCenter() {
  const { tick } = useRefresh()
  const toast = useToast()

  const [pending, setPending] = useState<Pending | null>(null)
  const [busyLabel, setBusyLabel] = useState('')
  const [concurrency, setConcurrency] = useState(1)
  const [scanNote, setScanNote] = useState('')
  const [jobDetail, setJobDetail] = useState<TaskJob | null>(null)
  const [edits, setEdits] = useState<Record<string, unknown>>({})
  const [savingSchedule, setSavingSchedule] = useState(false)

  const queue = useQuery<QueueResp>((signal) => api.get<QueueResp>('/tasks/queue', undefined, signal), [tick], {
    poll: 5000,
  })
  const jobs = useQuery<JobsResp>((signal) => api.get<JobsResp>('/tasks/jobs', undefined, signal), [tick], {
    poll: 10000,
  })
  const cfg = useQuery<ConfigResp>((signal) => api.get<ConfigResp>('/config', undefined, signal), [tick])

  const q = queue.data
  const jobList = jobs.data?.jobs ?? []
  const schedule: Schedule = { ...(cfg.data?.config.schedule ?? {}), ...edits } as Schedule
  const dirty = Object.keys(edits).length > 0
  const editKeys = new Set(Object.keys(edits).map((k) => 'schedule.' + k))
  const envManaged = (cfg.data?.environment_managed_fields ?? []).filter((f) => f.startsWith('schedule.'))
  const touchedEnv = [...editKeys].filter((k) => envManaged.includes(k))

  const failedJobs = useMemo(() => jobList.filter((j) => j.status === 'error' || j.status === 'interrupted'), [jobList])
  const queueDone = q?.items?.filter((i) => i.status === 'done').length ?? 0
  const queueError = q?.items?.filter((i) => i.status === 'error').length ?? 0

  const setField = (key: keyof Schedule, value: unknown) => {
    setEdits((prev) => ({ ...prev, [key]: value }))
  }

  const reloadAll = () => {
    queue.reload()
    jobs.reload()
  }

  const confirmPending = async () => {
    if (!pending) return
    const label = pending.kind === 'batch' ? pending.action.label : '执行队列'
    setBusyLabel(label)
    try {
      if (pending.kind === 'queue') {
        const res = await api.post<{ ok: boolean; started: boolean; total: number; message?: string }>(
          '/tasks/run_queue',
          { concurrency },
        )
        if (res.started) {
          toast.ok(`队列已启动：${num(res.total)} 个待办`)
        } else {
          toast.warn(res.message || '队列未启动')
        }
      } else if (pending.action.sync) {
        const res = await api.post<{ ok: boolean; accounts?: unknown[] }>(pending.action.path)
        toast.ok(`余额已刷新：${num((res.accounts ?? []).length)} 个账号`)
      } else {
        await api.post<{ ok: boolean; started?: boolean }>(pending.action.path)
        toast.ok(`${pending.action.label}已开始，可在下方队列与运行记录查看进度`)
      }
      setPending(null)
      reloadAll()
    } catch (e: unknown) {
      toast.err(errorText(e))
    } finally {
      setBusyLabel('')
    }
  }

  const scan = () => {
    setBusyLabel('扫描')
    api
      .post<{ ok: boolean; pending_count: number; skipped_count: number; error_count: number }>('/tasks/scan_all')
      .then((res) => {
        setScanNote(
          `扫描完成：待执行 ${num(res.pending_count)} · 跳过 ${num(res.skipped_count)} · 扫描失败 ${num(res.error_count)}`,
        )
        toast.ok('扫描完成')
      })
      .catch((e: unknown) => {
        toast.err(errorText(e))
      })
      .finally(() => {
        setBusyLabel('')
      })
  }

  const saveSchedule = () => {
    if (!dirty) return
    setSavingSchedule(true)
    api
      .post<{ ok: boolean; restart_required: string[] | null }>('/config', { schedule: edits })
      .then((res) => {
        const need = res.restart_required ?? []
        toast.ok(need.length > 0 ? `已保存，需重启生效：${need.join('、')}` : '定时设置已保存')
        setEdits({})
        cfg.reload()
        queue.reload()
      })
      .catch((e: unknown) => {
        toast.err(errorText(e))
      })
      .finally(() => {
        setSavingSchedule(false)
      })
  }

  const queueColumns: Column<QueueItem>[] = [
    { key: 'nickname', title: '账号', render: (i) => i.nickname || i.uid.slice(0, 8) },
    { key: 'title', title: '任务', hideMobile: true, render: (i) => i.title || i.code },
    { key: 'status', title: '状态', render: (i) => <Badge tone={QUEUE_TONE[i.status]}>{QUEUE_LABEL[i.status]}</Badge> },
    {
      key: 'message',
      title: '说明',
      render: (i) =>
        i.status === 'error' && i.message ? (
          <span className="warn-text truncate" title={i.message}>
            {i.message}
          </span>
        ) : (
          <span className="muted truncate" title={i.message}>
            {i.message || '-'}
          </span>
        ),
    },
  ]

  const jobColumns: Column<TaskJob>[] = [
    { key: 'nickname', title: '账号', render: (j) => j.nickname || j.uid.slice(0, 8) },
    {
      key: 'trigger',
      title: '触发',
      hideMobile: true,
      render: (j) => <span className="muted">{j.trigger === 'daily' ? '定时' : '手动'}</span>,
    },
    { key: 'status', title: '状态', render: (j) => <Badge tone={JOB_TONE[j.status]}>{JOB_LABEL[j.status]}</Badge> },
    {
      key: 'progress',
      title: '进度',
      align: 'right',
      render: (j) => (
        <span className="nowrap">
          {num(j.completed)}/{num(j.total)}
        </span>
      ),
    },
    { key: 'started', title: '开始', hideMobile: true, render: (j) => <span title={j.started_at}>{ago(j.started_at)}</span> },
    {
      key: 'detail',
      title: '说明',
      render: (j) =>
        j.error ? (
          <span className="warn-text truncate" title={j.error}>
            {j.error}
          </span>
        ) : (
          <span className="muted truncate" title={j.message || j.current_task}>
            {j.message || j.current_task || '-'}
          </span>
        ),
    },
  ]

  const jobDetailItems = jobDetail
    ? [
        { k: '任务 ID', v: jobDetail.id, mono: true },
        { k: '账号', v: jobDetail.nickname || jobDetail.uid },
        { k: '状态', v: JOB_LABEL[jobDetail.status] },
        { k: '触发', v: jobDetail.trigger === 'daily' ? '定时' : '手动' },
        { k: '进度', v: `${num(jobDetail.completed)} / ${num(jobDetail.total)}` },
        { k: '开始', v: shortTime(jobDetail.started_at) + '（' + ago(jobDetail.started_at) + '）' },
        { k: '结束', v: jobDetail.finished_at ? shortTime(jobDetail.finished_at) : '未结束' },
        { k: '耗时', v: jobSeconds(jobDetail) > 0 ? `${num(jobSeconds(jobDetail))} 秒` : '-' },
        { k: '当前任务', v: jobDetail.current_task || '-' },
        { k: '说明', v: jobDetail.message || '-' },
        { k: '错误', v: jobDetail.error || '-' },
      ]
    : []

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>任务中心</h2>
          <p className="lead">手动触发签到/旅行/点亮/保活，查看成长队列与最近运行记录，并调整定时策略</p>
        </div>
        <div className="acts">
          <button className="btn sm" onClick={reloadAll} disabled={queue.loading || jobs.loading}>
            刷新
          </button>
        </div>
      </div>

      {queue.error ? (
        <div className="notice bad">
          <span>队列状态读取失败：{errorText(queue.error)}</span>
          <button className="btn sm" onClick={queue.reload}>
            重试
          </button>
        </div>
      ) : null}
      {jobs.error ? (
        <div className="notice bad">
          <span>运行记录读取失败：{errorText(jobs.error)}</span>
          <button className="btn sm" onClick={jobs.reload}>
            重试
          </button>
        </div>
      ) : null}
      {failedJobs.length > 0 ? (
        <div className="notice warn">
          <span>
            最近记录中有 {num(failedJobs.length)} 条失败/中断：
            {failedJobs
              .slice(0, 2)
              .map((j) => `${j.nickname || j.uid.slice(0, 8)}（${JOB_LABEL[j.status]}）`)
              .join('、')}
            {failedJobs.length > 2 ? ' 等' : ''}
          </span>
        </div>
      ) : null}

      {q ? (
        <StatsGrid>
          <Stat label="队列状态" value={q.running ? '执行中' : q.started ? '已完成' : '空闲'} sub={`并发 ${num(q.conc)}`} />
          <Stat label="待办总数" value={num(q.total)} sub={`完成 ${num(queueDone)}`} />
          <Stat label="失败任务" value={num(queueError)} sub={queueError > 0 ? '查看下方队列明细' : '无'} />
          <Stat label="自动任务" value={jobs.data?.auto_enabled ? '已启用' : '已关闭'} sub={`记录 ${num(jobList.length)} 条`} />
        </StatsGrid>
      ) : (
        <Box>
          <Skeleton rows={3} />
        </Box>
      )}

      <Box title="手动触发" sub="全量任务共用一把闸门，同一时刻只允许一轮在执行" actions={
        <span className="tiny muted">{busyLabel ? `执行中：${busyLabel}` : ''}</span>
      }>
        <div className="row-wrap">
          {BATCH_ACTIONS.map((a) => (
            <button
              key={a.path}
              className={a.sync ? 'btn' : 'btn primary'}
              disabled={busyLabel !== ''}
              onClick={() => setPending({ kind: 'batch', action: a })}
            >
              {a.label}
            </button>
          ))}
        </div>
        <p className="tiny muted">
          已有一轮全量任务在跑时，后端会返回 409，请等本轮完成再触发。
        </p>
      </Box>

      <Box
        title="成长任务队列"
        sub={
          q?.started_at && !isZeroTime(q.started_at)
            ? `本轮启动于 ${shortTime(q.started_at)}（${ago(q.started_at)}）`
            : '尚未启动过队列'
        }
        actions={
          <div className="row">
            <select
              className="select xs"
              value={concurrency}
              onChange={(e) => setConcurrency(Number(e.target.value))}
              aria-label="队列并发"
            >
              <option value={1}>并发 1</option>
              <option value={2}>并发 2</option>
              <option value={3}>并发 3</option>
              <option value={4}>并发 4</option>
            </select>
            <button className="btn sm" disabled={busyLabel !== ''} onClick={scan}>
              仅扫描
            </button>
            <button
              className="btn sm primary"
              disabled={busyLabel !== '' || (q?.running ?? false)}
              onClick={() => setPending({ kind: 'queue' })}
            >
              扫描并执行
            </button>
          </div>
        }
        footer={
          scanNote ? <span className="tiny muted">{scanNote}</span> : <span className="tiny muted">执行后队列每 5 秒自动刷新</span>
        }
      >
        <DataTable<QueueItem>
          columns={queueColumns}
          rows={q?.items ?? []}
          rowKey={(i) => i.uid + ':' + i.code}
          loading={queue.first && queue.loading}
          empty="队列为空"
          emptyHint="点击右上「扫描并执行」抓取各账号的成长任务并开始执行。"
        />
      </Box>

      <Box title="最近运行记录" sub="按更新时间倒序，最多保留后端设置的历史条数">
        <DataTable<TaskJob>
          columns={jobColumns}
          rows={jobList}
          rowKey={(j) => j.id}
          loading={jobs.first && jobs.loading}
          empty="暂无运行记录"
          emptyHint="定时任务到点或手动触发后，这里会出现每次运行的明细。"
          onRowClick={(j) => setJobDetail(j)}
          caption="点击行可查看本次运行的详细说明"
        />
      </Box>

      <Box
        title="定时设置"
        sub="修改后立即热生效，无需重启；留空的时段表示该任务当天不执行"
        footer={
          dirty ? (
            <div className="actionbar">
              <span className="tiny warn-text">有未保存的修改</span>
              <button className="btn ghost" onClick={() => setEdits({})} disabled={savingSchedule}>
                放弃修改
              </button>
              <button className="btn primary" onClick={saveSchedule} disabled={savingSchedule}>
                {savingSchedule ? '保存中…' : '保存定时设置'}
              </button>
            </div>
          ) : (
            <span className="tiny muted">尚未修改</span>
          )
        }
      >
        {!cfg.data ? (
          cfg.error ? (
            <div className="notice bad">
              <span>配置读取失败：{errorText(cfg.error)}</span>
              <button className="btn sm" onClick={cfg.reload}>
                重试
              </button>
            </div>
          ) : (
            <Skeleton rows={4} />
          )
        ) : (
          <div className="stack">
            {touchedEnv.length > 0 ? (
              <div className="notice warn">
                <span>这些字段由环境变量托管，保存不会生效：{touchedEnv.join('、')}</span>
              </div>
            ) : null}
            <div className="formgrid">
              <Field label="每日签到" hint="控制 /checkin_all 对应的定时任务">
                <div className="row">
                  <Switch
                    checked={!!schedule.checkin_enabled}
                    onChange={(v) => setField('checkin_enabled', v)}
                  />
                  <span className="tiny muted">{schedule.checkin_enabled ? '启用' : '关闭'}</span>
                </div>
              </Field>
              <Field label="签到时段" hint="0–23 小时，逗号分隔">
                <input
                  className="input"
                  value={hoursText(schedule.checkin_hours)}
                  placeholder="如 8, 20"
                  onChange={(e) => setField('checkin_hours', parseHours(e.target.value))}
                />
              </Field>
              <Field label="成长任务" hint="成长任务队列的定时执行">
                <div className="row">
                  <Switch
                    checked={!!schedule.growth_enabled}
                    onChange={(v) => setField('growth_enabled', v)}
                  />
                  <span className="tiny muted">{schedule.growth_enabled ? '启用' : '关闭'}</span>
                </div>
              </Field>
              <Field label="队列并发" hint="1–4，过高容易触发上游限流">
                <input
                  className="input"
                  type="number"
                  min={1}
                  max={4}
                  value={schedule.growth_concurrency ?? 1}
                  onChange={(e) => setField('growth_concurrency', clamp(Number(e.target.value) || 1, 1, 4))}
                />
              </Field>
              <Field label="成长时段" hint="0–23 小时，逗号分隔">
                <input
                  className="input"
                  value={hoursText(schedule.growth_hours)}
                  placeholder="如 9, 21"
                  onChange={(e) => setField('growth_hours', parseHours(e.target.value))}
                />
              </Field>
              <Field label="猫猫旅行">
                <div className="row">
                  <Switch
                    checked={!!schedule.travel_enabled}
                    onChange={(v) => setField('travel_enabled', v)}
                  />
                  <span className="tiny muted">{schedule.travel_enabled ? '启用' : '关闭'}</span>
                </div>
              </Field>
              <Field label="旅行时段" hint="0–23 小时，逗号分隔">
                <input
                  className="input"
                  value={hoursText(schedule.travel_hours)}
                  placeholder="如 10"
                  onChange={(e) => setField('travel_hours', parseHours(e.target.value))}
                />
              </Field>
              <Field label="点亮连登">
                <div className="row">
                  <Switch
                    checked={!!schedule.activity_enabled}
                    onChange={(v) => setField('activity_enabled', v)}
                  />
                  <span className="tiny muted">{schedule.activity_enabled ? '启用' : '关闭'}</span>
                </div>
              </Field>
              <Field label="点亮时段" hint="0–23 小时，逗号分隔">
                <input
                  className="input"
                  value={hoursText(schedule.activity_hours)}
                  placeholder="如 7"
                  onChange={(e) => setField('activity_hours', parseHours(e.target.value))}
                />
              </Field>
              <Field label="网页活动" hint="仅在网页端参与的账号需要">
                <div className="row">
                  <Switch
                    checked={!!schedule.web_activity_enabled}
                    onChange={(v) => setField('web_activity_enabled', v)}
                  />
                  <span className="tiny muted">{schedule.web_activity_enabled ? '启用' : '关闭'}</span>
                </div>
              </Field>
              <Field label="保活刷新">
                <div className="row">
                  <Switch
                    checked={!!schedule.keepalive_enabled}
                    onChange={(v) => setField('keepalive_enabled', v)}
                  />
                  <span className="tiny muted">{schedule.keepalive_enabled ? '启用' : '关闭'}</span>
                </div>
              </Field>
              <Field label="保活时段" hint="0–23 小时，逗号分隔">
                <input
                  className="input"
                  value={hoursText(schedule.keepalive_hours)}
                  placeholder="如 6, 18"
                  onChange={(e) => setField('keepalive_hours', parseHours(e.target.value))}
                />
              </Field>
              <Field label="余额刷新">
                <div className="row">
                  <Switch
                    checked={!!schedule.balance_refresh_enabled}
                    onChange={(v) => setField('balance_refresh_enabled', v)}
                  />
                  <span className="tiny muted">{schedule.balance_refresh_enabled ? '启用' : '关闭'}</span>
                </div>
              </Field>
              <Field label="余额刷新间隔" hint="分钟">
                <input
                  className="input"
                  type="number"
                  min={1}
                  value={schedule.balance_refresh_minutes ?? 5}
                  onChange={(e) => setField('balance_refresh_minutes', clamp(Number(e.target.value) || 5, 1, 1440))}
                />
              </Field>
              <Field label="包含已禁用账号" hint="定时任务是否扫描已禁用账号">
                <div className="row">
                  <Switch
                    checked={!!schedule.include_disabled_in_tasks}
                    onChange={(v) => setField('include_disabled_in_tasks', v)}
                  />
                  <span className="tiny muted">{schedule.include_disabled_in_tasks ? '包含' : '不包含'}</span>
                </div>
              </Field>
            </div>
          </div>
        )}
      </Box>

      <ConfirmDialog
        open={pending !== null}
        title={pending?.kind === 'batch' ? pending.action.label : '执行成长任务队列'}
        message={
          pending?.kind === 'batch'
            ? `${pending.action.desc}。确认现在触发？`
            : `将为所有启用账号抓取成长任务并以并发 ${concurrency} 执行，执行期间其他全量任务会被拒绝。确认开始？`
        }
        confirmText={pending?.kind === 'batch' ? '立即触发' : '开始执行'}
        busy={busyLabel !== ''}
        onConfirm={confirmPending}
        onClose={() => setPending(null)}
      />

      <Modal
        open={jobDetail !== null}
        title="运行详情"
        wide
        onClose={() => setJobDetail(null)}
        footer={
          <button className="btn" onClick={() => setJobDetail(null)}>
            关闭
          </button>
        }
      >
        {jobDetail ? (
          <div className="stack">
            {jobDetail.error ? (
              <div className="notice bad">
                <span>失败原因：{jobDetail.error}</span>
              </div>
            ) : null}
            <KV items={jobDetailItems} />
            {jobDetail.results && jobDetail.results.length > 0 ? (
              <div className="col">
                <div className="tiny muted">逐任务结果</div>
                <pre className="pre">{JSON.stringify(jobDetail.results, null, 2)}</pre>
              </div>
            ) : null}
          </div>
        ) : (
          <Empty title="未选择记录" />
        )}
      </Modal>
    </div>
  )
}
