import { useEffect, useMemo, useRef, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useDebounced, useLocalStorage, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { clock } from '../lib/format'
import { Badge, Box, Empty, Segmented, Skeleton, Switch } from '../components/ui'

type LogEntry = {
  ts: string
  seq: number
  ch: 'chat' | 'task' | 'sys'
  text: string
}

type LogsResp = {
  entries: LogEntry[] | null
  archive_error?: string | null
}

type Level = 'debug' | 'info' | 'warn' | 'error'
type LevelFilter = 'all' | Level
type ChanFilter = 'all' | 'chat' | 'task' | 'sys'

const CAP = 800

const LEVEL_OPTIONS: { value: LevelFilter; label: string }[] = [
  { value: 'all', label: '全部级别' },
  { value: 'error', label: '错误' },
  { value: 'warn', label: '警告' },
  { value: 'info', label: '信息' },
  { value: 'debug', label: '调试' },
]

const CHAN_OPTIONS: { value: ChanFilter; label: string }[] = [
  { value: 'all', label: '全部频道' },
  { value: 'chat', label: '对话' },
  { value: 'task', label: '任务' },
  { value: 'sys', label: '系统' },
]

const CHAN_LABEL: Record<LogEntry['ch'], string> = {
  chat: '对话',
  task: '任务',
  sys: '系统',
}

function levelOf(text: string): Level {
  const cleaned = text.replace(/\b[a-z_]+=(""|'')/g, '')
  if (/error|失败|错误/i.test(cleaned)) return 'error'
  if (/warn|冷却|熔断/i.test(cleaned)) return 'warn'
  if (/debug|调试/i.test(cleaned)) return 'debug'
  return 'info'
}

export default function Logs() {
  const { tick } = useRefresh()
  const toast = useToast()
  const [lines, setLines] = useState<LogEntry[]>([])
  const [level, setLevel] = useState<LevelFilter>('all')
  const [chan, setChan] = useState<ChanFilter>('all')
  const [keyword, setKeyword] = useState('')
  const kw = useDebounced(keyword, 250).trim().toLowerCase()
  const [autoScroll, setAutoScroll] = useLocalStorage<boolean>('wb.logs.autoscroll', true)
  const [paused, setPaused] = useState(false)

  const cursor = useRef(0)
  const boxRef = useRef<HTMLDivElement | null>(null)
  const stickRef = useRef(true)

  const q = useQuery<LogsResp>(
    (signal) => api.get<LogsResp>('/logs', { since_seq: cursor.current }, signal),
    [tick, paused],
    { poll: 5000, enabled: !paused },
  )

  useEffect(() => {
    const batch = q.data?.entries ?? []
    if (batch.length === 0) return
    let maxSeq = cursor.current
    for (const e of batch) if (e.seq > maxSeq) maxSeq = e.seq
    cursor.current = maxSeq
    setLines((prev) => {
      const seen = new Set(prev.map((l) => l.seq))
      const add = batch.filter((e) => !seen.has(e.seq))
      if (add.length === 0) return prev
      add.sort((a, b) => a.seq - b.seq)
      const next = prev.concat(add)
      return next.length > CAP ? next.slice(next.length - CAP) : next
    })
  }, [q.data])

  useEffect(() => {
    if (!autoScroll) return
    const el = boxRef.current
    if (!el || !stickRef.current) return
    el.scrollTop = el.scrollHeight
  }, [lines, autoScroll])

  const filtered = useMemo(
    () =>
      lines.filter((l) => {
        if (chan !== 'all' && l.ch !== chan) return false
        if (level !== 'all' && levelOf(l.text) !== level) return false
        if (kw !== '' && !l.text.toLowerCase().includes(kw)) return false
        return true
      }),
    [lines, chan, level, kw],
  )

  const errCount = useMemo(() => lines.filter((l) => levelOf(l.text) === 'error').length, [lines])
  const warnCount = useMemo(() => lines.filter((l) => levelOf(l.text) === 'warn').length, [lines])

  const reloadAll = () => {
    cursor.current = 0
    setLines([])
    // 暂停时 q.reload() 不会触发请求（effect 被 enabled=false 短路），先恢复拉取
    setPaused(false)
    q.reload()
  }

  const clearView = () => {
    setLines([])
    toast.info('已清空当前视图，只影响本页显示')
  }

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>运行日志</h2>
          <p className="lead">服务端运行日志，每 5 秒增量拉取，仅在内存环中保留最近 500 条/频道</p>
        </div>
        <div className="acts">
          {errCount > 0 ? <Badge tone="bad">{errCount} 错误</Badge> : null}
          {warnCount > 0 ? <Badge tone="warn">{warnCount} 警告</Badge> : null}
          <Badge tone="muted">{lines.length} 行</Badge>
          <button
            className="btn sm"
            onClick={() => {
              setPaused((v) => !v)
            }}
          >
            {paused ? '继续拉取' : '暂停拉取'}
          </button>
          <button className="btn sm ghost" onClick={reloadAll}>
            重新载入
          </button>
          <button className="btn sm ghost" onClick={clearView} disabled={lines.length === 0}>
            清空视图
          </button>
        </div>
      </div>

      {q.error ? (
        <div className="notice bad">
          <span>拉取日志失败：{errorText(q.error)}</span>
          <button className="btn sm" onClick={reloadAll}>
            重试
          </button>
        </div>
      ) : null}
      {q.data?.archive_error ? (
        <div className="notice warn">
          <span>日志落盘异常：{q.data.archive_error}</span>
        </div>
      ) : null}
      {paused ? (
        <div className="notice warn">
          <span>已暂停拉取，恢复后会自动补齐暂停期间的日志。</span>
        </div>
      ) : null}

      <Box
        title="日志流"
        sub="等宽字体、可换行；含 error/失败 的行标红，含 warn/冷却/熔断 的行标黄"
        actions={
          <span className="tiny muted">
            {filtered.length === lines.length ? `${lines.length} 条` : `${filtered.length}/${lines.length} 条`}
          </span>
        }
      >
        <div className="filters">
          <div className="segmented-scroll">
            <Segmented<LevelFilter> value={level} options={LEVEL_OPTIONS} onChange={setLevel} ariaLabel="日志级别" />
          </div>
          <div className="segmented-scroll">
            <Segmented<ChanFilter> value={chan} options={CHAN_OPTIONS} onChange={setChan} ariaLabel="日志频道" />
          </div>
          <input
            className="input"
            placeholder="关键字过滤"
            value={keyword}
            onChange={(e) => {
              setKeyword(e.target.value)
            }}
            aria-label="关键字过滤"
          />
          <div className="row">
            <Switch
              checked={autoScroll}
              onChange={(v) => {
                stickRef.current = true
                setAutoScroll(v)
              }}
              label="自动滚到底部"
            />
            <span className="small muted">自动滚动</span>
          </div>
        </div>

        {q.first && lines.length === 0 ? (
          <Skeleton rows={6} />
        ) : lines.length === 0 ? (
          <Empty
            title="暂无日志"
            hint="服务刚启动或日志已被清空；发起一次对话或手动触发任务后这里会出现记录。"
            action={
              <button className="btn sm" onClick={reloadAll}>
                重新载入
              </button>
            }
          />
        ) : filtered.length === 0 ? (
          <Empty
            title="没有匹配的日志"
            hint="放宽级别/频道筛选，或清空关键字再试。"
            action={
              <button
                className="btn sm"
                onClick={() => {
                  setLevel('all')
                  setChan('all')
                  setKeyword('')
                }}
              >
                重置筛选
              </button>
            }
          />
        ) : (
          <div
            className="feed-box"
            ref={boxRef}
            onScroll={(e) => {
              const el = e.currentTarget
              stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
            }}
          >
            <div className="feed">
              {filtered.map((l) => {
                const lv = levelOf(l.text)
                return (
                  <div className={`line ${lv}`} key={l.seq}>
                    <span className="t">{clock(l.ts)}</span>
                    <span className={`lv ${lv}`}>{lv}</span>
                    <span className="msg">
                      {chan === 'all' ? <span className="dim">[{CHAN_LABEL[l.ch]}] </span> : null}
                      {l.text}
                    </span>
                  </div>
                )
              })}
            </div>
          </div>
        )}
      </Box>
    </div>
  )
}
