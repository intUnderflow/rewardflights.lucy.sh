/* Reward Flights — service worker: alerts (push) + offline-first shell.

   CACHING SCOPE — same-origin shell only. Availability data is deliberately
   NOT cached here: it has its own freshness protocol in the app (localStorage
   snapshot + version compare + stale banner), and a second cache layer would
   fight it. What we cache:
   - versioned assets (?v=N urls, /assets/*): cache-first — a new deploy mints
     new URLs, so cached entries can never go stale, and on a bad connection
     we skip even the 304 revalidation round-trip;
   - navigations: network-first with a short timeout, falling back to the
     cached shell — at dial-up speeds (or offline) the app still opens
     instantly and paints from its data snapshot. */
"use strict";

const PUSH_API = "https://alerts-rewardflights.lucy.sh";
const SHELL_CACHE = "rf-shell-v1";
const NAV_TIMEOUT_MS = 3500;

self.addEventListener("install", (e) => {
  e.waitUntil((async () => {
    // Precache the shell so offline works from the SECOND visit, not the
    // third: fetch the live index, then pull its versioned asset URLs out.
    try {
      const cache = await caches.open(SHELL_CACHE);
      const res = await fetch("/", { cache: "no-cache" });
      if (res.ok) {
        await cache.put("/", res.clone());
        const html = await res.text();
        const urls = [...html.matchAll(/(?:src|href)="(\/[^"]+)"/g)]
          .map((m) => m[1])
          .filter((u) => /\?v=\d+|^\/assets\//.test(u));
        await Promise.all(urls.map((u) => cache.add(u).catch(() => {})));
      }
    } catch { /* offline install — runtime caching will fill in */ }
    self.skipWaiting();
  })());
});

self.addEventListener("activate", (e) => {
  e.waitUntil((async () => {
    for (const k of await caches.keys()) {
      if (k !== SHELL_CACHE) await caches.delete(k);
    }
    await self.clients.claim();
  })());
});

/* A versioned asset: served cache-first forever (the URL IS the version). */
function isImmutable(url) {
  return url.origin === self.location.origin &&
    (/[?&]v=\d+/.test(url.search) || url.pathname.startsWith("/assets/"));
}

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return; // data + alerts API manage themselves

  if (req.mode === "navigate") {
    event.respondWith((async () => {
      const cache = await caches.open(SHELL_CACHE);
      try {
        const fresh = await Promise.race([
          fetch(req),
          new Promise((_, rej) => setTimeout(() => rej(new Error("nav timeout")), NAV_TIMEOUT_MS)),
        ]);
        // Belt against interception (captive portals, enterprise MITM roots):
        // only our own origin's un-redirected HTML may become the offline
        // shell. TLS blocks the classic portal forgery already; this closes
        // the残 remaining paths cheaply.
        const cacheable = fresh.ok && !fresh.redirected &&
          new URL(fresh.url).origin === self.location.origin &&
          (fresh.headers.get("content-type") || "").includes("text/html");
        if (cacheable) cache.put("/", fresh.clone()); // SPA: one shell serves every path
        return fresh;
      } catch {
        const shell = await cache.match("/");
        if (shell) return shell;
        return fetch(req); // no cache yet: let the failure surface honestly
      }
    })());
    return;
  }

  if (isImmutable(url)) {
    event.respondWith((async () => {
      const cache = await caches.open(SHELL_CACHE);
      const hit = await cache.match(req);
      if (hit) return hit;
      const res = await fetch(req);
      if (res.ok) {
        cache.put(req, res.clone());
        // A new ?v= supersedes its older siblings — drop them so the cache
        // doesn't accumulate one copy per deploy.
        const v = /[?&]v=\d+/.test(url.search);
        if (v) {
          for (const k of await cache.keys()) {
            const ku = new URL(k.url);
            if (ku.pathname === url.pathname && ku.search !== url.search) await cache.delete(k);
          }
        }
      }
      return res;
    })());
  }
});

self.addEventListener("push", (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch { /* malformed → generic */ }

  const title = data.title || "Award space just opened";
  const options = {
    body: data.body || "New award availability on a route you're watching.",
    data: { url: data.url || "/" },
    // One notification per topic: a re-alert on the same route+cabin replaces
    // the old one rather than stacking up.
    tag: data.tag || "rewardflights",
    renotify: true,
    icon: "/assets/icon-192.png",
    badge: "/assets/icon-192.png",
    timestamp: Date.now(),
  };
  // Tell the server this device actually received the push. The push service
  // returns 201 whether or not the OS ends up showing anything, so this ack —
  // sent from the device, when the push handler truly runs — is the only signal
  // that separates "reached the device" from "silently dropped in transit / dead
  // subscription". (It still can't prove the banner was displayed; only a click
  // proves that.) Best-effort: never let it block the notification.
  event.waitUntil(Promise.all([
    self.registration.showNotification(title, options),
    ackDelivery(),
  ]));
});

async function ackDelivery() {
  try {
    const sub = await self.registration.pushManager.getSubscription();
    if (!sub) return;
    await fetch(`${PUSH_API}/ack`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ endpoint: sub.endpoint }),
      keepalive: true,
    });
  } catch { /* diagnostic only — a failed ack must never affect the alert */ }
}

/* ---- offline outbox: deliver queued alert saves when the network returns.
   The queue is written by the page (IndexedDB "rf-outbox"); this handler
   makes delivery survive the tab being closed. Browsers without Background
   Sync flush from the page instead (online event + boot). ---- */
self.addEventListener("sync", (event) => {
  if (event.tag === "rf-outbox") event.waitUntil(flushOutboxSW());
});

function outboxDBSW() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open("rf-outbox", 1);
    req.onupgradeneeded = () => req.result.createObjectStore("ops", { keyPath: "endpoint" });
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function flushOutboxSW() {
  const db = await outboxDBSW();
  const ops = await new Promise((resolve, reject) => {
    const req = db.transaction("ops").objectStore("ops").getAll();
    req.onsuccess = () => resolve(req.result); req.onerror = () => reject(req.error);
  });
  for (const op of ops) {
    try {
      const res = await fetch(`${PUSH_API}${op.op === "subscribe" ? "/subscribe" : "/unsubscribe"}`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(op.payload),
      });
      // Delivered, or rejected outright (a 4xx never succeeds by retrying).
      if (res.ok || (res.status >= 400 && res.status < 500)) {
        await new Promise((resolve, reject) => {
          const tx = db.transaction("ops", "readwrite");
          tx.objectStore("ops").delete(op.endpoint);
          tx.oncomplete = resolve; tx.onerror = () => reject(tx.error);
        });
      }
    } catch {
      // Still offline: leaving the op queued makes the sync retry later —
      // rethrow so the platform knows this attempt didn't finish.
      db.close();
      throw new Error("outbox not drained");
    }
  }
  db.close();
}

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = event.notification.data?.url || "/";
  event.waitUntil((async () => {
    const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    // Reuse an open tab if we have one; otherwise open a new window.
    for (const c of all) {
      if (c.url.startsWith(self.location.origin)) {
        await c.focus();
        if ("navigate" in c) await c.navigate(url);
        return;
      }
    }
    await self.clients.openWindow(url);
  })());
});
