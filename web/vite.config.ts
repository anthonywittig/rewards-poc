import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Dev server on :5173. Point VITE_API_BASE at the mock (:8082) or real API (:8081).
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
  },
})
