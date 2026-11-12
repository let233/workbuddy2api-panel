# 前端重构约定（web/）

React 18 + TypeScript + Vite，零 UI 库、零图表库（图表是手写 SVG）。产物输出到仓库根 `public/`，
由 Go 侧 `http.FileServer` 映射到 `/panel/`，不再 go:embed。

## 目录

```
web/src/
  main.tsx              入口：先定主题，再挂 Provider（Theme > Toast > Session > Refresh > App）
  App.tsx               会话探测 → 登录层 or 外壳；14 个视图用 React.lazy 分包
  lib/api.ts            api.get/post/put/del/form/downloadUrl、ApiError、errorText、qs
  lib/format.ts         num/credits/tok/pct/clock/shortTime/cstDate/ago/duration/bytes/ts/clamp
  lib/hooks.ts          useQuery / useBusy / useDebounced / useMediaQuery / useNow / useSelection
  lib/session.tsx       useSession（login/logout/changePassword/mustChange/defaultCredentials）
  lib/toast.tsx         useToast（ok/err/warn/info）
  lib/refresh.tsx       useRefresh（tick/refresh/interval）、useAutoRefresh(pollMs)
  lib/router.tsx        useRoute() → {name, query}；navigate(name, query?)
  lib/theme.tsx         useTheme（mode/resolved/setMode/cycle）
  components/ui.tsx     Box/Stat/StatsGrid/Badge/Dot/Bar/Empty/Skeleton/KV/Field/Switch/
                        Segmented/Tabs/Modal/ConfirmDialog/CopyText/Pager/Pre
  components/DataTable.tsx  列定义 + 排序 + 多选 + 窄屏自动变卡片
  components/charts.tsx     LineChart/BarChart/Donut/Sparkline/StackBar + PALETTE
  components/AppShell.tsx   侧栏/顶栏/账号菜单/改密弹层
  views/*.tsx           每个视图一个文件，默认导出组件（`export default function Xxx()`）
  views/views.css       页面级样式（图表、页面头、日志流、表单网格等），已被 App.tsx 引入
```

## 取数

```tsx
const { tick } = useRefresh()              // 顶栏刷新按钮 / 自动刷新
const q = useQuery<Resp>((signal) => api.get('/accounts', { page, size }, signal), [page, size, tick])
```

- `useQuery` **必须**把 `tick` 放进 deps，否则顶栏刷新按钮对它无效。
- 失败态：`q.error` 用 `errorText(q.error)` 显示；首次加载用 `q.first` 判断是否显示 Skeleton。
- 轮询：`useQuery(fn, deps, { poll: 5000 })`（页面不可见自动暂停）。日志/事件页用轮询。
- 写操作：`api.post/put/del` → 成功后 `toast.ok('...')` + `q.reload()`；失败 `toast.err(errorText(e))`。
- 401 由 api 层统一处理，视图里不用管。

## 样式

只用已有类名，不写内联颜色、不引新 CSS 文件：

- 容器：`.box`（`<Box title sub actions footer flush>`）、`.page`、`.pagehead`(h2 + .lead + .acts)、`.grid2`、`.grid3`、`.stack`、`.row`、`.toolbar`、`.filters`、`.formgrid`(+`.span2`)
- 按钮：`.btn` + `primary/ghost/danger/ok/sm/lg/icon/block`；触摸目标移动端已放大
- 表格：用 `<DataTable>`，不要手写 `<table>`
- 指标：`<Stat label value sub tone>`，色调 `ok/warn/bad/accent`
- 其他：`.badge`、`.chip`、`.empty`、`.notice`(ok/warn/bad)、`.pre`、`.kv`、`.feed`、`.mono`、`.muted`、`.small`、`.tiny`
- 移动端：不到万不得已不写新 @media；DataTable 已自动转卡片

## 交互红线

- 破坏性操作走 `<ConfirmDialog>`，禁止 `window.confirm/alert/prompt`。
- 表单弹层用 `<Modal wide|narrow>`；窄屏自动变底部 sheet。
- 每个视图必须有：加载态（Skeleton）、空态（Empty，带一句下一步提示）、错误态（notice bad + 重试按钮）。
- 长列表要分页或限高滚动，不要把整个页面撑到几千像素。
- 文案中文、直白，不写「请点击此处」这类废话。

## 后端接口

所有路由在 `internal/panel/panel.go` 的 `routes()`（约 283-377 行），字段名以对应 handler 的
`writeJSON(... map[string]any{...})` 为准——**不要凭猜**，读 handler 再写类型。成功响应直接是数据，
失败统一 `{ok:false,error:"..."}`（api.ts 已把它变成 ApiError）。

## TypeScript

- `strict` + `noUnusedLocals`：写完全部 refactor（`npm run typecheck` 必须零错误）。
- 接口类型写在各自视图文件顶部，用 `type Xxx = {...}`，字段用普通 snake_case（后端原样）。
- 不用 `any`；不确定的字段用 `unknown` 再窄化。
