// DataTable.tsx 表格：PC 端是普通表，窄屏自动降级成卡片列表（靠 CSS + data-label 实现，
// 不用 JS 分叉渲染，滚动与选中状态天然一致）。
import { useMemo, useState, type ReactNode } from 'react'
import { Empty, Skeleton } from './ui'

export type Column<T> = {
  key: string
  title: ReactNode
  render?: (row: T, index: number) => ReactNode
  /** 列宽（CSS 值），不写就自适应 */
  width?: string
  align?: 'left' | 'right' | 'center'
  mono?: boolean
  /** 提供即可排序；返回可比较值 */
  sortValue?: (row: T) => string | number
  /** 移动端卡片里藏掉这一列（比如纯装饰列） */
  hideMobile?: boolean
  className?: string
}

type Props<T> = {
  columns: Column<T>[]
  rows: T[]
  rowKey: (row: T) => string
  loading?: boolean
  /** 空态文案 */
  empty?: ReactNode
  emptyHint?: ReactNode
  selectable?: boolean
  selected?: Set<string>
  onToggle?: (key: string, next: boolean) => void
  onToggleAll?: (next: boolean, keys: string[]) => void
  onRowClick?: (row: T) => void
  rowClass?: (row: T) => string
  /** 初始排序字段 */
  initialSort?: { key: string; dir: 'asc' | 'desc' }
  /** 表格上方说明/工具栏 */
  caption?: ReactNode
}

function labelOf(node: ReactNode, fallback: string): string {
  return typeof node === 'string' || typeof node === 'number' ? String(node) : fallback
}

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  loading = false,
  empty = '暂无数据',
  emptyHint,
  selectable = false,
  selected,
  onToggle,
  onToggleAll,
  onRowClick,
  rowClass,
  initialSort,
  caption,
}: Props<T>) {
  const [sortKey, setSortKey] = useState<string>(initialSort?.key || '')
  const [dir, setDir] = useState<'asc' | 'desc'>(initialSort?.dir || 'desc')

  const sorted = useMemo(() => {
    const col = columns.find((c) => c.key === sortKey)
    if (!col?.sortValue) return rows
    const arr = rows.slice().sort((a, b) => {
      const va = col.sortValue!(a)
      const vb = col.sortValue!(b)
      if (typeof va === 'number' && typeof vb === 'number') return va - vb
      return String(va).localeCompare(String(vb), 'zh-CN')
    })
    return dir === 'asc' ? arr : arr.reverse()
  }, [rows, columns, sortKey, dir])

  const keys = useMemo(() => rows.map(rowKey), [rows, rowKey])
  const allChecked = selectable && keys.length > 0 && keys.every((k) => selected?.has(k))

  const toggleSort = (key: string) => {
    if (sortKey === key) setDir(dir === 'asc' ? 'desc' : 'asc')
    else {
      setSortKey(key)
      setDir('desc')
    }
  }

  if (loading && rows.length === 0) {
    return (
      <div className="tbl-state">
        <Skeleton rows={4} />
      </div>
    )
  }

  if (rows.length === 0) {
    return (
      <div className="tbl-state">
        <Empty title={empty} hint={emptyHint} />
      </div>
    )
  }

  return (
    <div className="tbl-wrap">
      {caption && <div className="tbl-caption">{caption}</div>}
      <table className="dtable">
        <thead>
          <tr>
            {selectable && (
              <th className="sel">
                <input
                  type="checkbox"
                  aria-label="全选当前页"
                  checked={Boolean(allChecked)}
                  onChange={(e) => onToggleAll?.(e.target.checked, keys)}
                />
              </th>
            )}
            {columns.map((c) => {
              const sortable = Boolean(c.sortValue)
              const on = sortKey === c.key
              return (
                <th
                  key={c.key}
                  style={c.width ? { width: c.width } : undefined}
                  className={(c.align ? 'ta-' + c.align : '') + (sortable ? ' sortable' : '')}
                  aria-sort={on ? (dir === 'asc' ? 'ascending' : 'descending') : undefined}
                >
                  {sortable ? (
                    <button type="button" className={'th-sort' + (on ? ' on' : '')} onClick={() => toggleSort(c.key)}>
                      <span>{c.title}</span>
                      <span className="arrow" aria-hidden="true">
                        {on ? (dir === 'asc' ? '▲' : '▼') : '⇅'}
                      </span>
                    </button>
                  ) : (
                    c.title
                  )}
                </th>
              )
            })}
          </tr>
        </thead>
        <tbody>
          {sorted.map((row, i) => {
            const k = rowKey(row)
            const on = selected?.has(k) ?? false
            return (
              <tr
                key={k}
                className={(on ? 'on ' : '') + (rowClass?.(row) || '')}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
                data-clickable={onRowClick ? '1' : undefined}
              >
                {selectable && (
                  <td className="sel" onClick={(e) => e.stopPropagation()}>
                    <input
                      type="checkbox"
                      aria-label="选择该行"
                      checked={on}
                      onChange={(e) => onToggle?.(k, e.target.checked)}
                    />
                  </td>
                )}
                {columns.map((c) => (
                  <td
                    key={c.key}
                    data-label={labelOf(c.title, c.key)}
                    className={
                      (c.align ? 'ta-' + c.align + ' ' : '') +
                      (c.mono ? 'mono ' : '') +
                      (c.hideMobile ? 'hide-mobile ' : '') +
                      (c.className || '')
                    }
                    style={c.width ? { width: c.width } : undefined}
                  >
                    {c.render ? c.render(row, i) : String((row as Record<string, unknown>)[c.key] ?? '-')}
                  </td>
                ))}
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}
