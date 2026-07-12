/* Reward Flights — service worker.
   Its only job is alerts: receive a push, show it, and open the right page on
   click. It deliberately does NOT cache the app (the site is already tiny and
   the data has its own freshness protocol; a caching SW would just risk
   serving stale availability). */
"use strict";

const PUSH_API = "https://alerts-rewardflights.lucy.sh";

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
