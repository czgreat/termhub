import { mount } from 'svelte'
import './app.css'
import '@xterm/xterm/css/xterm.css'
import App from './App.svelte'
import { app } from './lib/state.svelte'
import { initTooltips } from './lib/tooltip'

mount(App, { target: document.getElementById('app')! })
initTooltips()

// Installable shell (docs/M11 第 2 节). A new version waits until the user
// clicks "refresh"; it never interrupts a terminal on its own.
if ('serviceWorker' in navigator) {
  navigator.serviceWorker.register('/sw.js').then(reg => {
    const watch = (w: ServiceWorker | null) => {
      if (!w) return
      w.addEventListener('statechange', () => {
        if (w.state !== 'installed' || !navigator.serviceWorker.controller) return
        // The shell is fetched network first, so a worker that finishes
        // installing right after the page loaded belongs to this very page:
        // take it quietly instead of offering a "new version" (PWA 复核).
        if (performance.now() < 15000) w.postMessage('skipWaiting')
        else app.updateReady = true
      })
    }
    if (reg.waiting && navigator.serviceWorker.controller) app.updateReady = true
    watch(reg.installing)
    reg.addEventListener('updatefound', () => watch(reg.installing))
    const check = () => reg.update().catch(() => {})
    setInterval(check, 60 * 60 * 1000)
    document.addEventListener('visibilitychange', () => { if (document.visibilityState === 'visible') check() })
    app.applyUpdate = () => {
      // Another tab may have activated the new worker already: then just reload.
      if (!reg.waiting) { location.reload(); return }
      navigator.serviceWorker.addEventListener('controllerchange', () => location.reload(), { once: true })
      reg.waiting.postMessage('skipWaiting')
    }
  }).catch(() => { /* not a secure context, or blocked: the app works without it */ })
}
