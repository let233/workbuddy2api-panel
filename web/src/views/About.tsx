import { useMemo, useState } from 'react'
import { api, errorText } from '../lib/api'
import { useBusy, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { ago, duration, num, shortTime } from '../lib/format'
import { Badge, Box, ConfirmDialog, Empty, Field, KV, Skeleton, Stat, StatsGrid, Switch } from '../components/ui'

type UpdateStatus = {
  current_version: string
  current_known: boolean
  latest_version: string
  release_url: string
  release_notes: string
  checked_at: string
  last_error: string
  update_available: boolean
  can_install: boolean
  installing: boolean
  auto_enabled: boolean
  auto_time: string
  channel: string
  proxy_host?: string
  mirror?: string
  last_error_kind?: string
  container?: boolean
  install_blocked?: string
  install_mode?: string
  insecure_tls?: boolean
}

type Overview = {
  version: string
  poll_interval_sec: number
  update: UpdateStatus | null
  uptime_sec: number
  login_required: boolean
  redis_mode: string
  sticky_sessions: number
  total: number
  healthy: number
  cooling: number
  disabled: number
  paused: number
  credits_sum: number
}

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

type ProbeResult = {
  target: string
  url: string
  ok: boolean
  status?: number
  elapsed_ms: number
  kind?: string
  error?: string
}

type ProbeResp = {
  ok: boolean
  meta: ProbeResult
  asset: ProbeResult
  update: UpdateStatus
}

const ERROR_KIND_LABEL: Record<string, string> = {
  dns: 'DNS 解析失败',
  connect: '连接失败',
  tls: 'TLS 握手失败',
  'header-timeout': '首包超时',
  'body-stall': '响应体停滞',
  checksum: '校验和不匹配',
  http: 'HTTP 错误',
  'proxy-config': '代理地址不合法',
  'mirror-config': '镜像前缀不合法',
  unknown: '未知原因',
}

// 每种失败类目对应一句能直接照做的建议（旧面板同口径）。
const ERROR_ADVICE: Record<string, string> = {
  dns: '检查容器 DNS（可用 dns_servers）与出站策略。',
  connect: '出站被拦或网络不可达；可配「更新代理」或「镜像前缀」绕行。',
  tls: '握手一直超时说明链路被拦，换代理或镜像（关证书校验救不了）；证书不被信任可临时开「跳过证书校验」。',
  'header-timeout': '出站到 release-assets 很可能被拦（api.github.com 往往仍通）；配代理或镜像即可。',
  'body-stall': '链路抖动或镜像限速；换镜像或配代理。',
  checksum: '下载内容被中间层改写；换回直连或换可信镜像（不要放宽校验）。',
  http: '镜像不可用或前缀路径不对；检查镜像前缀。',
  'proxy-config': '代理地址不合法；只支持 http、https、socks5 三种协议。',
  'mirror-config': '镜像前缀不合法；需要 http 或 https 加主机名加斜杠的形式。',
}

const CHANNEL_LABEL: Record<string, string> = {
  direct: '直连（已关闭环境变量代理）',
  'direct(env)': '直连（跟随环境变量）',
  proxy: '代理',
  mirror: '镜像',
  'mirror+proxy': '镜像 + 代理',
}

const INSTALL_MODE_LABEL: Record<string, string> = {
  helper: '辅助进程替换',
  'in-place': '原地替换',
}

type UpdateField = {
  path: string
  label: string
  kind: 'switch' | 'text' | 'time'
  placeholder?: string
  hint?: string
}

const UPDATE_FIELDS: UpdateField[] = [
  {
    path: 'update.auto_enabled',
    label: '自动更新',
    kind: 'switch',
    hint: '每天按上海时间检查并安装新的时间标签版 Release；容器内是原地替换（重建容器会回到镜像版本）',
  },
  { path: 'update.auto_time', label: '自动更新时间（上海）', kind: 'time', hint: 'HH:MM，缺省 04:00' },
  {
    path: 'update.proxy',
    label: '更新代理',
    kind: 'text',
    placeholder: 'socks5://127.0.0.1:1080',
    hint: '空 = 不显式设置；支持 http / https / socks5',
  },
  {
    path: 'update.mirror',
    label: '资产下载镜像前缀',
    kind: 'text',
    placeholder: 'https://ghfast.top/',
    hint: '只代理资产下载；版本元数据与 sha256 校验仍直连 GitHub',
  },
  {
    path: 'update.proxy_from_env',
    label: '沿用环境变量代理',
    kind: 'switch',
    hint: '关闭后完全不读 HTTP_PROXY / HTTPS_PROXY，只认上面的「更新代理」',
  },
  {
    path: 'update.insecure_tls',
    label: '跳过证书校验（应急）',
    kind: 'switch',
    hint:
      '只对「证书不被信任」（自签 / 系统 CA 过期）有效；TLS 握手超时开它没用，那要走代理或镜像。' +
      '开启期间元数据与下载都不再校验，SHA-256 校验随之失效（摘要来自同一条通道），仅限链路可信的内网部署临时使用',
  },
]

function stripV(v: string): string {
  if (!v) return ''
  return v.startsWith('v') ? v.slice(1) : v
}

function getPath(obj: unknown, path: string): unknown {
  let cur: unknown = obj
  for (const key of path.split('.')) {
    if (typeof cur !== 'object' || cur === null) return undefined
    cur = (cur as Record<string, unknown>)[key]
  }
  return cur
}

function updateNote(up: UpdateStatus | null, self: string): string {
  if (!up) return '未启用更新检查'
  if (up.installing) return '正在准备更新…'
  if (up.last_error) return '更新失败：' + up.last_error
  if (!up.current_known) {
    return '开发构建，无法比较版本' + (up.latest_version ? ' · 最新 Release v' + stripV(up.latest_version) : '')
  }
  if (up.update_available) {
    const tail = up.can_install
      ? up.container
        ? ' · 容器内原地更新（重建容器会回到镜像版本）'
        : ''
      : ' · 请手动更新：' + (up.install_blocked || '当前环境不支持面板内安装')
    return '发现新版本 v' + stripV(up.latest_version || self) + tail
  }
  if (up.latest_version) return '已是最新版本 · v' + stripV(up.latest_version)
  return '尚未检查'
}

export default function About() {
  const { tick } = useRefresh()
  const toast = useToast()
  const busy = useBusy()
  const [confirmInstall, setConfirmInstall] = useState(false)
  const [edits, setEdits] = useState<Record<string, unknown>>({})
  const [saving, setSaving] = useState(false)
  const [probe, setProbe] = useState<ProbeResp | null>(null)
  const [probing, setProbing] = useState(false)

  const q = useQuery<Overview>((signal) => api.get<Overview>('/overview', undefined, signal), [tick], { poll: 15000 })
  // GET /update 单独查一次：比 overview.update 更新更及时（检查/探测后立刻反映）
  const updQ = useQuery<UpdateStatus>(
    (signal) => api.get<UpdateStatus>('/update', undefined, signal),
    [tick],
    { poll: 15000, enabled: q.data !== undefined },
  )
  const cfgQ = useQuery<ConfigResp>((signal) => api.get<ConfigResp>('/config', undefined, signal), [tick])

  const d = q.data
  const up: UpdateStatus | null = updQ.data ?? d?.update ?? null
  const cfg = cfgQ.data?.config
  const envManaged = useMemo(() => new Set(cfgQ.data?.environment_managed_fields ?? []), [cfgQ.data])

  const dirtyPaths = Object.keys(edits)
  const dirty = dirtyPaths.length > 0
  const read = (path: string): unknown => (path in edits ? edits[path] : getPath(cfg, path))
  const write = (path: string, value: unknown) => {
    setEdits((prev) => ({ ...prev, [path]: value }))
  }

  const saveSettings = () => {
    if (!dirty) return
    const body: Record<string, Record<string, unknown>> = {}
    for (const path of dirtyPaths) {
      const parts = path.split('.')
      if (parts.length !== 2) continue
      const group = body[parts[0]] ?? {}
      group[parts[1]] = edits[path]
      body[parts[0]] = group
    }
    setSaving(true)
    api
      .post<SaveResp>('/config', body)
      .then((res) => {
        const need = res.restart_required ?? []
        setEdits({})
        toast.ok(need.length > 0 ? '已保存，需重启生效：' + need.join('、') : '更新设置已保存并生效')
        cfgQ.reload()
        updQ.reload()
        q.reload()
      })
      .catch((e: unknown) => {
        toast.err(errorText(e))
      })
      .finally(() => {
        setSaving(false)
      })
  }

  const checkUpdate = () => {
    void busy.run('check', async () => {
      try {
        const s = await api.post<UpdateStatus>('/update/check')
        if (s.update_available) toast.ok('发现新版本 ' + (s.latest_version || ''))
        else toast.ok('已是最新版本')
        updQ.reload()
        q.reload()
      } catch (e: unknown) {
        toast.err(errorText(e))
      }
    })
  }

  const installUpdate = () => {
    void busy.run('install', async () => {
      try {
        await api.post<{ ok: boolean; restarting: boolean }>('/update/install')
        setConfirmInstall(false)
        toast.ok('更新已开始，服务将在几秒后自动重启')
        q.reload()
      } catch (e: unknown) {
        toast.err(errorText(e))
      }
    })
  }

  const probeChannel = () => {
    setProbing(true)
    api
      .post<ProbeResp>('/update/probe')
      .then((res) => {
        setProbe(res)
        if (res.ok) toast.ok('通道可达：版本元数据与资产下载都通')
        else toast.warn('通道未全部通过，见下方探测结果')
      })
      .catch((e: unknown) => {
        toast.err(errorText(e))
      })
      .finally(() => {
        setProbing(false)
      })
  }

  const runtimeItems = d
    ? [
        { k: '当前版本', v: (d.version || 'dev') + '（dev = 本地构建未注入版本）' },
        { k: '运行时长', v: duration(d.uptime_sec) + '（' + num(d.uptime_sec) + ' 秒）' },
        { k: 'Redis 模式', v: d.redis_mode === 'upstash' ? 'Redis 镜像（upstash）' : '本地内存 / sqlite' },
        { k: '配置文件', v: cfgQ.data?.path || '读取中…', mono: true },
        { k: '面板刷新间隔', v: d.poll_interval_sec > 0 ? num(d.poll_interval_sec) + ' 秒' : '前端默认（5 秒）' },
        { k: '登录鉴权', v: d.login_required ? '已开启' : '未开启' },
        { k: '粘性会话', v: num(d.sticky_sessions) + ' 个在绑会话' },
        {
          k: '账号状态',
          v:
            '共 ' +
            num(d.total) +
            ' · 健康 ' +
            num(d.healthy) +
            ' · 冷却 ' +
            num(d.cooling) +
            ' · 暂停 ' +
            num(d.paused) +
            ' · 禁用 ' +
            num(d.disabled),
        },
        { k: '数据与凭据', v: '账号、客户端 Key、面板账号与会话存在本地 sqlite，不写进配置文件' },
      ]
    : []

  const channelText = up
    ? (CHANNEL_LABEL[up.channel] ?? up.channel ?? '未知通道') +
      (up.proxy_host ? ' · 代理 ' + up.proxy_host : '') +
      (up.mirror ? ' · 镜像 ' + up.mirror : '') +
      (up.insecure_tls ? ' · 不校验证书（应急）' : '')
    : '—'

  const updateItems = up
    ? [
        { k: '当前版本', v: up.current_version || d?.version || 'dev', mono: true },
        { k: '最新版本', v: up.latest_version ? 'v' + stripV(up.latest_version) : '未知', mono: true },
        {
          k: '最近检查',
          v: up.checked_at ? shortTime(up.checked_at) + '（' + ago(up.checked_at) + '）' : '尚未检查',
        },
        { k: '自动更新', v: up.auto_enabled ? '每天 ' + (up.auto_time || '-') : '已关闭' },
        { k: '安装方式', v: up.install_mode ? INSTALL_MODE_LABEL[up.install_mode] ?? up.install_mode : '未知' },
        { k: '运行环境', v: up.container ? '容器（原地替换）' : '裸机 / 服务托管' },
        { k: '可安装', v: up.can_install ? '是' : '否' + (up.install_blocked ? '：' + up.install_blocked : '') },
      ]
    : []

  const probeRows = (): { label: string; p: ProbeResult }[] =>
    probe
      ? [
          { label: '版本元数据', p: probe.meta },
          { label: '资产下载', p: probe.asset },
        ]
      : []

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>关于</h2>
          <p className="lead">版本、运行环境与更新通道；通道设置保存后立即热生效</p>
        </div>
        <div className="acts">
          <button className="btn sm" onClick={checkUpdate} disabled={busy.busy !== ''}>
            {busy.busy === 'check' ? '检查中…' : '检查更新'}
          </button>
          <button className="btn sm" onClick={probeChannel} disabled={probing}>
            {probing ? '探测中…' : '测试连通性'}
          </button>
          {up?.release_url ? (
            <a className="btn sm" href={up.release_url} target="_blank" rel="noreferrer noopener">
              打开 Release
            </a>
          ) : null}
          <button
            className="btn sm primary"
            onClick={() => setConfirmInstall(true)}
            disabled={!up || !up.can_install || busy.busy !== '' || up.installing}
          >
            {up?.installing ? '安装中…' : '立即更新'}
          </button>
        </div>
      </div>

      {q.error ? (
        <div className="notice bad">
          <span>状态读取失败：{errorText(q.error)}</span>
          <button className="btn sm" onClick={q.reload}>
            重试
          </button>
        </div>
      ) : null}

      {!d ? (
        q.error ? (
          <Box>
            <Empty title="无法读取运行状态" hint="确认后端服务是否正常，再点上方「重试」。" />
          </Box>
        ) : (
          <Box>
            <Skeleton rows={6} />
          </Box>
        )
      ) : (
        <>
          <StatsGrid>
            <Stat
              label="当前版本"
              value={stripV(d.version) || 'dev'}
              sub={d.login_required ? '已开启登录鉴权' : '未开启登录鉴权'}
            />
            <Stat
              label="最新版本"
              value={up?.latest_version ? stripV(up.latest_version) : '未知'}
              sub={up?.update_available ? '有可用更新' : up ? '已是最新' : '未启用更新检查'}
              tone={up?.update_available ? 'warn' : undefined}
            />
            <Stat
              label="运行时长"
              value={duration(d.uptime_sec)}
              sub={'启动于 ' + ago(new Date(Date.now() - d.uptime_sec * 1000).toISOString())}
            />
            <Stat
              label="更新状态"
              value={up ? (up.installing ? '安装中' : up.last_error ? '检查失败' : '正常') : '未配置'}
              sub={
                up?.last_error_kind
                  ? ERROR_KIND_LABEL[up.last_error_kind] ?? up.last_error_kind
                  : up?.last_error
                    ? '见下方错误'
                    : '通道就绪'
              }
              tone={up?.last_error ? 'bad' : undefined}
            />
          </StatsGrid>

          {up?.last_error ? (
            <div className="notice bad">
              <span>
                最近一次更新失败
                {up.last_error_kind ? '（' + (ERROR_KIND_LABEL[up.last_error_kind] ?? up.last_error_kind) + '）' : ''}
                ：{up.last_error}
                {up.last_error_kind && ERROR_ADVICE[up.last_error_kind] ? ' ｜ 建议：' + ERROR_ADVICE[up.last_error_kind] : ''}
              </span>
              <button className="btn sm" onClick={checkUpdate} disabled={busy.busy !== ''}>
                重试检查
              </button>
            </div>
          ) : null}
          {up?.install_blocked ? (
            <div className="notice warn">
              <span>当前无法自动安装：{up.install_blocked}</span>
            </div>
          ) : null}

          <Box
            title="版本与更新"
            sub={updateNote(up, d.version)}
            actions={up ? (up.update_available ? <Badge tone="warn">有可用更新</Badge> : <Badge tone="ok">已最新</Badge>) : null}
          >
            {up ? (
              <>
                <KV items={updateItems} />
                <p className="tiny muted">
                  通道：{channelText}
                  {up.last_error_kind && ERROR_ADVICE[up.last_error_kind]
                    ? ' ｜ 上次失败[' + up.last_error_kind + ']：' + ERROR_ADVICE[up.last_error_kind]
                    : ''}
                </p>
                {!up.can_install ? (
                  <p className="tiny muted">
                    当前环境不支持面板内自动安装，请手动替换二进制或使用容器镜像更新。
                  </p>
                ) : null}
              </>
            ) : (
              <Empty
                title="未启用更新检查"
                hint="后端未返回 updater 状态；如需面板内更新，请在下方通道设置里打开自动更新。"
              />
            )}
            {probe ? (
              <div style={{ marginTop: 12 }}>
                {probeRows().map((row) => (
                  <div className="row" key={row.label} style={{ gap: 8 }}>
                    <Badge tone={row.p.ok ? 'ok' : 'bad'}>{row.p.ok ? '通' : '失败'}</Badge>
                    <span className="small">
                      {row.label}：{row.p.ok ? '可达' : '失败[' + (row.p.kind || 'unknown') + ']'} ·{' '}
                      {num(Math.round(row.p.elapsed_ms))}ms
                      {row.p.status ? ' · HTTP ' + row.p.status : ''}
                      {row.p.error ? ' · ' + row.p.error : ''}
                    </span>
                  </div>
                ))}
                {probeRows()
                  .filter((row) => !row.p.ok && row.p.kind)
                  .map((row) => (
                    <p className="tiny warn-text" key={row.label + '-advice'}>
                      {row.label}建议：{ERROR_ADVICE[row.p.kind || ''] || '检查出站网络与代理配置。'}
                    </p>
                  ))}
              </div>
            ) : null}
            {up?.release_notes ? (
              <div style={{ marginTop: 12 }}>
                <p className="small muted">最新版本更新说明</p>
                <pre className="pre">{up.release_notes}</pre>
              </div>
            ) : null}
          </Box>

          <Box
            title="更新通道设置"
            sub="写入 config 的 update 段，保存后立即热生效；留空表示不显式设置"
            footer={
              dirty ? (
                <div className="row">
                  <span className="tiny muted">{num(dirtyPaths.length)} 项未保存</span>
                  <button className="btn sm" onClick={() => setEdits({})} disabled={saving}>
                    放弃修改
                  </button>
                  <button className="btn sm primary" onClick={saveSettings} disabled={saving}>
                    {saving ? '保存中…' : '保存更新设置'}
                  </button>
                </div>
              ) : null
            }
          >
            {!cfg ? (
              cfgQ.error ? (
                <div className="notice bad">
                  <span>读取配置失败：{errorText(cfgQ.error)}</span>
                  <button className="btn sm" onClick={() => cfgQ.reload()}>
                    重试
                  </button>
                </div>
              ) : (
                <Skeleton rows={4} />
              )
            ) : (
              <div className="formgrid">
                {UPDATE_FIELDS.map((f) => {
                  const managed = envManaged.has(f.path)
                  const v = read(f.path)
                  const hint = managed ? (f.hint ? f.hint + '；' : '') + '环境变量托管，保存不生效' : f.hint
                  return (
                    <Field key={f.path} label={managed ? f.label + '（托管）' : f.label} hint={hint}>
                      {f.kind === 'switch' ? (
                        <div className="row">
                          <Switch
                            checked={v === true}
                            disabled={managed}
                            onChange={(next) => write(f.path, next)}
                            label={f.label}
                          />
                          <span className="tiny muted">{v === true ? '启用' : '关闭'}</span>
                        </div>
                      ) : (
                        <input
                          className="input"
                          type={f.kind === 'time' ? 'time' : 'text'}
                          step={f.kind === 'time' ? 60 : undefined}
                          value={typeof v === 'string' ? v : ''}
                          placeholder={f.placeholder}
                          disabled={managed}
                          onChange={(e) => write(f.path, e.target.value)}
                        />
                      )}
                    </Field>
                  )
                })}
              </div>
            )}
          </Box>

          <div className="grid2">
            <Box title="运行环境" sub="来自 /panel/api/overview 与 /panel/api/config">
              <KV items={runtimeItems} />
            </Box>
            <Box title="关于本项目" sub="WorkBuddy / CodeBuddy 多账号网关与面板">
              <KV
                items={[
                  { k: '项目', v: 'workbuddy2api-panel' },
                  { k: '源码仓库', v: (
                    <a href="https://github.com/let233/workbuddy2api-panel" target="_blank" rel="noreferrer noopener">
                      let233/workbuddy2api-panel
                    </a>
                  ) },
                  { k: '问题反馈', v: (
                    <a href="https://github.com/let233/workbuddy2api-panel/issues" target="_blank" rel="noreferrer noopener">
                      GitHub Issues
                    </a>
                  ) },
                  { k: 'Release', v: (
                    <a href="https://github.com/let233/workbuddy2api-panel/releases" target="_blank" rel="noreferrer noopener">
                      Releases
                    </a>
                  ) },
                  { k: '许可证', v: 'MIT' },
                  { k: '版本口径', v: '二进制名即发布标签，形如 v2026.9.26.711；本地构建显示 dev' },
                ]}
              />
            </Box>
          </div>
        </>
      )}

      <ConfirmDialog
        open={confirmInstall}
        title="安装更新"
        message={
          '将下载并安装 ' +
          (up?.latest_version || '新版本') +
          '，服务会在安装完成后自动重启，期间请求会短暂中断。确认继续？'
        }
        confirmText="开始更新"
        danger
        busy={busy.busy === 'install'}
        onConfirm={installUpdate}
        onClose={() => setConfirmInstall(false)}
      />
    </div>
  )
}
