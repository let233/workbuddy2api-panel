// nav.ts 导航定义：分组 + 图标名，AppShell 与 App 共用一份。
export type NavIcon =
  | 'gauge'
  | 'users'
  | 'clock'
  | 'chart'
  | 'key'
  | 'box'
  | 'trend'
  | 'tasks'
  | 'cpu'
  | 'net'
  | 'settings'
  | 'feed'
  | 'list'
  | 'info'

export type NavItem = {
  name: string
  label: string
  icon: NavIcon
  group: string
  desc: string
}

export const NAV: NavItem[] = [
  { name: 'overview', label: '概览', icon: 'gauge', group: '总览', desc: '账号池、密钥与近 24 小时请求的整体状态' },
  { name: 'accounts', label: '账号池', icon: 'users', group: '总览', desc: '管理上游账号、刷新 token、批量导入与探测' },
  { name: 'expiry', label: '积分到期', icon: 'clock', group: '总览', desc: '按到期时间排布各账号积分，提前发现要过期的额度' },

  { name: 'usage', label: '用量', icon: 'chart', group: '用量与账单', desc: '按账号 / 密钥 / 模型拆分请求量与 token 消耗' },
  { name: 'balhist', label: '余额趋势', icon: 'trend', group: '用量与账单', desc: '余额与积分的历史快照曲线' },
  { name: 'packages', label: '积分构成', icon: 'box', group: '用量与账单', desc: '套餐积分与赠送积分的构成占比' },

  { name: 'apikeys', label: 'API Key', icon: 'key', group: '资源', desc: '签发、限额与吊销下发给客户端的密钥' },
  { name: 'models', label: '模型与档位', icon: 'cpu', group: '资源', desc: '可用模型、档位映射与实际定价' },
  { name: 'proxies', label: '代理池', icon: 'net', group: '资源', desc: '出口代理的可用性、延迟与绑定账号数' },

  { name: 'taskscenter', label: '任务中心', icon: 'tasks', group: '运维', desc: '后台任务（签到、刷新、探测）的执行记录' },
  { name: 'events', label: '请求事件', icon: 'feed', group: '运维', desc: '逐条请求事件与失败原因，支持按状态筛选' },
  { name: 'logs', label: '运行日志', icon: 'list', group: '运维', desc: '服务端运行日志，按级别与关键字查看' },
  { name: 'config', label: '配置', icon: 'settings', group: '运维', desc: '运行参数、模型档位与安全设置' },
  { name: 'about', label: '关于', icon: 'info', group: '运维', desc: '版本、构建信息与开源许可' },
]

export const NAV_GROUPS: string[] = Array.from(new Set(NAV.map((n) => n.group)))

export function navItem(name: string): NavItem | undefined {
  return NAV.find((n) => n.name === name)
}

export const DEFAULT_ROUTE = 'overview'
