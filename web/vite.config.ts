import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

// Dev server on :5173. Browser calls stay same-origin (/api/…) and Vite proxies
// to the Go API — avoids CORS (the API does not send CORS headers).
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const proxyTarget = env.VITE_API_PROXY_TARGET || 'http://localhost:8081'

  return {
    plugins: [react()],
    server: {
      port: 5173,
      strictPort: true,
      proxy: {
        '/api': { target: proxyTarget, changeOrigin: true },
        '/healthz': { target: proxyTarget, changeOrigin: true },
      },
    },
  }
})
