import { useState } from 'react'
import { api, errorText } from '../lib/api'
import { useBusy, useQuery } from '../lib/hooks'
import { useRefresh } from '../lib/refresh'
import { useToast } from '../lib/toast'
import { ago, duration, num, shortTime } from '../lib/format'
import { Badge, Box, ConfirmDialog, Empty, KV, Skeleton, Stat, StatsGrid } from '../components/ui'

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
  container?: string
  install_blocked?: string
  install_mode?: string
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

const ERROR_KIND_LABEL: Record<string, string> = {
  dns: 'DNS 解析失败',
  connect: '连接失败',
  tls: 'TLS 握手失败',
  'header-timeout': '首包超时',
  'body-stall': '响应体停滞',
  checksum: '校验和不匹配',
  http: 'HTTP 错误',
  unknown: '未知原因',
}

const INSTALL_MODE_LABEL: Record<string, string> = {
  helper: '辅助进程替换',
  'in-place': '原地替换',
}

export default function About() {
  const { tick } = useRefresh()
  const toast = useToast()
  const busy = useBusy()
  const [confirmInstall, setConfirmInstall] = useState(false)

  const q = useQuery<Overview>((signal) => api.get<Overview>('/overview', undefined, signal), [tick], { poll: 15000 })

  const d = q.data
  const up = d?.update ?? null

  const checkUpdate = () => {
    void busy.run('check', async () => {
      try {
        const s = await api.post<UpdateStatus>('/update/check')
        if (s.update_available) {
          toast.ok(`发现新版本 ${s.latest_version}`)
        } else {
          toast.ok('已是最新版本')
        }
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

  const runtimeItems = d
    ? [
        { k: '面板版本', v: d.version || 'dev', mono: true },
        { k: '运行时长', v: `${duration(d.uptime_sec)}（${num(d.uptime_sec)} 秒）`, mono: true },
        { k: 'Redis 模式', v: d.redis_mode || '未配置（纯内存）', mono: true },
        {
          k: '面板刷新间隔',
          v: d.poll_interval_sec > 0 ? `${num(d.poll_interval_sec)} 秒` : '前端默认（5 秒）',
        },
        { k: '登录鉴权', v: d.login_required ? '已开启' : '未开启' },
        { k: '粘性会话', v: `${num(d.sticky_sessions)} 个在绑会话` },
        {
          k: '账号状态',
          v: `共 ${num(d.total)} · 健康 ${num(d.healthy)} · 冷却 ${num(d.cooling)} · 暂停 ${num(d.paused)} · 禁用 ${num(
            d.disabled,
          )}`,
        },
        { k: '额度合计', v: `${num(d.credits_sum)}` },
      ]
    : []

  const buildItems = [
    { k: 'Go 版本', v: '接口未暴露（需后端补充 build info）' },
    { k: '构建时间', v: '接口未暴露（本地构建显示 dev）' },
    { k: 'Commit', v: '接口未暴露（版本号即发布标签）' },
  ]

  const updateItems = up
    ? [
        { k: '更新通道', v: up.channel || 'stable', mono: true },
        { k: '当前版本', v: up.current_version || d?.version || 'dev', mono: true },
        { k: '最新版本', v: up.latest_version || '未知', mono: true },
        { k: '最近检查', v: up.checked_at ? `${shortTime(up.checked_at)}（${ago(up.checked_at)}）` : '尚未检查' },
        { k: '自动更新', v: up.auto_enabled ? `每天 ${up.auto_time || '-'}` : '已关闭' },
        { k: '代理', v: up.proxy_host || '未设置', mono: true },
        { k: '镜像前缀', v: up.mirror || '官方源', mono: true },
        { k: '安装方式', v: up.install_mode ? INSTALL_MODE_LABEL[up.install_mode] ?? up.install_mode : '未知' },
        { k: '运行环境', v: up.container ? `容器（${up.container}）` : '裸机' },
        { k: '可安装', v: up.can_install ? '是' : '否' },
      ]
    : []

  return (
    <div className="page">
      <div className="pagehead">
        <div>
          <h2>关于</h2>
          <p className="lead">版本、运行状态与更新通道</p>
        </div>
        <div className="acts">
          <button className="btn sm" onClick={checkUpdate} disabled={busy.busy !== ''}>
            {busy.busy === 'check' ? '检查中…' : '检查更新'}
          </button>
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
            <Stat label="当前版本" value={d.version || 'dev'} sub={d.login_required ? '已开启登录鉴权' : '未开启登录鉴权'} />
            <Stat
              label="最新版本"
              value={up?.latest_version || '未知'}
              sub={up?.update_available ? '有可用更新' : up ? '已是最新' : '未启用更新检查'}
              tone={up?.update_available ? 'warn' : undefined}
            />
            <Stat label="运行时长" value={duration(d.uptime_sec)} sub={`启动于 ${ago(new Date(Date.now() - d.uptime_sec * 1000).toISOString())}`} />
            <Stat
              label="更新状态"
              value={up ? (up.installing ? '安装中' : up.last_error ? '检查失败' : '正常') : '未配置'}
              sub={up?.last_error_kind ? ERROR_KIND_LABEL[up.last_error_kind] ?? up.last_error_kind : up?.last_error ? '见下方错误' : '通道就绪'}
              tone={up?.last_error ? 'bad' : undefined}
            />
          </StatsGrid>

          {up?.last_error ? (
            <div className="notice bad">
              <span>
                最近一次更新检查失败
                {up.last_error_kind ? `（${ERROR_KIND_LABEL[up.last_error_kind] ?? up.last_error_kind}）` : ''}：
                {up.last_error}
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
          {up?.update_available && up.release_notes ? (
            <Box title={`版本 ${up.latest_version} 更新说明`}>
              <pre className="pre">{up.release_notes}</pre>
            </Box>
          ) : null}

          <div className="grid2">
            <Box title="运行时" sub="来自 /panel/api/overview">
              <KV items={runtimeItems} />
            </Box>
            <Box title="构建信息" sub="后端未暴露这些字段，只能显示占位">
              <KV items={buildItems} />
              <p className="tiny muted">
                版本号即发布标签（形如 v2026.9.26.711）；本地构建固定显示 dev。
              </p>
            </Box>
            <Box
              title="更新检查"
              sub={up ? '来自后端 updater 状态' : '更新检查未启用'}
              className="grid-wide"
              actions={up?.update_available ? <Badge tone="warn">有可用更新</Badge> : <Badge tone="ok">已最新</Badge>}
            >
              {up ? (
                <>
                  <KV items={updateItems} />
                  {!up.can_install ? (
                    <p className="tiny muted">当前环境不支持面板内自动安装，请手动替换二进制或使用容器镜像更新。</p>
                  ) : null}
                </>
              ) : (
                <Empty title="未启用更新检查" hint="后端未返回 updater 状态；如需面板内更新，请在配置里打开 update 相关选项。" />
              )}
            </Box>
          </div>
        </>
      )}

      <ConfirmDialog
        open={confirmInstall}
        title="安装更新"
        message={`将下载并安装 ${up?.latest_version || '新版本'}，服务会在安装完成后自动重启，期间请求会短暂中断。确认继续？`}
        confirmText="开始更新"
        danger
        busy={busy.busy === 'install'}
        onConfirm={installUpdate}
        onClose={() => setConfirmInstall(false)}
      />
    </div>
  )
}
