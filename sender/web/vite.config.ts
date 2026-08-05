import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The dev server proxies the API rather than the app calling a different origin.
//
// It means the browser code never needs to know where the API is: in development the proxy forwards
// /api to the local sender, and in production nginx serves the built assets and forwards the same
// paths to the same place. One set of relative URLs works in both, so there is no build-time
// configuration to get wrong and no CORS to arrange.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: process.env.OTP_SENDER_API ?? 'http://localhost:8080', changeOrigin: true },
      '/health': { target: process.env.OTP_SENDER_API ?? 'http://localhost:8080', changeOrigin: true },
    },
  },
  build: { outDir: 'dist', sourcemap: true },
})
