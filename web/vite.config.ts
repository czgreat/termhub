import { defineConfig, type Plugin } from 'vite'
import { svelte } from '@sveltejs/vite-plugin-svelte'
import { createHash } from 'node:crypto'
import { readFileSync, writeFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'

const outDir = '../internal/hub/web/dist'

// Writes sw.js from src/sw.js with the list of shell files and a version that
// changes whenever any of them does (docs/M11 第 2 节).
function serviceWorker(): Plugin {
  return {
    name: 'termhub-sw',
    apply: 'build',
    writeBundle(_, bundle) {
      const files = Object.keys(bundle).filter(f => f.startsWith('assets/')).map(f => '/' + f)
      const icons = readdirSync(join(outDir, 'icons')).map(f => '/icons/' + f)
      const assets = ['/', '/manifest.webmanifest', ...icons, ...files]
      const h = createHash('sha256').update(assets.join('\n')).update(readFileSync(join(outDir, 'index.html')))
      for (const f of ['manifest.webmanifest', ...icons.map(i => i.slice(1))]) h.update(readFileSync(join(outDir, f)))
      const version = 'v' + h.digest('hex').slice(0, 12)
      const src = readFileSync('src/sw.js', 'utf8').replace('__VERSION__', version).replace('__ASSETS__', JSON.stringify(assets))
      writeFileSync(join(outDir, 'sw.js'), src)
    },
  }
}

export default defineConfig({
  plugins: [svelte(), serviceWorker()],
  // shown in the sidebar so a stale page is easy to spot; the release script sets it
  define: { __TH_VERSION__: JSON.stringify(process.env.TH_WEB_VERSION || 'dev') },
  build: {
    // xterm 6 ships pre-minified code using `||=`; a lower target rewrites it
    // in a way that breaks terminal query replies (docs/M9 第 2 节).
    target: 'es2022',
    outDir,
    emptyOutDir: true,
    sourcemap: false,
  },
  server: {
    // dev: proxy to a Hub started with TH_PUBLIC_URL including this origin
    proxy: { '/api': { target: process.env.TH_DEV_HUB ?? 'https://127.0.0.1:27443', secure: false, changeOrigin: false },
             '/ws': { target: process.env.TH_DEV_HUB ?? 'https://127.0.0.1:27443', secure: false, ws: true } },
  },
})
