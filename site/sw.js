/* Reward Flights — service worker.
   Its only job is alerts: receive a push, show it, and open the right page on
   click. It deliberately does NOT cache the app (the site is already tiny and
   the data has its own freshness protocol; a caching SW would just risk
   serving stale availability). */
"use strict";

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

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
  event.waitUntil(self.registration.showNotification(title, options));
});

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
