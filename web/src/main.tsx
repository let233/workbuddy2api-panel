// main.tsx 启动：先定主题（避免刷新闪一下），再挂 Provider 树。
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { bootstrapTheme, ThemeProvider } from './lib/theme'
import { ToastProvider } from './lib/toast'
import { SessionProvider } from './lib/session'
import { RefreshProvider } from './lib/refresh'
import './styles/tokens.css'
import './styles/base.css'
import './styles/components.css'

bootstrapTheme()

const root = document.getElementById('root')
if (!root) throw new Error('#root 不存在')

createRoot(root).render(
  <StrictMode>
    <ThemeProvider>
      <ToastProvider>
        <SessionProvider>
          <RefreshProvider>
            <App />
          </RefreshProvider>
        </SessionProvider>
      </ToastProvider>
    </ThemeProvider>
  </StrictMode>,
)
