// Renders public/icons/icon.svg to the PNG sizes the manifest and iOS need,
// using the Chromium that Playwright already has. Run: node tools/make-icons.mjs
import { chromium } from '@playwright/test'
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const dir = join(dirname(fileURLToPath(import.meta.url)), '..', 'public', 'icons')
mkdirSync(dir, { recursive: true })
const svg = readFileSync(join(dir, 'icon.svg'), 'utf8')

// The maskable icon keeps everything inside the central safe zone (80%).
const page = (size, inset) => `<!doctype html><body style="margin:0;background:#1b1f24">
  <div style="width:${size}px;height:${size}px;display:flex;align-items:center;justify-content:center">
    <div style="width:${size * (1 - 2 * inset)}px;height:${size * (1 - 2 * inset)}px">${svg}</div>
  </div></body>`

const browser = await chromium.launch()
const tab = await browser.newPage({ viewport: { width: 512, height: 512 }, deviceScaleFactor: 1 })
for (const [name, size, inset] of [['icon-192.png', 192, 0], ['icon-512.png', 512, 0], ['maskable-512.png', 512, 0.1], ['apple-touch-icon.png', 180, 0]]) {
  await tab.setViewportSize({ width: size, height: size })
  await tab.setContent(page(size, inset))
  writeFileSync(join(dir, name), await tab.screenshot({ type: 'png', omitBackground: false }))
  console.log('wrote', name)
}
await browser.close()
