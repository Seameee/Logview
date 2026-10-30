import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// 前端只构建静态产物，由 Go 进程内嵌并通过白名单路由提供。
// 开发时用 `/api/stream` 代理到本地 Go 进程。
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // 产物进入 internal/server/dist 由构建脚本负责，这里保持默认 outDir。
    outDir: 'dist',
    emptyOutDir: true,
    // 关闭 sourcemap：产物会内嵌进镜像，不需要暴露源码映射。
    sourcemap: false,
    target: 'es2022',
    assetsInlineLimit: 4096,
  },
  server: {
    proxy: {
      '/api/stream': {
        target: 'http://127.0.0.1:7681',
        changeOrigin: false,
      },
    },
  },
})
