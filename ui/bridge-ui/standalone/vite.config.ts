import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))

// Standalone launcher for @kayushkin/bridge-ui.
//
//   npm start            → dev server, proxies /api/* to local backends
//   npm run build:standalone → static bundle in standalone/dist
//
// The alias points the package name at ./src so the launcher tracks source
// directly and needs no prior `npm run build`. Override the bridge target
// without the proxy via VITE_BRIDGE_BASE (e.g. an absolute URL).
export default defineConfig({
  root: here,
  plugins: [react()],
  resolve: {
    dedupe: ['react', 'react-dom', 'react-router', 'react-router-dom'],
    alias: {
      '@kayushkin/bridge-ui': resolve(here, '../src/index.ts'),
    },
  },
  server: {
    // Backends serve their own canonical paths (bridge-server handles
    // /sessions, skill-store handles /skills, …), so the prefix is stripped
    // before forwarding — mirroring dash-server's reverse proxies.
    proxy: {
      '/api/bridge': {
        target: 'http://localhost:8160',
        changeOrigin: true,
        ws: true,
        rewrite: (p) => p.replace(/^\/api\/bridge/, '') || '/',
      },
      '/api/skill-store': {
        target: 'http://localhost:8301',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api\/skill-store/, '') || '/',
      },
      '/api/tool-store': {
        target: 'http://localhost:8302',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api\/tool-store/, '') || '/',
      },
      '/api/principals': {
        target: 'http://127.0.0.1:8314',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api\/principals/, '') || '/',
      },
      '/api/bundle-store': {
        target: 'http://localhost:8307',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api\/bundle-store/, '') || '/',
      },
      '/api/repo-store': {
        target: 'http://localhost:8306',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api\/repo-store/, '') || '/',
      },
      '/api/model-store': {
        target: 'http://localhost:8155',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api\/model-store/, '') || '/',
      },
      '/api/work-graph': {
        target: 'http://127.0.0.1:8319',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api\/work-graph/, '') || '/',
      },
    },
  },
  build: {
    outDir: resolve(here, 'dist'),
    emptyOutDir: true,
  },
})
