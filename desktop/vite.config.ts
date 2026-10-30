import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { existsSync, readFileSync } from 'node:fs'

const pkg = JSON.parse(readFileSync('package.json', 'utf8')) as { version: string }

// Tauri 开发时固定端口；不清屏，便于看到 Rust 的输出。
// 在普通浏览器里调试时，开发桥接（scripts/dev-bridge.mjs）的令牌只在 dev 模式注入；构建产物里为空。
export default defineConfig(({ command }) => ({
  plugins: [vue()],
  clearScreen: false,
  server: { port: 1420, strictPort: true },
  envPrefix: ['VITE_', 'TAURI_'],
  build: { target: 'es2022', chunkSizeWarningLimit: 2000 },
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
    __BRIDGE_TOKEN__: JSON.stringify(
      command === 'serve' && existsSync('.bridge-token') ? readFileSync('.bridge-token', 'utf8').trim() : '',
    ),
  },
}))
