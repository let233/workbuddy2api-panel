// charts.tsx 手写 SVG 图表（沿用原面板不引图表库的做法）：
// 折线 / 柱状 / 环形 / 迷你走势 / 100% 堆叠条。尺寸靠 ResizeObserver 量父容器，
// 保证文字不被等比缩放糊掉。颜色一律吃 CSS 变量，跟着主题走。
import { useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'

export const PALETTE = [
  'var(--accent)',
  'var(--ok)',
  'var(--warn)',
  'var(--bad)',
  '#8b5cf6',
  '#14b8a6',
  '#f472b6',
  '#facc15',
]

function useWidth<T extends HTMLElement>() {
  const ref = useRef<T | null>(null)
  const [w, setW] = useState(0)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    setW(el.clientWidth)
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) setW(e.contentRect.width)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  return [ref, w] as const
}

function niceMax(max: number): number {
  if (!isFinite(max) || max <= 0) return 1
  const exp = Math.floor(Math.log10(max))
  const base = Math.pow(10, exp)
  const n = max / base
  const step = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10
  return step * base
}

export type Series = {
  label: string
  color?: string
  points: (number | null)[]
}

export function LineChart({
  labels,
  series,
  height = 200,
  formatY = (n: number) => String(Math.round(n)),
  area = true,
  yTicks = 4,
  emptyText = '暂无数据',
}: {
  labels: string[]
  series: Series[]
  height?: number
  formatY?: (n: number) => string
  area?: boolean
  yTicks?: number
  emptyText?: string
}) {
  const [ref, w] = useWidth<HTMLDivElement>()
  const [hover, setHover] = useState<number | null>(null)

  const m = { l: 46, r: 12, t: 10, b: 24 }
  const iw = Math.max(10, w - m.l - m.r)
  const ih = Math.max(10, height - m.t - m.b)

  const n = labels.length
  const max = useMemo(() => {
    let mx = 0
    for (const s of series) for (const p of s.points) if (p != null && isFinite(p) && p > mx) mx = p
    return niceMax(mx)
  }, [series])

  const x = (i: number) => (n <= 1 ? m.l + iw / 2 : m.l + (iw * i) / (n - 1))
  const y = (v: number) => m.t + ih - (Math.max(0, v) / max) * ih

  if (n === 0 || series.length === 0) {
    return <div className="chart-empty">{emptyText}</div>
  }

  const ticks = Array.from({ length: yTicks + 1 }, (_, i) => (max * i) / yTicks)
  const showEvery = Math.max(1, Math.ceil(n / Math.max(3, Math.floor(iw / 62))))

  return (
    <div className="chart" ref={ref}>
      <svg
        width={Math.max(w, 10)}
        height={height}
        role="img"
        aria-label={series.map((s) => s.label).join('、')}
        onMouseLeave={() => setHover(null)}
        onMouseMove={(e) => {
          if (!n) return
          const rect = (e.currentTarget as SVGSVGElement).getBoundingClientRect()
          const px = e.clientX - rect.left
          const i = n <= 1 ? 0 : Math.round(((px - m.l) / iw) * (n - 1))
          setHover(Math.min(n - 1, Math.max(0, i)))
        }}
      >
        {ticks.map((t, i) => (
          <g key={i}>
            <line x1={m.l} x2={m.l + iw} y1={y(t)} y2={y(t)} stroke="var(--line-soft)" strokeWidth="1" />
            <text x={m.l - 8} y={y(t) + 3} textAnchor="end" fontSize="10" fill="var(--ink-3)">
              {formatY(t)}
            </text>
          </g>
        ))}

        {labels.map((lb, i) =>
          i % showEvery === 0 ? (
            <text key={i} x={x(i)} y={height - 6} textAnchor="middle" fontSize="10" fill="var(--ink-3)">
              {lb}
            </text>
          ) : null,
        )}

        {series.map((s, si) => {
          const color = s.color || PALETTE[si % PALETTE.length]
          const pts = s.points.map((v, i) => ({ v, i })).filter((p) => p.v != null) as { v: number; i: number }[]
          if (pts.length === 0) return null
          const d = pts.map((p, k) => (k === 0 ? 'M' : 'L') + x(p.i) + ' ' + y(p.v)).join(' ')
          const ad =
            area && pts.length > 1
              ? d + ' L' + x(pts[pts.length - 1].i) + ' ' + (m.t + ih) + ' L' + x(pts[0].i) + ' ' + (m.t + ih) + ' Z'
              : ''
          return (
            <g key={si}>
              {area && <path d={ad} fill={color} opacity="0.1" />}
              <path d={d} fill="none" stroke={color} strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />
            </g>
          )
        })}

        {hover != null && (
          <g pointerEvents="none">
            <line x1={x(hover)} x2={x(hover)} y1={m.t} y2={m.t + ih} stroke="var(--ink-3)" strokeDasharray="3 3" />
            {series.map((s, si) => {
              const v = s.points[hover]
              if (v == null) return null
              return (
                <circle
                  key={si}
                  cx={x(hover)}
                  cy={y(v)}
                  r="3"
                  fill={s.color || PALETTE[si % PALETTE.length]}
                  stroke="var(--surface)"
                  strokeWidth="1.5"
                />
              )
            })}
          </g>
        )}
      </svg>

      {hover != null && (
        <div
          className="chart-tip"
          style={{
            left: Math.min(Math.max(x(hover) - 60, 4), Math.max(4, w - 130)),
            top: 4,
          }}
        >
          <div className="lab">{labels[hover]}</div>
          {series.map((s, si) => (
            <div key={si} className="row gap1 v-center">
              <span className="swatch" style={{ background: s.color || PALETTE[si % PALETTE.length] }} />
              <span className="muted small">{s.label}</span>
              <span className="num small grow right">{s.points[hover] == null ? '-' : formatY(s.points[hover] as number)}</span>
            </div>
          ))}
        </div>
      )}

      {series.length > 1 && (
        <div className="chart-legend">
          {series.map((s, si) => (
            <span key={si} className="row gap1 v-center small muted">
              <span className="swatch" style={{ background: s.color || PALETTE[si % PALETTE.length] }} />
              {s.label}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

export function BarChart({
  items,
  height = 200,
  horizontal = false,
  format = (n: number) => String(Math.round(n)),
  emptyText = '暂无数据',
  tone = 'accent',
}: {
  items: { label: string; value: number; tone?: string }[]
  height?: number
  horizontal?: boolean
  format?: (n: number) => string
  emptyText?: string
  tone?: 'accent' | 'ok' | 'warn' | 'bad'
}) {
  const [ref, w] = useWidth<HTMLDivElement>()
  if (items.length === 0) return <div className="chart-empty">{emptyText}</div>

  if (horizontal) {
    const max = Math.max(...items.map((i) => i.value), 1)
    return (
      <div className="hbars" ref={ref}>
        {items.map((it, i) => (
          <div key={i} className="hbar">
            <span className="lbl truncate" title={it.label}>
              {it.label}
            </span>
            <span className="track">
              <i className={it.tone || tone} style={{ width: (Math.max(0, it.value) / max) * 100 + '%' }} />
            </span>
            <span className="val num">{format(it.value)}</span>
          </div>
        ))}
      </div>
    )
  }

  const m = { l: 46, r: 10, t: 10, b: 26 }
  const iw = Math.max(10, w - m.l - m.r)
  const ih = Math.max(10, height - m.t - m.b)
  const max = niceMax(Math.max(...items.map((i) => i.value), 0))
  const bw = Math.max(3, Math.min(38, (iw / Math.max(1, items.length)) * 0.62))
  const step = iw / Math.max(1, items.length)
  const showEvery = Math.max(1, Math.ceil(items.length / Math.max(3, Math.floor(iw / 62))))

  return (
    <div className="chart" ref={ref}>
      <svg width={Math.max(w, 10)} height={height} role="img" aria-label="柱状图">
        {[0, 0.25, 0.5, 0.75, 1].map((t, i) => {
          const yy = m.t + ih - ih * t
          return (
            <g key={i}>
              <line x1={m.l} x2={m.l + iw} y1={yy} y2={yy} stroke="var(--line-soft)" />
              <text x={m.l - 8} y={yy + 3} textAnchor="end" fontSize="10" fill="var(--ink-3)">
                {format(max * t)}
              </text>
            </g>
          )
        })}
        {items.map((it, i) => {
          const h = max > 0 ? (Math.max(0, it.value) / max) * ih : 0
          const cx = m.l + step * i + step / 2
          return (
            <g key={i}>
              <rect
                x={cx - bw / 2}
                y={m.t + ih - h}
                width={bw}
                height={Math.max(0, h)}
                rx="3"
                fill={it.tone ? 'var(--' + it.tone + ')' : 'var(--accent)'}
                opacity="0.9"
              >
                <title>{it.label + '：' + format(it.value)}</title>
              </rect>
              {i % showEvery === 0 && (
                <text x={cx} y={height - 8} textAnchor="middle" fontSize="10" fill="var(--ink-3)">
                  {it.label}
                </text>
              )}
            </g>
          )
        })}
      </svg>
    </div>
  )
}

export function Donut({
  parts,
  size = 160,
  thickness = 18,
  center,
  emptyText = '暂无数据',
}: {
  parts: { label: string; value: number; color?: string }[]
  size?: number
  thickness?: number
  center?: ReactNode
  emptyText?: string
}) {
  const total = parts.reduce((a, b) => a + Math.max(0, b.value), 0)
  const r = (size - thickness) / 2
  const c = 2 * Math.PI * r
  if (total <= 0) return <div className="chart-empty">{emptyText}</div>

  let acc = 0
  return (
    <div className="donut-wrap">
      <div className="donut" style={{ width: size, height: size }}>
        <svg width={size} height={size} role="img" aria-label="占比图">
          <g transform={'translate(' + size / 2 + ',' + size / 2 + ') rotate(-90)'}>
            <circle r={r} fill="none" stroke="var(--line-soft)" strokeWidth={thickness} />
            {parts.map((p, i) => {
              const frac = Math.max(0, p.value) / total
              const dash = frac * c
              const el = (
                <circle
                  key={i}
                  r={r}
                  fill="none"
                  stroke={p.color || PALETTE[i % PALETTE.length]}
                  strokeWidth={thickness}
                  strokeDasharray={dash + ' ' + (c - dash)}
                  strokeDashoffset={-acc * c}
                  strokeLinecap="butt"
                >
                  <title>{p.label + '：' + p.value}</title>
                </circle>
              )
              acc += frac
              return el
            })}
          </g>
        </svg>
        {center && <div className="donut-center">{center}</div>}
      </div>
      <ul className="donut-legend">
        {parts.map((p, i) => (
          <li key={i}>
            <span className="swatch" style={{ background: p.color || PALETTE[i % PALETTE.length] }} />
            <span className="grow truncate">{p.label}</span>
            <span className="num small muted">{((Math.max(0, p.value) / total) * 100).toFixed(1)}%</span>
          </li>
        ))}
      </ul>
    </div>
  )
}

export function Sparkline({ points, height = 30, tone = 'accent' }: { points: number[]; height?: number; tone?: 'accent' | 'ok' | 'warn' | 'bad' }) {
  const [ref, w] = useWidth<HTMLDivElement>()
  const clean = points.filter((p) => isFinite(p))
  if (clean.length < 2) return <div className="spark" ref={ref} />
  const max = Math.max(...clean, 1)
  const min = Math.min(...clean, 0)
  const span = max - min || 1
  const W = Math.max(w, 20)
  const d = clean
    .map((v, i) => {
      const px = (W * i) / (clean.length - 1)
      const py = height - ((v - min) / span) * (height - 2) - 1
      return (i === 0 ? 'M' : 'L') + px.toFixed(1) + ' ' + py.toFixed(1)
    })
    .join(' ')
  return (
    <div className="spark" ref={ref}>
      <svg width={W} height={height} aria-hidden="true">
        <path d={d} fill="none" stroke={'var(--' + tone + ')'} strokeWidth="1.5" strokeLinejoin="round" />
      </svg>
    </div>
  )
}

export function StackBar({
  parts,
  height = 10,
  showLegend = false,
}: {
  parts: { label: string; value: number; color?: string }[]
  height?: number
  showLegend?: boolean
}) {
  const total = parts.reduce((a, b) => a + Math.max(0, b.value), 0)
  if (total <= 0) return <div className="bar" style={{ height }} />
  return (
    <div className="stackbar-wrap">
      <div className="stackbar" style={{ height }}>
        {parts.map((p, i) => (
          <i
            key={i}
            style={{
              width: (Math.max(0, p.value) / total) * 100 + '%',
              background: p.color || PALETTE[i % PALETTE.length],
            }}
            title={p.label + '：' + p.value}
          />
        ))}
      </div>
      {showLegend && (
        <div className="chart-legend">
          {parts.map((p, i) => (
            <span key={i} className="row gap1 v-center small muted">
              <span className="swatch" style={{ background: p.color || PALETTE[i % PALETTE.length] }} />
              {p.label}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}
