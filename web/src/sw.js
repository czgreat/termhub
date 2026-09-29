// Service worker (docs/M11 第 2 节): caches only the application shell, by
// version. It never touches /api/, /ws/ or downloads: only the paths listed
// here are handled at all; everything else goes straight to the network.
// The build fills in VERSION and ASSETS (vite.config.ts).
const VERSION = '__VERSION__'
const ASSETS = __ASSETS__

self.addEventListener('install', e => {
  // No skipWaiting: the page offers "new version, click to reload" instead of
  // pulling the rug from under a terminal being watched.
  e.waitUntil(caches.open(VERSION).then(c => c.addAll(ASSETS)))
})

self.addEventListener('activate', e => {
  e.waitUntil(caches.keys().then(keys => Promise.all(keys.filter(k => k !== VERSION).map(k => caches.delete(k)))).then(() => self.clients.claim()))
})

self.addEventListener('message', e => {
  if (e.data === 'skipWaiting') self.skipWaiting()
})

function shell(url) {
  return url.pathname === '/' || url.pathname === '/index.html' || url.pathname.startsWith('/assets/') ||
    url.pathname.startsWith('/icons/') || url.pathname === '/manifest.webmanifest'
}

self.addEventListener('fetch', e => {
  if (e.request.method !== 'GET') return
  const url = new URL(e.request.url)
  if (url.origin !== self.location.origin || !shell(url)) return
  if (url.pathname.startsWith('/assets/')) {
    // content-hashed: the cache is authoritative
    e.respondWith(caches.match(e.request).then(r => r || fetch(e.request)))
    return
  }
  // the shell itself: network first so a new version is seen; offline falls back
  // (a 5xx from the proxy while the Hub restarts is treated like no network)
  const cached = () => caches.match(e.request.mode === 'navigate' ? '/' : e.request)
  e.respondWith(fetch(e.request).then(r => r.ok ? r : cached().then(c => c || r)).catch(cached))
})
