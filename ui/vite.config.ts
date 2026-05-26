import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  // GitHub Pages serves from /parley/ — no effect on the dev proxy
  base: '/parley/',
  plugins: [react()],
  server: {
    proxy: {
      '/posts': 'http://localhost:18080',
      '/openapi.json': 'http://localhost:18080',
    }
  }
})
