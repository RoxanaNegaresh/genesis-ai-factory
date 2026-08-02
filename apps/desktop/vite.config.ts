import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'

// Tauri serves the built assets from the filesystem, so the base must be
// relative; an absolute base breaks asset resolution in the packaged app.
export default defineConfig({
  plugins: [react()],
  base: './',
  resolve: { alias: { '@': path.resolve(__dirname, './src') } },
  server: { port: 1420, strictPort: true },
  build: { target: 'es2022', outDir: 'dist', sourcemap: true, emptyOutDir: true },
})
