import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 开发代理：前端所有 /v1 请求转发到控制面（REST + WebSocket）。
// 生产环境可用 VITE_API_BASE 指向控制面地址，或由网关反向代理同源。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/v1': {
        target: process.env.VITE_API_BASE || 'http://127.0.0.1:18080',
        changeOrigin: true,
        ws: true,
      },
    },
  },
})
