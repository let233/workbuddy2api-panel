import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath, URL } from 'node:url'

// 前端产物直接落到仓库根的 public/：Go 用 http.FileServer 映射该目录，
// 不再 go:embed 进二进制（前端改动无需重新编译 Go）。
export default defineConfig({
  plugins: [react()],
  base: '/panel/',
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: '../public',
    emptyOutDir: true,
    target: 'es2020',
    assetsDir: 'assets',
    sourcemap: false,
    // 产物名带内容指纹：配合 Go 侧 assets 目录的长缓存头，跨版本不会拿到旧脚本。
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name]-[hash].js',
        chunkFileNames: 'assets/[name]-[hash].js',
        assetFileNames: 'assets/[name]-[hash][extname]',
      },
    },
  },
  server: {
    port: 5273,
    proxy: { '/panel/api': { target: 'http://127.0.0.1:7863', changeOrigin: false } },
  },
})
