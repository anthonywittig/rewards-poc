import { defineConfig, loadEnv } from 'vite'
import react from '@vitejs/plugin-react'

// Dev server on :5173 by default; `make web` passes WEB_PORT from the stack's
// env file so a second stack serves elsewhere. Browser calls stay same-origin
// (/api/…) and Vite proxies to the Go API — avoids CORS (the API does not send
// CORS headers).
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  const proxyTarget = env.VITE_API_PROXY_TARGET || 'http://localhost:8081'

  return {
    plugins: [react()],
    server: {
      port: Number(env.WEB_PORT) || 5173,
      strictPort: true,
      proxy: {
        '/api': { target: proxyTarget, changeOrigin: true },
        '/healthz': { target: proxyTarget, changeOrigin: true },
      },
    },
  }
})
