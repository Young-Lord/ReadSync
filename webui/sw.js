const CACHE_NAME = 'readsync-offline-v2';
const OFFLINE_URL = new URL('offline.html', self.location.href).toString();
const CONNECTIVITY_CHECK_HEADER = 'X-ReadSync-Connectivity-Check';

self.addEventListener('install', function (event) {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then(function (cache) {
        return cache.add(OFFLINE_URL);
      })
      .then(function () {
        return self.skipWaiting();
      })
  );
});

self.addEventListener('activate', function (event) {
  event.waitUntil(
    Promise.all([
      caches.keys().then(function (names) {
        return Promise.all(names.map(function (name) {
          if (name.startsWith('readsync-offline-') && name !== CACHE_NAME) {
            return caches.delete(name);
          }
          return Promise.resolve();
        }));
      }),
      self.clients.claim(),
    ])
  );
});

function isNavigationRequest(request) {
  const accept = request.headers.get('accept') || '';
  return request.mode === 'navigate' || accept.includes('text/html');
}

function fallbackOfflinePage() {
  return caches.open(CACHE_NAME)
    .then(function (cache) {
      return cache.match(OFFLINE_URL);
    })
    .then(function (response) {
      return response || new Response(
        '<!doctype html><meta charset="utf-8"><title>ReadSync 离线</title><body>网络恢复后会自动刷新。</body>',
        { headers: { 'Content-Type': 'text/html; charset=utf-8' } }
      );
    });
}

self.addEventListener('fetch', function (event) {
  const request = event.request;
  if (request.method !== 'GET') return;

  if (request.headers.get(CONNECTIVITY_CHECK_HEADER) === '1') {
    event.respondWith(fetch(request));
    return;
  }

  if (!isNavigationRequest(request)) return;

  event.respondWith(
    fetch(request).catch(function () {
      return fallbackOfflinePage();
    })
  );
});
