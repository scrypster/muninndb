import { defineConfig } from 'vite'
import { resolve } from 'path'
import { fileURLToPath } from 'url'
import { dirname } from 'path'

const __dirname = dirname(fileURLToPath(import.meta.url))

export default defineConfig({
  root: 'static',
  build: {
    outDir: resolve(__dirname, 'static/dist'),
    emptyOutDir: true,
    rollupOptions: {
      input: resolve(__dirname, 'static/css/app.css'),
      output: {
        assetFileNames: '[name][extname]',
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8475', changeOrigin: true },
      '/events': { target: 'http://localhost:8476', changeOrigin: true },
    },
  },
  test: {
    root: resolve(__dirname),  // look for tests in web/ not web/static/
    environment: 'node',
    globals: false,
  },
})
