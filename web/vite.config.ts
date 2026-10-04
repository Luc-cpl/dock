import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  build: {
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (/\/node_modules\/(react|react-dom|scheduler|react-is)\//.test(id)) return 'react'
          if (/\/node_modules\/(?:@mui|@emotion)\//.test(id)) return 'mui'
        },
      },
    },
  },
  server: {
    proxy: { '/api': 'http://127.0.0.1:9080' },
  },
})
