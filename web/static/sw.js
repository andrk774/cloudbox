'use strict';
/* Service worker CloudBox: установка как приложение (PWA) и приём файлов из меню «Поделиться» (Android). */
const SHELL = 'cloudbox-shell-v2';
const ASSETS = ['/', '/app.css', '/app.js', '/views.js', '/md.js', '/vendor/purify.min.js', '/vendor/qrcode.js', '/icon.svg'];

self.addEventListener('install', e => { e.waitUntil(caches.open(SHELL).then(c => c.addAll(ASSETS)).then(() => self.skipWaiting())); });
self.addEventListener('activate', e => {
  e.waitUntil(caches.keys().then(keys => Promise.all(keys.filter(k => k !== SHELL).map(k => caches.delete(k)))).then(() => self.clients.claim()));
});

function idb() {
  return new Promise((res, rej) => {
    const r = indexedDB.open('cloudbox-share', 1);
    r.onupgradeneeded = () => r.result.createObjectStore('files');
    r.onsuccess = () => res(r.result);
    r.onerror = () => rej(r.error);
  });
}

self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (url.origin !== location.origin) return;
  if (url.pathname === '/share-target' && e.request.method === 'POST') {
    e.respondWith((async () => {
      const form = await e.request.formData();
      const files = form.getAll('files').filter(f => f && f.size !== undefined);
      const db = await idb();
      await new Promise((res, rej) => { const tx = db.transaction('files', 'readwrite'); tx.objectStore('files').put(files, 'pending'); tx.oncomplete = res; tx.onerror = () => rej(tx.error); });
      return Response.redirect('/#/receive', 303);
    })());
    return;
  }
  // API, WebDAV и файлы — всегда из сети; оболочку приложения — сеть с запасным вариантом из кэша (работает без интернета).
  if (e.request.method !== 'GET' || /^\/(api|dav|caldav|carddav|wopi|office|s)\//.test(url.pathname)) return;
  if (ASSETS.includes(url.pathname)) {
    e.respondWith(fetch(e.request).then(r => { const copy = r.clone(); caches.open(SHELL).then(c => c.put(e.request, copy)); return r; })
      .catch(() => caches.match(e.request)));
  }
});
