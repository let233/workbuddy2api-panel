// format.ts 纯格式化函数：任何显示口径的修正都在这里改，页面只调用。
export function num(v: unknown): string {
  const n = typeof v === 'number' ? v : Number(v)
  if (!isFinite(n)) return '-'
  return n.toLocaleString('zh-CN')
}

/** 积分/额度：绝对值小时保留 2 位，大时按千分位。 */
export function credits(v: unknown): string {
  const n = typeof v === 'number' ? v : Number(v)
  if (!isFinite(n)) return '-'
  const abs = Math.abs(n)
  if (abs === 0) return '0'
  if (abs < 10) return n.toFixed(abs < 1 ? 2 : 1).replace(/\.0$/, '')
  return n.toLocaleString('zh-CN', { maximumFractionDigits: 0 })
}

/** Token 量：>=1e9 用 B、>=1e6 用 M、>=1e4 用 K，保留一位有效小数。 */
export function tok(v: unknown): string {
  const n = typeof v === 'number' ? v : Number(v)
  if (!isFinite(n) || n === 0) return '0'
  const a = Math.abs(n)
  if (a >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B'
  if (a >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M'
  if (a >= 1e4) return (n / 1e3).toFixed(0) + 'K'
  return n.toLocaleString('zh-CN')
}

export function pct(v: unknown, digits = 1): string {
  const n = typeof v === 'number' ? v : Number(v)
  if (!isFinite(n)) return '-'
  return n.toFixed(digits).replace(/\.0+$/, '') + '%'
}

function pad(n: number, w = 2): string {
  return String(n).padStart(w, '0')
}

/** 本地时间 HH:MM:SS */
export function clock(v?: string | number | Date | null): string {
  if (!v || isZeroTime(v)) return '-'
  const d = parseDate(v)
  if (!d) return String(v)
  return pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds())
}

function parseDate(v?: string | number | Date | null): Date | null {
  if (v === null || v === undefined || v === '') return null
  const d = v instanceof Date ? v : new Date(v)
  return isNaN(d.getTime()) ? null : d
}

/**
 * Go 的 time.Time 零值（0001-01-01T00:00:00Z）序列化后是个合法字符串，
 * 但落到前端会变成 1970 之前的时间戳；这类值必须当「没有」处理，
 * 否则会渲染出「01-01 08:05（739931 天前）」这种鬼东西。
 */
export function isZeroTime(v?: string | number | Date | null): boolean {
  const d = parseDate(v)
  return d !== null && d.getTime() <= 0
}

/** 本地时间 MM-DD HH:MM */
export function shortTime(v?: string | number | Date | null): string {
  if (!v || isZeroTime(v)) return '-'
  const d = parseDate(v)
  if (!d) return String(v)
  return pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes())
}

/** UTC+8 自然日：上游到期口径统一 +08，不能用浏览器本地时区截日期。 */
export function cstDate(v?: string | number | Date | null): string {
  if (!v || isZeroTime(v)) return '-'
  const d = parseDate(v)
  if (!d) return String(v)
  const shifted = new Date(d.getTime() + 8 * 3600 * 1000)
  return shifted.getUTCFullYear() + '-' + pad(shifted.getUTCMonth() + 1) + '-' + pad(shifted.getUTCDate())
}

/** 相对时间：刚刚 / N 分钟前 / N 小时前 / N 天前 */
export function ago(v?: string | number | Date | null): string {
  if (!v || isZeroTime(v)) return '-'
  const d = parseDate(v)
  if (!d) return '-'
  const ms = Date.now() - d.getTime()
  if (!isFinite(ms)) return '-'
  if (ms < 0) return '刚刚'
  const s = Math.floor(ms / 1000)
  if (s < 60) return '刚刚'
  const m = Math.floor(s / 60)
  if (m < 60) return m + ' 分钟前'
  const h = Math.floor(m / 60)
  if (h < 24) return h + ' 小时前'
  return Math.floor(h / 24) + ' 天前'
}

/** 秒 → 1天2小时 / 3小时5分 */
export function duration(sec: unknown): string {
  const n = Number(sec)
  if (!isFinite(n) || n <= 0) return '-'
  const d = Math.floor(n / 86400)
  const h = Math.floor((n % 86400) / 3600)
  const m = Math.floor((n % 3600) / 60)
  if (d > 0) return d + '天' + (h ? h + '小时' : '')
  if (h > 0) return h + '小时' + (m ? m + '分' : '')
  if (m > 0) return m + '分'
  return Math.floor(n) + '秒'
}

export function bytes(v: unknown): string {
  const n = Number(v)
  if (!isFinite(n) || n <= 0) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let x = n
  while (x >= 1024 && i < u.length - 1) {
    x /= 1024
    i++
  }
  return (i === 0 ? x.toFixed(0) : x.toFixed(1)) + ' ' + u[i]
}

/** 毫秒时间戳/秒时间戳都能吃：小于 1e12 视为秒。 */
export function ts(v: unknown): number {
  const n = Number(v)
  if (!isFinite(n) || n <= 0) return 0
  return n < 1e12 ? n * 1000 : n
}

export function clamp(v: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, v))
}
