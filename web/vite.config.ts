/// <reference types="vitest/config" />
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { fileURLToPath, URL } from 'node:url';

// Dev loop:
//   1. Run the Go daemon on 127.0.0.1:8090 (it prints a one-time password on startup).
//   2. In this directory run `npm run dev` (Vite serves the SPA on http://localhost:5173).
//   3. The dev server proxies `/api` and `/config.json` to the daemon, so the SPA talks to
//      the real backend same-origin during development. The daemon's own embedded build is
//      served same-origin in production, so no proxy is needed there.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // Emitted to web/dist - the Go daemon embeds this directory.
    outDir: 'dist',
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8090',
        changeOrigin: true,
      },
      '/config.json': {
        target: 'http://127.0.0.1:8090',
        changeOrigin: true,
      },
    },
  },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: true,
  },
});
