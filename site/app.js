/* Reward Flights — static SPA over open award-availability data.
   All data comes from the derived-data repo via raw.githubusercontent.com;
   the whole calendar dataset is one small bundle, so after boot every
   interaction (search, calendars, explore) is in-memory. */
"use strict";

/* ---------------- config ---------------- */

const DEFAULT_DATA_BASE =
  "https://raw.githubusercontent.com/intUnderflow/rewardflights.lucy.sh-data/main";

/* A data-origin override is allowed ONLY for local development. Accepting an
   arbitrary ?data= URL and persisting it would let a crafted link point a
   victim at attacker-controlled availability data forever, so we restrict it
   to loopback origins and never persist a remote one. */
function isDevOrigin(u) {
  try {
    const url = new URL(u, location.href);
    if (url.protocol !== "http:" && url.protocol !== "https:") return false;
    return ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  } catch { return false; }
}

const dataBase = (() => {
  try {
    const q = new URLSearchParams(location.search).get("data");
    if (q === "default") { localStorage.removeItem("rf:dataBase"); return DEFAULT_DATA_BASE; }
    if (q && isDevOrigin(q)) { localStorage.setItem("rf:dataBase", q); return q; }
    const saved = localStorage.getItem("rf:dataBase");
    if (saved && isDevOrigin(saved)) return saved;
    localStorage.removeItem("rf:dataBase");
    return DEFAULT_DATA_BASE;
  } catch { return DEFAULT_DATA_BASE; }
})();

const SNAPSHOT_KEY = "rf:avail:v1";
// One minute: the poll target is the ~200-byte manifest, so a tight loop is
// nearly free — and a 5-minute gap can miss a seat that opens and goes.
const MANIFEST_POLL_MS = 60 * 1000;
const DAY_MS = 86400000;
// How far ahead an EXACT-DATE alert may reach. BA releases award space ~355
// days out, so alerts for known dates that aren't on sale yet are a core use
// case; this cap (~18 months) sits generously past the release window while
// still catching year-typos. The server has no future cap and fires such a
// watch the moment its dates load (EC-3 frontier), so this is purely the UI
// guard-rail on the date picker.
const EXACT_MAX_AHEAD_DAYS = 540;
const NEW_BADGE_MS = 48 * 3600 * 1000;

/* Cabin bit → color class. Position in the seat-stack is ascending bit
   value (Economy at the bottom, First on top); unknown bits render gray. */
const BIT_CLASS = { 1: "cab-m", 2: "cab-w", 4: "cab-c", 8: "cab-f" };
const bitClass = (bit) => BIT_CLASS[bit] || "cab-x";

/* Cabin bit → BA redemption CabinCode (M/W/C/F). */
const CABIN_CODE = { 1: "M", 2: "W", 4: "C", 8: "F" };

/* "?cabins=CF" ⇄ bitmask, same letter vocabulary as alert watches. */
function parseCabins(s) {
  if (!/^[mwcf]{1,4}$/i.test(s || "")) return null;
  let m = 0;
  for (const ch of s.toUpperCase()) m |= { M: 1, W: 2, C: 4, F: 8 }[ch];
  return m;
}
function cabinLetters(mask) {
  return [1, 2, 4, 8].filter((b) => mask & b).map((b) => CABIN_CODE[b]).join("");
}

/* THE SIGNATURE gauge, one renderer for every surface that draws it: a fixed
   4-lane seat-stack. Lanes hold FIXED positions bottom→top = Economy(1),
   Premium(2), Business(4), First(8). A present cabin renders as a full bar;
   an absent cabin keeps its lane as a faint track line (styled by CSS), so
   cabin identity always reads by position — even without color.
   size: "cell" (calendar day cells) | "row" (panel headers, pair rows). */
const STACK_LANES = [1, 2, 4, 8];
function stackHTML(bits, { size = "cell" } = {}) {
  const lanes = STACK_LANES.map((bit) =>
    `<i class="${bits & bit ? bitClass(bit) : "lane-off"}"></i>`).join("");
  return `<span class="stack stack-${size}" aria-hidden="true">${lanes}</span>`;
}

/* Deep link into BA's Avios redemption search, pre-filled with the route,
   date, and cabin. Uses the metro/city codes we hold (departurePoint=LON,
   not a single airport) so the search covers the whole city — matching the
   granularity of our data. A return date is optional. `pax` becomes
   NumberOfAdults so the BA search never contradicts an active party-size
   filter (N adults is the honest v1 of party composition). */
function baBookingURL(origin, dest, outIso, bit, returnIso, pax = 1) {
  const ddmmyyyy = (iso) => { const [y, m, d] = iso.split("-"); return `${d}/${m}/${y}`; };
  const oneWay = !returnIso;
  const params = [
    ["eId", "100002"],
    ["pageid", "PLANREDEMPTIONJOURNEY"],
    ["tab_selected", "redeem"],
    ["redemption_type", "STD_RED"],
    ["amex_redemption_type", ""],
    ["upgradeOutbound", "true"],
    ["WebApplicationID", "BOD"],
    ["Output", ""],
    ["hdnAgencyCode", ""],
    ["departurePoint", origin],
    ["destinationPoint", dest],
    ["departInputDate", ddmmyyyy(outIso)],
  ];
  if (!oneWay) params.push(["returnInputDate", ddmmyyyy(returnIso)]);
  params.push(
    ["oneWay", oneWay ? "true" : "false"],
    ["RestrictionType", "Restricted"],
    ["NumberOfAdults", String(pax >= 1 && pax <= 9 ? pax : 1)],
    ["NumberOfYoungAdults", "0"],
    ["NumberOfChildren", "0"],
    ["NumberOfInfants", "0"],
    ["aviosapp", "true"],
    ["CabinCode", CABIN_CODE[bit] || ""],
  );
  // Values are codes/dates/flags — no characters that need percent-encoding,
  // and BA expects literal "/" in the dates, so join as-is.
  return "https://www.britishairways.com/travel/redeem/execclub/_gf/en_gb?" +
    params.map(([k, v]) => `${k}=${v}`).join("&");
}

/* ---------------- alerts (Web Push) ---------------- */

const PUSH_API = "https://alerts-rewardflights.lucy.sh";
const VAPID_PUBLIC_KEY = "BMHjtxbmirrQAoG2QNGDFNRZ-n5ijHTz99bVUkVLEAJDWsv3Ks6DSoKK88WKhCDk3rS_CmIDPWifQupVjL15TtQ";


const pushSupported = () =>
  "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;

/* iOS exposes PushManager only to a Home-Screen web app (16.4+), so a plain
   Safari tab needs an install step first. Detect that case to explain it
   rather than showing a button that can't work. */
const isIOS = () => /iP(hone|ad|od)/.test(navigator.userAgent) ||
  (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
const isStandalone = () =>
  window.matchMedia?.("(display-mode: standalone)").matches || navigator.standalone === true;

/* Get a live registration to talk to pushManager through. Never cache one: the
   worker calls skipWaiting()+claim(), so the active worker can be swapped while
   the page is open, and pushManager calls made through a superseded
   registration never settle. register() is idempotent and cheap once
   registered; awaiting ready guarantees the worker is active before we
   subscribe. */
async function ensureSW() {
  const reg = await navigator.serviceWorker.register("/sw.js");
  await navigator.serviceWorker.ready;
  return reg;
}

function urlB64ToUint8(b64) {
  const pad = "=".repeat((4 - (b64.length % 4)) % 4);
  const raw = atob((b64 + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from([...raw].map((c) => c.charCodeAt(0)));
}

const b64url = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf)))
  .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

/* Thrown when the browser won't let us ask, or the user said no. `state` is
   the Notification permission value; "default" means the prompt never really
   appeared (Chrome's "quieter messaging" resolves the request immediately and
   hides it behind an address-bar icon), which needs different advice from an
   outright block. */
class PermissionError extends Error {
  constructor(state) {
    super(state);
    this.state = state;
  }
}

/* The browser's push subscription, creating it (and asking permission) only
   when the user has actually asked for an alert. */
async function getSubscription({ create = false } = {}) {
  const reg = await ensureSW();
  let sub = await reg.pushManager.getSubscription();
  if (!sub && create) {
    // Only prompt when we actually need to: re-asking when permission is
    // already granted is pointless (and some engines never resolve it).
    const perm = Notification.permission === "granted"
      ? "granted" : await Notification.requestPermission();
    if (perm !== "granted") throw new PermissionError(perm);
    sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlB64ToUint8(VAPID_PUBLIC_KEY),
    });
  }
  return sub;
}

/* What to tell someone whose browser won't show the prompt. We can't override
   these settings from a page — all we can do is say exactly where the switch
   is. Returns inner HTML, so it can drop straight into the status paragraph. */
function permissionHelpHTML(state) {
  const chromey = /Chrome|Chromium|Edg/.test(navigator.userAgent) && !/Firefox/.test(navigator.userAgent);
  if (state === "denied") {
    return `<b>Notifications are blocked for this site.</b> ${chromey
      ? `Click the icon at the left of the address bar → <b>Notifications</b> → <b>Allow</b>, then reload and try again.`
      : `Open your browser's site settings for this page, allow notifications, then try again.`}`;
  }
  // "default": the request resolved without the user ever seeing a dialog.
  return `<b>Your browser hid the permission prompt.</b> ${chromey
    ? `Chrome calls this <i>quieter messaging</i>. Click the blocked-bell icon at the
       right of the address bar and choose <b>Allow</b>, then press Save alerts again —
       or turn it off in Settings → Privacy and security → Site settings → Notifications.`
    : `Allow notifications for this site from the address bar or site settings,
       then press Save alerts again.`}`;
}

const subPayload = (sub) => ({
  endpoint: sub.endpoint,
  p256dh: b64url(sub.getKey("p256dh")),
  auth: b64url(sub.getKey("auth")),
});

/* A watch is one thing a person wants: a route, a direction, some cabins, and
   — the point of all this — when they can actually travel.
     {route, kind:"rt"|"ow", cabins:["C","F"],
      out?:{from,to}, ret?:{from,to}, nights?:{min,max}}
   Omitting `out` means "any time"; omitting `ret`/`nights` on a round trip
   means "wherever the nights window lands me". */

/* Every alerts-API call goes through a hard timeout: on a blackholing
   network (captive portal accepting packets into the void) a bare fetch()
   hangs for minutes, and the bell would spin forever. Retries only where
   the operation is idempotent. */
async function apiFetch(path, { method = "GET", body, timeout = 8000, retries = 1 } = {}) {
  // onLine === false is definitive (true is not): don't make an offline user
  // sit through timeout+retry before the cached/queued path kicks in.
  if (navigator.onLine === false) throw new Error("offline");
  let lastErr;
  for (let i = 0; i <= retries; i++) {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeout);
    try {
      return await fetch(`${PUSH_API}${path}`, {
        method,
        signal: ctl.signal,
        ...(body !== undefined && {
          headers: { "content-type": "application/json" },
          body: JSON.stringify(body),
        }),
      });
    } catch (e) {
      lastErr = e;
    } finally {
      clearTimeout(timer);
    }
    if (i < retries) await new Promise((r) => setTimeout(r, 800 * (i + 1)));
  }
  throw lastErr;
}

async function fetchWatches(sub) {
  const res = await apiFetch(`/watches?endpoint=${encodeURIComponent(sub.endpoint)}`);
  if (!res.ok) throw new Error(`alert service unavailable (${res.status})`);
  const watches = (await res.json()).watches || [];
  // Last-known-good copy: offline saves are read-modify-write, and modifying
  // a GUESS of the list would sync data loss. This cache is the only basis
  // an offline edit is allowed to build on.
  try { localStorage.setItem("rf:watches-cache", JSON.stringify(watches)); } catch {}
  return watches;
}

/* null = we have never successfully seen the server list on this device —
   distinct from a cached empty list, which is a fine base to edit. */
function loadWatchesCache() {
  try {
    const raw = localStorage.getItem("rf:watches-cache");
    return raw === null ? null : JSON.parse(raw);
  } catch { return null; }
}

/* The full device view: watches plus delivery telemetry. `device.reachable` is
   false when we've sent this device pushes it never acknowledged — the silent-
   suppression case that has no browser-visible signal. */
async function fetchDevice(sub) {
  const res = await apiFetch(`/watches?endpoint=${encodeURIComponent(sub.endpoint)}`);
  if (!res.ok) throw new Error(`alert service unavailable (${res.status})`);
  const j = await res.json();
  return { watches: j.watches || [], device: j.device || null };
}

/* Strip a watch to the WIRE schema before POSTing. Every save path is
   read-modify-write: it fetches the stored list (whose entries carry
   server-added fields — id, status, createdAt, lastFiredAt) and re-POSTs it,
   and the server's strict decoder rejects fields outside the Watch schema
   ("status" in particular → 400 "invalid JSON body" the moment any other
   watch exists). One sanitize function, used by saveWatches itself so no
   save path can forget it. Empty/default optionals are dropped too, keeping
   the canonical form the content-addressed id is derived from. */
function wireWatch(w) {
  const out = { route: w.route, kind: w.kind, cabins: w.cabins };
  if (w.out) out.out = { ...(w.out.from && { from: w.out.from }), ...(w.out.to && { to: w.out.to }) };
  if (w.ret) out.ret = { ...(w.ret.from && { from: w.ret.from }), ...(w.ret.to && { to: w.ret.to }) };
  if (w.nights) out.nights = { min: w.nights.min, max: w.nights.max };
  if (w.leadDays > 0) out.leadDays = w.leadDays;
  if ((w.minSeats || 0) >= 2) out.minSeats = w.minSeats;
  if (w.via) { out.via = w.via; out.conn = w.conn || 1; }
  return out;
}

/* Saves the list and returns the server's echo (watches with ids + status),
   so callers can e.g. re-baseline a just-edited watch by its new id. */
/* Thrown when a save couldn't reach the server but IS safely queued in the
   outbox — callers show "will sync when you're back online" instead of an
   error. Server REJECTIONS (4xx) still throw plain errors: a watch the
   server won't accept must never sit silently in a retry queue. */
class QueuedOffline extends Error {
  constructor() { super("queued for sync"); this.queued = true; }
}

async function saveWatches(sub, watches) {
  if (!watches.length) {
    // Nothing left to watch: drop the subscription rather than keep a dangling
    // endpoint on file.
    try {
      const res = await apiFetch("/unsubscribe", { method: "POST", body: { endpoint: sub.endpoint } });
      void res;
    } catch {
      await queueOutbox({ endpoint: sub.endpoint, op: "unsubscribe", payload: { endpoint: sub.endpoint } });
      await sub.unsubscribe().catch(() => {});
      throw new QueuedOffline();
    }
    await sub.unsubscribe().catch(() => {});
    return [];
  }
  const payload = { ...subPayload(sub), watches: watches.map(wireWatch) };
  let res;
  try {
    res = await apiFetch("/subscribe", { method: "POST", body: payload });
  } catch {
    // Network-level failure only: queue the LATEST desired state and let the
    // sync machinery deliver it when connectivity returns.
    await queueOutbox({ endpoint: sub.endpoint, op: "subscribe", payload });
    throw new QueuedOffline();
  }
  if (!res.ok) {
    const detail = await res.json().catch(() => ({}));
    throw new Error(detail.error || `couldn't save alerts (${res.status})`);
  }
  const echo = (await res.json().catch(() => ({}))).watches || [];
  try { localStorage.setItem("rf:watches-cache", JSON.stringify(echo)); } catch {}
  return echo;
}

/* ---------------- offline outbox (alert saves survive dead networks) ------ */

/* One pending operation per endpoint — last write wins, exactly like the
   server's own semantics (POST /subscribe replaces the whole list). Lives in
   IndexedDB so the service worker's Background Sync handler can read it too;
   browsers without Background Sync (Safari) flush on the window's "online"
   event and at boot instead. */
function outboxDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open("rf-outbox", 1);
    req.onupgradeneeded = () => req.result.createObjectStore("ops", { keyPath: "endpoint" });
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function queueOutbox(op) {
  try {
    const db = await outboxDB();
    await new Promise((resolve, reject) => {
      const tx = db.transaction("ops", "readwrite");
      tx.objectStore("ops").put({ ...op, queuedAt: Date.now() });
      tx.oncomplete = resolve; tx.onerror = () => reject(tx.error);
    });
    db.close();
    // One-shot Background Sync where the platform has it: delivery happens
    // even if the tab is closed before the network returns.
    try {
      const reg = await navigator.serviceWorker.ready;
      await reg.sync?.register("rf-outbox");
    } catch { /* no SW / no sync: the online-event + boot flush covers it */ }
  } catch { /* IDB unavailable (private mode): the save error stands */ }
}

async function flushOutbox() {
  try {
    const db = await outboxDB();
    const ops = await new Promise((resolve, reject) => {
      const req = db.transaction("ops").objectStore("ops").getAll();
      req.onsuccess = () => resolve(req.result); req.onerror = () => reject(req.error);
    });
    for (const op of ops) {
      try {
        const res = await apiFetch(op.op === "subscribe" ? "/subscribe" : "/unsubscribe",
          { method: "POST", body: op.payload, retries: 0 });
        // Delivered (or rejected outright — either way the queue entry is
        // done; a 4xx will never succeed by retrying).
        if (res.ok || (res.status >= 400 && res.status < 500)) {
          await new Promise((resolve, reject) => {
            const tx = db.transaction("ops", "readwrite");
            tx.objectStore("ops").delete(op.endpoint);
            tx.oncomplete = resolve; tx.onerror = () => reject(tx.error);
          });
          if (res.ok) refreshAlertCount();
        }
      } catch { /* still offline — keep it queued */ }
    }
    db.close();
  } catch { /* IDB unavailable */ }
}

/* ---------------- watches: helpers + the live match count ---------------- */

const CABIN_BIT = { M: 1, W: 2, C: 4, F: 8 };
const watchMask = (w) => (w.cabins || []).reduce((m, c) => m | (CABIN_BIT[c] || 0), 0);
const reverseRoute = (r) => r.split("-").reverse().join("-");
const NIGHTS_ANY = [1, 30];

/* Day index (into the bundle) for an ISO date, unclamped. */
function isoToIdx(iso) {
  const [y, m, d] = (iso || "").split("-").map(Number);
  if (!y) return null;
  return Math.round((Date.UTC(y, m - 1, d) - store.epochMs) / DAY_MS);
}
const idxToIso = (i) => isoOf(dayDate(i));

/* THE credibility feature. A date-constrained watch can be legitimately silent
   for weeks, and the user will assume it's broken. The whole dataset is already
   in memory, so we can answer "what matches RIGHT NOW?" instantly, with no
   network — while they're still choosing the dates.
   Returns {pairs, perCabin: Map<bit,count>, first: {out,ret}|null}. */
function matchesNow(w, { list = false } = {}) {
  const empty = { pairs: 0, perCabin: new Map(), first: null, list: [] };
  const keys = [];
  if (!store.bundle) return empty;
  if (!w.via && !store.bundle.routes[w.route]) return empty;
  const mask = watchMask(w);
  if (!mask) return empty;

  const t0 = Math.max(0, todayIndex());
  const last = store.bundle.days - 1;
  // A lead-time watch floors the outbound at today + leadDays (rolling), which
  // must give the same answer the server does.
  const floor = w.leadDays ? Math.min(last, t0 + w.leadDays) : t0;
  // Mirror the SERVER's asymmetric clamp (clampRange): From only rises to the
  // watched start, To only falls to the horizon. A range entirely beyond the
  // horizon therefore collapses to From > To (empty) — NOT onto the last day,
  // which a symmetric clamp would do, inventing a false "matches right now"
  // for an exact-date watch whose dates aren't even released yet.
  const oFrom = w.out?.from ? Math.max(floor, isoToIdx(w.out.from)) : floor;
  const oTo = w.out?.to ? Math.min(last, isoToIdx(w.out.to)) : last;

  // A party watch counts only days with evidence of >= minSeats together —
  // the same predicate the alert server fires on (its satisfiedBits), so the
  // "N match right now" line never promises what a push wouldn't deliver.
  // A leg with no seat data satisfies NOTHING at minSeats >= 2 (all-zero
  // bits, matching the server); watchProblem explains that dead state before
  // this function is ever consulted.
  const need = (w.minSeats || 0) >= 2 ? w.minSeats : 1;
  const legBits = (key) =>
    need > 1 ? (routeBitsAtLeast(key, need) || new Uint8Array(store.bundle.days)) : routeBits(key);

  // Via watches match CHAINS: the same overnight-stop windows and focus-leg
  // cabin coupling as the via calendar (and the alert server), so the "N
  // match right now" line never promises what a push wouldn't deliver.
  if (w.via) {
    const [vo, vd] = w.route.split("-");
    const conn = w.conn || 1;
    const path = w.kind === "ow" ? [vo, w.via, vd] : [vo, w.via, vd, w.via, vo];
    const legKeys = [];
    for (let i = 0; i < path.length - 1; i++) legKeys.push(`${path[i]}-${path[i + 1]}`);
    if (legKeys.some((k) => !store.bundle.routes[k])) return empty;
    const legsB = legKeys.map((k) => routeBits(k));
    const focus = new Set(focusLegs(path));
    const holds = (i, day, cb) => {
      const bits = day >= 0 && day <= last ? legsB[i][day] : 0;
      return focus.has(i) ? (bits & cb) : bits;
    };
    const bitsList = cabinLegend().map(([bit]) => bit).filter((b) => mask & b);
    const perCabin = new Map();
    let pairs = 0, first = null;
    const [minN, maxN] = w.nights ? [w.nights.min, w.nights.max] : NIGHTS_ANY;
    const rFrom = w.ret?.from ? Math.max(t0, isoToIdx(w.ret.from)) : t0;
    const rTo = w.ret?.to ? Math.min(last, isoToIdx(w.ret.to)) : last;
    for (let d = oFrom; d <= oTo; d++) {
      if (w.kind === "ow") {
        let v = 0;
        for (const cb of bitsList) {
          if (!holds(0, d, cb)) continue;
          for (let n = d + 1; n <= Math.min(d + conn, last); n++) {
            if (holds(1, n, cb)) { v |= cb; break; }
          }
        }
        if (!v) continue;
        pairs++;
        if (!first) first = { out: idxToIso(d), ret: null };
        if (list) for (const [bit] of cabinLegend()) if (v & bit) keys.push(`${idxToIso(d)}|${bit}`);
        for (const [bit] of cabinLegend()) if (v & bit) perCabin.set(bit, (perCabin.get(bit) || 0) + 1);
        continue;
      }
      const perR = new Map(); // return long-haul day -> coupled cabins
      for (const cb of bitsList) {
        if (!holds(0, d, cb)) continue;
        for (let n = d + 1; n <= Math.min(d + conn, last); n++) {
          if (!holds(1, n, cb)) continue;
          const lo = Math.max(n + minN, rFrom), hi = Math.min(n + maxN, rTo);
          for (let r = lo; r <= hi; r++) {
            if ((perR.get(r) & cb) || !holds(2, r, cb)) continue;
            for (let h = r + 1; h <= Math.min(r + conn, last); h++) {
              if (holds(3, h, cb)) { perR.set(r, (perR.get(r) || 0) | cb); break; }
            }
          }
        }
      }
      for (const r of [...perR.keys()].sort((a, b) => a - b)) {
        const both = perR.get(r);
        pairs++;
        if (!first) first = { out: idxToIso(d), ret: idxToIso(r) };
        if (list) for (const [bit] of cabinLegend()) if (both & bit) keys.push(`${idxToIso(d)}|${idxToIso(r)}|${bit}`);
        for (const [bit] of cabinLegend()) if (both & bit) perCabin.set(bit, (perCabin.get(bit) || 0) + 1);
      }
    }
    return { pairs, perCabin, first, list: keys };
  }

  const out = legBits(w.route);
  const perCabin = new Map();
  let pairs = 0, first = null;

  if (w.kind === "ow") {
    for (let d = oFrom; d <= oTo; d++) {
      const v = out[d] & mask;
      if (!v) continue;
      pairs++;
      if (!first) first = { out: idxToIso(d), ret: null };
      if (list) for (const [bit] of cabinLegend()) if (v & bit) keys.push(`${idxToIso(d)}|${bit}`);
      for (const [bit] of cabinLegend()) if (v & bit) perCabin.set(bit, (perCabin.get(bit) || 0) + 1);
    }
    return { pairs, perCabin, first, list: keys };
  }

  const rev = reverseRoute(w.route);
  if (!store.bundle.routes[rev]) return empty;      // a round trip needs a way home
  const ret = legBits(rev);
  const [minN, maxN] = w.nights ? [w.nights.min, w.nights.max] : NIGHTS_ANY;
  // No return range given → it's implied by the outbound window + nights.
  // Same asymmetric clamp as the outbound (server parity).
  const rFrom = w.ret?.from ? Math.max(t0, isoToIdx(w.ret.from)) : t0;
  const rTo = w.ret?.to ? Math.min(last, isoToIdx(w.ret.to)) : last;

  for (let d = oFrom; d <= oTo; d++) {
    const o = out[d] & mask;
    if (!o) continue;
    const lo = Math.max(d + minN, rFrom);
    const hi = Math.min(d + maxN, rTo, last);
    for (let r = lo; r <= hi; r++) {
      const both = o & ret[r];                       // same cabin on BOTH legs
      if (!both) continue;
      pairs++;
      if (!first) first = { out: idxToIso(d), ret: idxToIso(r) };
      if (list) for (const [bit] of cabinLegend()) if (both & bit) keys.push(`${idxToIso(d)}|${idxToIso(r)}|${bit}`);
      for (const [bit] of cabinLegend()) if (both & bit) perCabin.set(bit, (perCabin.get(bit) || 0) + 1);
    }
  }
  return { pairs, perCabin, first, list: keys };
}

/* A notification you never receive is worse than no notification: the push can
   be accepted by the push service and still be dropped by the OS, silently, with
   no signal to us or to you. So the site keeps its own record of what a watch
   matched last time you looked, and tells you what's new — no server, no data
   held, and it works even when notifications are broken end to end. */
const SEEN_KEY = "rf:seen:v1";

function loadSeen() {
  try { return JSON.parse(localStorage.getItem(SEEN_KEY)) || {}; } catch { return {}; }
}
function saveSeen(seen) {
  try { localStorage.setItem(SEEN_KEY, JSON.stringify(seen)); } catch {}
}

/* Trips matching a watch that weren't matching the last time this device looked.
   Returns {pairs:[{out,ret}], count} — the same news a notification would carry. */
function newSinceSeen(w, seen) {
  const now = matchesNow(w, { list: true });
  const before = new Set(seen[w.id]?.pairs || []);
  const fresh = now.list.filter((p) => !before.has(p));
  return { count: fresh.length, first: fresh.slice(0, 3), everSeen: !!seen[w.id] };
}

/* Record what a watch matches now, so the next visit can diff against it. */
function markSeen(watches) {
  const seen = loadSeen();
  const keep = {};
  for (const w of watches) {
    keep[w.id] = { pairs: matchesNow(w, { list: true }).list, t: Math.floor(Date.now() / 1000) };
  }
  saveSeen(keep);   // watches that no longer exist drop out
}

/* Why a watch can never fire — so we can say so instead of leaving it silent. */
function watchProblem(w) {
  const t0 = Math.max(0, todayIndex());
  if (w.via) {
    const [vo, vd] = w.route.split("-");
    const path = w.kind === "ow" ? [vo, w.via, vd] : [vo, w.via, vd, w.via, vo];
    for (let i = 0; i < path.length - 1; i++) {
      if (!store.bundle?.routes?.[`${path[i]}-${path[i + 1]}`]) {
        return `The ${path[i]}→${path[i + 1]} leg isn't in the data, so this via journey can't be found.`;
      }
    }
  } else if (w.kind === "rt" && !store.bundle?.routes?.[reverseRoute(w.route)]) {
    return "There's no return route in the data, so a round trip can't be found.";
  }
  // A seat-count watch on a route with no seat data is silently dead forever
  // — the exact failure mode this function exists to prevent. (Matches the
  // server: unknown counts never fire a minSeats >= 2 watch.) The bell keeps
  // its party row visible for such a watch so the threshold can be lowered.
  if ((w.minSeats || 0) >= 2 &&
      (!store.seatRoutes.has(w.route) ||
        (w.kind === "rt" && !store.seatRoutes.has(reverseRoute(w.route))))) {
    return `This route has no seat-count data yet, so an alert for ${w.minSeats}${w.minSeats === 4 ? "+" : ""} seats together can never fire.`;
  }
  if (w.out?.from && w.out?.to && isoToIdx(w.out.from) > isoToIdx(w.out.to)) {
    return "Your leave window starts after it ends.";
  }
  if (w.out?.to && isoToIdx(w.out.to) < t0) return "These dates have passed.";
  if (w.kind === "rt" && w.out && w.ret) {
    const [minN, maxN] = w.nights ? [w.nights.min, w.nights.max] : NIGHTS_ANY;
    if (isoToIdx(w.ret.to) < isoToIdx(w.out.from) + minN) {
      return `Your return window ends before your outbound date plus ${minN} night${minN > 1 ? "s" : ""}.`;
    }
    if (isoToIdx(w.ret.from) > isoToIdx(w.out.to) + maxN) {
      return `Your return window starts after the longest trip you'd take (${maxN} nights).`;
    }
  }
  return null;
}

/* "1–20 Oct" / "28 Sep–5 Oct" / "28 Dec–4 Jan 2027" */
function fmtRange(fromIso, toIso) {
  if (!fromIso) return "any date";
  const a = dayDate(isoToIdx(fromIso)), b = dayDate(isoToIdx(toIso));
  const thisYear = new Date().getUTCFullYear();
  const mA = fmtMonthShort.format(a), mB = fmtMonthShort.format(b);
  const yB = b.getUTCFullYear() !== thisYear ? ` ${b.getUTCFullYear()}` : "";
  if (fromIso === toIso) return `${a.getUTCDate()} ${mA}${yB}`;
  if (mA === mB && !yB) return `${a.getUTCDate()}–${b.getUTCDate()} ${mA}`;
  return `${a.getUTCDate()} ${mA}–${b.getUTCDate()} ${mB}${yB}`;
}

/* One line describing what a watch is watching. */
/* "at least 7 days' notice" → a friendly phrase. */
function leadPhrase(n) {
  if (n % 7 === 0 && n >= 7) { const w = n / 7; return `${w} week${w > 1 ? "s" : ""}' notice`; }
  return `${n} day${n > 1 ? "s" : ""}' notice`;
}

function watchSummary(w) {
  const bits = [];
  bits.push(w.leadDays ? `Any time, ${leadPhrase(w.leadDays)}`
    : w.out ? `Out ${fmtRange(w.out.from, w.out.to)}` : "Any date");
  if (w.kind === "rt") {
    if (w.ret) bits.push(`back ${fmtRange(w.ret.from, w.ret.to)}`);
    if (w.nights) bits.push(`${w.nights.min}–${w.nights.max} nights`);
  }
  if (w.via) bits.push(`via ${w.via}${(w.conn || 1) > 1 ? ` (≤${w.conn}n stop)` : ""}`);
  // The party constraint is part of what the watch IS — same noun ("N+
  // seats") as the bell control and the push copy, so the three surfaces
  // agree.
  if ((w.minSeats || 0) >= 2) bits.push(`${w.minSeats}+ seats`);
  return bits.join(" · ");
}

/* The current subscription and its watches, or null if this device isn't
   watching anything. Never throws — alerts are an enhancement, and the page
   must render fine when the alert service is unreachable. */
async function currentAlerts() {
  try {
    if (!pushSupported() || Notification.permission !== "granted") return null;
    const sub = await getSubscription();
    if (!sub) return null;
    return { sub, ...(await fetchDevice(sub)) };
  } catch { return null; }
}

/* ---------------- tiny utils ---------------- */

const $ = (sel, el = document) => el.querySelector(sel);
const esc = (s) => String(s).replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

function el(html) {
  const t = document.createElement("template");
  t.innerHTML = html.trim();
  return t.content.firstElementChild;
}

const fmtMonth = new Intl.DateTimeFormat("en-GB", { month: "long", year: "numeric", timeZone: "UTC" });
const fmtMonthShort = new Intl.DateTimeFormat("en-GB", { month: "short", timeZone: "UTC" });
const fmtDate = new Intl.DateTimeFormat("en-GB", { weekday: "long", day: "numeric", month: "long", year: "numeric", timeZone: "UTC" });
const fmtTime = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });
const fmtShort = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short" });
const fmtRet = new Intl.DateTimeFormat("en-GB", { weekday: "short", day: "numeric", month: "short", timeZone: "UTC" });

function timeAgo(unixSec) {
  const s = Math.max(0, (Date.now() - unixSec * 1000) / 1000);
  if (s < 90) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400 * 1.5) return `${Math.round(s / 3600)} h ago`;
  if (s > 86400 * 365) return "a while ago"; // clock skew / very old — stay calm
  return `${Math.round(s / 86400)} days ago`;
}

function utcDate(y, m, d) { return new Date(Date.UTC(y, m, d)); }
function isoOf(dt) { return dt.toISOString().slice(0, 10); }

/* ---------------- fetch layer (concurrent-request dedupe) ---------------- */

/* Coalesces concurrent requests for the same URL; it is NOT a response cache.
   The entry is dropped once the request settles (success or failure), so a
   later fetch of the same URL re-hits the network — otherwise the changes feed
   and flight detail would freeze at boot-time state for the tab's whole life. */
const inflight = new Map();

function getJSON(url, { timeout = 8000, retries = 2 } = {}) {
  if (inflight.has(url)) return inflight.get(url);
  const attempt = async (left) => {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeout);
    try {
      const res = await fetch(url, { signal: ctl.signal });
      // Every response (404s included) carries a Date header from the edge:
      // the freshness buckets run on THIS clock, never the device's.
      const d = res.headers.get("date");
      if (d) syncServerTime(Date.parse(d));
      if (!res.ok) {
        // Cancel the unread body or the request stays "in flight" forever —
        // it pins the connection and never reaches requestfinished (the 404s
        // the bucket probes produce made this visible; it was always true).
        try { res.body?.cancel(); } catch { /* already closed */ }
        const err = new Error(`HTTP ${res.status} for ${url}`);
        err.status = res.status;
        throw err;
      }
      return await res.json();
    } catch (err) {
      if (left > 0 && err.status !== 404) {
        await new Promise((r) => setTimeout(r, 800 + Math.random() * 900));
        return attempt(left - 1);
      }
      throw err;
    } finally { clearTimeout(timer); }
  };
  const p = attempt(retries).finally(() => { inflight.delete(url); });
  inflight.set(url, p);
  return p;
}

/* ---------------- data store ---------------- */

const store = {
  bundle: null,        // availability.json payload
  changes: null,       // changes/recent.json payload (nullable)
  fromSnapshot: false, // current bundle came from localStorage
  epochMs: 0,
  placeList: [],       // [{code,name,country,search[]}]
  origins: new Set(),
  destsByOrigin: new Map(),
  chainCache: new Map(),
  monthCache: new Map(),
  rtCache: new Map(),
  hasAnySeatData: false, // any route in the adopted bundle carries the optional "s" seats layer
  seatRoutes: new Set(), // route keys with at least one validated "s" string
};

const ROUTE_RE = /^[A-Z]{3}-[A-Z]{3}$/;
const CODE_RE = /^[A-Z]{3}$/;

function adoptBundle(bundle, fromSnapshot) {
  if (bundle.schema !== 1) throw new Error(`Unsupported data schema ${bundle.schema}`);
  // Route keys and place codes get interpolated into markup and URLs, so
  // structurally validate them at the trust boundary and drop anything
  // malformed — defense in depth even though the data origin is trusted.
  bundle.routes = Object.fromEntries(
    Object.entries(bundle.routes || {}).filter(([k]) => ROUTE_RE.test(k)));
  bundle.places = Object.fromEntries(
    Object.entries(bundle.places || {}).filter(([k]) => CODE_RE.test(k)));
  store.bundle = bundle;
  store.fromSnapshot = fromSnapshot;
  const [ey, em, ed] = bundle.epoch.split("-").map(Number);
  store.epochMs = Date.UTC(ey, em - 1, ed);
  _months12 = null; // epoch (and thus month day-offsets) can change between bundles
  store.placeList = Object.entries(bundle.places).map(([code, p]) => ({
    code, name: p.name || code, country: p.country || "", search: p.search || [],
  }));
  store.origins = new Set();
  store.destsByOrigin = new Map();
  store.monthCache = new Map();
  store.rtCache = new Map();
  store.chainCache = new Map();
  // Seats layer ("s"): optional, additive, per route+airline. Validate it at
  // the same trust boundary as routes/places and recompute the presence flags
  // on EVERY adoption — a localStorage snapshot and the network bundle can
  // disagree about coverage within one session.
  store.hasAnySeatData = false;
  store.seatRoutes = new Set();
  const seatLen = 2 * (bundle.days || 0);
  for (const key of Object.keys(bundle.routes)) {
    const [o, d] = key.split("-");
    store.origins.add(o);
    if (!store.destsByOrigin.has(o)) store.destsByOrigin.set(o, []);
    store.destsByOrigin.get(o).push(d);
    const r = bundle.routes[key];
    if (r.s && typeof r.s === "object" && !Array.isArray(r.s)) {
      for (const [al, str] of Object.entries(r.s)) {
        // Exactly 2 hex chars per day, and only for an airline whose "a"
        // string exists — "a" is the sole presence authority.
        if (typeof str !== "string" || str.length !== seatLen ||
            !/^[0-9A-Fa-f]*$/.test(str) || typeof r.a?.[al] !== "string") {
          delete r.s[al];
        }
      }
      if (Object.keys(r.s).length) {
        store.seatRoutes.add(key);
        store.hasAnySeatData = true;
      } else delete r.s;
    } else if ("s" in r) delete r.s;
  }
}

function placeName(code) { return store.bundle?.places?.[code]?.name || code; }
function placeCountry(code) { return store.bundle?.places?.[code]?.country || ""; }

/* Merged (all airlines OR'd) bits for a route, as an array indexed by
   day offset from epoch. */
function routeBits(routeKey) {
  const route = store.bundle.routes[routeKey];
  if (!route) return null;
  const days = store.bundle.days;
  const merged = new Uint8Array(days);
  for (const [airlineId, str] of Object.entries(route.a)) {
    // Per the format contract a future airline may encode >1 hex char per day
    // (width>1). We only decode single-nibble strings today; skip anything
    // wider so we degrade to "not shown" rather than misaligning days.
    const width = store.bundle.airlines?.[airlineId]?.width ?? 1;
    if (width !== 1) continue;
    const n = Math.min(str.length, days);
    for (let i = 0; i < n; i++) {
      const v = parseInt(str[i], 16);
      if (v) merged[i] |= v;
    }
  }
  return merged;
}

/* Same Uint8Array shape as routeBits, but a bit is set only when there's
   evidence of >= n seats in that cabin on one flight — decoded from the
   optional per-route seats layer ("s": 2 hex chars per day, a 2-bit monotone
   threshold code per cabin: 0 = no sign of >=2 seats, 1 = >=2, 2 = >=3,
   3 = >=4; a party of n fits iff code >= n-1). Airlines merge per-cabin with
   MAX, never SUM — a party rides one airline's one flight; since the
   threshold test is monotone, MAX-then-compare equals OR of per-airline
   passes, which is what's computed here.
   n <= 1 returns exactly routeBits (the seats layer never changes
   single-passenger semantics). Returns null when the route (or its seats
   layer) is absent, so callers can render an honest presence fallback
   instead of silently filtering to zero. */
function routeBitsAtLeast(routeKey, n) {
  if (!n || n <= 1) return routeBits(routeKey);
  const route = store.bundle.routes[routeKey];
  if (!route || !store.seatRoutes.has(routeKey)) return null;
  const days = store.bundle.days;
  const need = n - 1; // party of n fits iff code >= n-1
  const merged = new Uint8Array(days);
  for (const [airlineId, str] of Object.entries(route.s)) {
    // The seats layer rides the same day window as "a"; airlines whose "a"
    // we don't decode (width !== 1) contribute no presence, so their seat
    // codes must not light anything either.
    const width = store.bundle.airlines?.[airlineId]?.width ?? 1;
    if (width !== 1) continue;
    const nDays = Math.min(str.length >> 1, days);
    for (let i = 0; i < nDays; i++) {
      const byte = parseInt(str.slice(2 * i, 2 * i + 2), 16) || 0;
      if (!byte) continue;
      let v = 0;
      if ((byte & 3) >= need) v |= 1;        // Economy  M: bits 0-1
      if (((byte >> 2) & 3) >= need) v |= 2; // Premium  W: bits 2-3
      if (((byte >> 4) & 3) >= need) v |= 4; // Business C: bits 4-5
      if (((byte >> 6) & 3) >= need) v |= 8; // First    F: bits 6-7
      merged[i] |= v;
    }
  }
  // "a" is the sole presence authority: never light a day/cabin the presence
  // layer doesn't show, whatever a (malformed) seats string claims.
  const pres = routeBits(routeKey);
  for (let i = 0; i < days; i++) merged[i] &= pres[i];
  return merged;
}

/* Per-cabin threshold codes for one day of a route (MAX across airlines):
   {bit: 0..3}, or null when the route carries no seats layer. Code 0 means
   "no sign of >=2 seats" (unknown OR known-1) — never "0 seats". */
function seatCodes(routeKey, idx) {
  const route = store.bundle.routes[routeKey];
  if (!route || !store.seatRoutes.has(routeKey)) return null;
  const codes = { 1: 0, 2: 0, 4: 0, 8: 0 };
  for (const [airlineId, str] of Object.entries(route.s)) {
    const width = store.bundle.airlines?.[airlineId]?.width ?? 1;
    if (width !== 1) continue;
    if (2 * idx + 2 > str.length || idx < 0) continue;
    const byte = parseInt(str.slice(2 * idx, 2 * idx + 2), 16) || 0;
    codes[1] = Math.max(codes[1], byte & 3);
    codes[2] = Math.max(codes[2], (byte >> 2) & 3);
    codes[4] = Math.max(codes[4], (byte >> 4) & 3);
    codes[8] = Math.max(codes[8], (byte >> 6) & 3);
  }
  return codes;
}

/* Seat coverage is per route, not per bundle: true when BOTH legs of a pair
   can answer "does a party of N fit?". */
function pairSeatsKnown(outKey, retKey) {
  return store.seatRoutes.has(outKey) && store.seatRoutes.has(retKey);
}

function dayDate(i) { return new Date(store.epochMs + i * DAY_MS); }
function dayIndexOf(iso) {
  const [y, m, d] = iso.split("-").map(Number);
  return Math.round((Date.UTC(y, m - 1, d) - store.epochMs) / DAY_MS);
}
/* Day indices are calendar dates (epoch + i, read as a UTC day). "Today" must
   be the user's LOCAL calendar date mapped onto that same index, or users west
   of UTC lose their current day every evening. */
function todayIndex() {
  const now = new Date();
  return Math.round((Date.UTC(now.getFullYear(), now.getMonth(), now.getDate()) - store.epochMs) / DAY_MS);
}

/* Cabin legend, merged across airlines: bit → label (first legend wins). */
function cabinLegend() {
  const legend = new Map();
  for (const al of Object.values(store.bundle.airlines)) {
    for (const [bitStr, label] of Object.entries(al.cabins || {})) {
      const bit = Number(bitStr);
      if (!legend.has(bit)) legend.set(bit, label);
    }
  }
  return [...legend.entries()].sort((a, b) => a[0] - b[0]); // [[bit,label],…]
}

/* Per-month availability-day counts for a route or an origin (12 months
   from the current month). Cached. Deliberately party-agnostic (presence
   bits, any party size) — surfaces that show these while a pax>1 filter is
   active must say so in their aria/title rather than pass the counts off as
   party counts. */
function monthCounts(kind, key) {
  const ck = `${kind}|${key}`;
  if (store.monthCache.has(ck)) return store.monthCache.get(ck);
  const months = next12Months();
  const counts = new Array(12).fill(0);
  const routes = kind === "route" ? [key]
    : Object.keys(store.bundle.routes).filter((r) => r.startsWith(key + "-"));
  for (const rk of routes) {
    const bits = routeBits(rk);
    if (!bits) continue;
    for (let mi = 0; mi < 12; mi++) {
      const { start, end } = months[mi];
      for (let i = Math.max(0, start); i < Math.min(bits.length, end); i++) {
        if (bits[i]) counts[mi]++;
      }
    }
  }
  store.monthCache.set(ck, counts);
  return counts;
}

/* The 12 months from the current LOCAL month, as day-offset windows into the
   current bundle's epoch. Cached, but the cache is keyed on (epoch, year-month)
   so it rebuilds after an epoch change (adoptBundle nulls it) or when a long-
   lived tab crosses a month/year boundary — otherwise every calendar would
   silently shift by a month or a year. */
let _months12 = null, _months12Key = "";
function next12Months() {
  const now = new Date();
  const key = `${store.epochMs}|${now.getFullYear()}-${now.getMonth()}`;
  if (_months12 && _months12Key === key) return _months12;
  const out = [];
  for (let k = 0; k < 12; k++) {
    const y = now.getFullYear(), m = now.getMonth() + k;
    const first = utcDate(y, m, 1), next = utcDate(y, m + 1, 1);
    out.push({
      y: first.getUTCFullYear(), m: first.getUTCMonth(),
      label: fmtMonthShort.format(first),
      start: Math.round((first - store.epochMs) / DAY_MS),
      end: Math.round((next - store.epochMs) / DAY_MS),
    });
  }
  _months12 = out;
  _months12Key = key;
  return out;
}

/* "2026-07" for a next12Months() entry — the URL form of a month filter. */
function monthISO(mo) {
  return `${mo.y}-${String(mo.m + 1).padStart(2, "0")}`;
}

/* pax > 1 counts only days with evidence of >= pax seats; a route without
   the seats layer falls back to presence (callers gate the honest note). */
function routeTotals(routeKey, pax = 1) {
  const bits = pax > 1
    ? (routeBitsAtLeast(routeKey, pax) ?? routeBits(routeKey))
    : routeBits(routeKey);
  if (!bits) return { total: 0, perCabin: new Map(), union: 0 };
  const t0 = Math.max(0, todayIndex());
  let total = 0, union = 0;
  const perCabin = new Map();
  for (let i = t0; i < bits.length; i++) {
    const v = bits[i];
    if (!v) continue;
    total++;
    union |= v;
    for (let bit = 1; bit <= v; bit <<= 1) {
      if (v & bit) perCabin.set(bit, (perCabin.get(bit) || 0) + 1);
    }
  }
  return { total, perCabin, union };
}

/* The round-trip engine. For every outbound day D (today onwards), the cabins
   in which the WHOLE trip can be flown: a bit survives only when it's open on
   the outbound day AND on at least one return day R in
   [D+minNights .. D+maxNights] (inclusive, clamped to the data horizon) —
   same-cabin-both-ways by construction. minNights is never allowed below 1:
   a zero-night same-day turnaround is not a round trip. Missing reverse
   route → all zeros. O(days × window), cached per (route, mask, window). */
function roundTripBits(outKey, retKey, mask, minNights, maxNights, pax = 1) {
  const t0 = Math.max(0, todayIndex());
  // pax MUST be in the key: rtCache only resets on adoptBundle, so omitting
  // it would serve party-of-1 arrays to party-of-4 views (and vice versa).
  const ck = `${outKey}|${retKey}|${mask}|${minNights}|${maxNights}|${pax}|${t0}`;
  if (store.rtCache.has(ck)) return store.rtCache.get(ck);
  // pax > 1 thresholds BOTH legs; a leg without the seats layer falls back to
  // presence bits (callers show the honest per-route note — never an
  // silently-empty view).
  const legBits = (k) => pax > 1
    ? (routeBitsAtLeast(k, pax) ?? routeBits(k))
    : routeBits(k);
  const out = legBits(outKey);
  const ret = store.bundle.routes[retKey] ? legBits(retKey) : null;
  const round = new Uint8Array(out ? out.length : store.bundle.days);
  if (out && ret) {
    const min = Math.max(1, minNights);
    for (let d = t0; d < out.length; d++) {
      const vOut = out[d] & mask;
      if (!vOut) continue;
      let acc = 0;
      const rEnd = Math.min(ret.length - 1, d + maxNights);
      for (let r = d + min; r <= rEnd && acc !== vOut; r++) acc |= ret[r] & vOut;
      round[d] = acc;
    }
  }
  store.rtCache.set(ck, round);
  return round;
}

/* One-stop destinations from o that have NO direct route: code -> {hub, both}.
   `both` = the same hub also gets you home, so a round trip is plannable
   (matching viaHub's rule); a hub that works both ways wins over one that
   only gets you there. The map and /from/ list use this to show the whole
   reachable world, not just the nonstop one. */
function viaDestsFrom(o) {
  const direct = new Set(store.destsByOrigin.get(o) || []);
  const out = new Map();
  for (const h of store.destsByOrigin.get(o) || []) {
    for (const d of store.destsByOrigin.get(h) || []) {
      if (d === o || direct.has(d)) continue;
      const both = (store.destsByOrigin.get(d) || []).includes(h) &&
        (store.destsByOrigin.get(h) || []).includes(o);
      const cur = out.get(d);
      if (!cur || (both && !cur.both)) out.set(d, { hub: h, both });
    }
  }
  return out;
}

/* ---------------- multi-city chain engine ----------------
   Foundation for spoke-city planning (e.g. Billund→London→Tokyo): not yet wired
   to any view. Generalises the round-trip engine from a fixed out/return pair to
   an N-leg itinerary so a search whose endpoints have no direct route resolves
   through the London hub instead of coming up empty. See MULTICITY-SPEC.md. */

/* Concrete routings that actually exist in the data for a searched O→D: the
   direct route first, then any one-stop O→H→D through a hub the graph connects.
   The route graph is a star around London, so for a spoke city this is
   O(out-degree) — usually the single path [O, "LON", D]. maxStops caps the
   number of intermediate hubs; returns [] for O===D. */
function resolveRoutings(o, d, { maxStops = 1 } = {}) {
  if (o === d) return [];
  const dests = (k) => store.destsByOrigin.get(k) || [];
  const paths = [];
  if (dests(o).includes(d)) paths.push([o, d]);
  if (maxStops >= 1) {
    for (const h of dests(o)) {
      if (h === o || h === d) continue;
      if (dests(h).includes(d)) paths.push([o, h, d]);
    }
  }
  return paths;
}

/* Per-departure-day cabin mask for completing a whole itinerary.
   path = [A, B, C, …]; every adjacent pair is a route that must exist.
   gaps = one [min, max] day-window per junction (so path.length - 2 entries):
   [0, 1] = a same-day-or-overnight connection through the hub, [7, 14] = a
   stopover of that many nights. So Billund→Tokyo return is ONE call:
   chainBits(["BLL","LON","TYO","LON","BLL"], mask, [[0,1],[7,14],[0,1]], pax, 1).

   Chain legs are SEPARATE redemptions, so — unlike roundTripBits — the cabin
   filter must NOT couple every leg: a Billund hop with no Club must never
   hide a Club long-haul (measured: BLL-LON has 0 premium-economy days while
   LON-TYO has 112 — all-legs-coupled would return nothing forever). `focus`
   is the ARRAY of leg indices the mask applies to — the long-haul legs
   (callers use focusLegs) — and those legs are coupled to one shared cabin
   (a "Club" trip means Club on both long legs); every other leg just needs
   award space in ANY cabin for the same party. The result's bits are the
   focus legs' shared cabins, keyed by first-leg departure day. focus = null
   couples ALL legs (the single-ticket semantics): with path [A,B,A], gaps
   [[minN,maxN]] that is exactly roundTripBits, which mctest.js cross-checks.
   Missing leg / bad gaps / bad focus → all zeros. O(legs × days × window);
   pax>1 thresholds every leg and falls back to presence where a leg has no
   seats layer, matching the round-trip engine's honest-note contract.
   Cached like roundTripBits (the map calls this per via destination on every
   pan/zoom frame); chainCache resets on adoptBundle, and everything that
   changes the answer — including pax and focus — is in the key. */
function chainBits(path, mask, gaps, pax = 1, focus = null) {
  const days = store.bundle.days;
  const empty = () => new Uint8Array(days);
  if (!Array.isArray(path) || path.length < 2) return empty();
  if (!Array.isArray(gaps) || gaps.length !== path.length - 2) return empty();
  const nLegs = path.length - 1;
  if (focus !== null && !(Array.isArray(focus) && focus.length &&
      focus.every((f) => Number.isInteger(f) && f >= 0 && f < nLegs))) return empty();
  const t0 = Math.max(0, todayIndex());
  const ck = `${path.join(".")}|${mask}|${gaps.map((g) => g.join("-")).join(".")}|${pax}|${focus ? focus.join(".") : "*"}|${t0}`;
  if (store.chainCache.has(ck)) return store.chainCache.get(ck);
  const legBits = (k) => (pax > 1 ? (routeBitsAtLeast(k, pax) ?? routeBits(k)) : routeBits(k));
  const legs = [];
  for (let i = 0; i < nLegs; i++) {
    const key = `${path[i]}-${path[i + 1]}`;
    if (!store.bundle.routes[key]) return empty();
    legs.push(legBits(key));
  }
  // Backward pass: reach[d] = the focus cabins legs i..end can still complete
  // if leg i departs day d. One rule covers every leg: a focus leg ANDs its
  // own masked cabins into the payload; a non-focus leg gates on any-space
  // and passes the payload through untouched (seeded at 15 = "every cabin
  // still possible" when no focus leg follows). Leg 0 is clamped to today;
  // inner legs start at 0 (harmless — junctions only move forward).
  let reach = null;
  for (let i = nLegs - 1; i >= 0; i--) {
    const cur = empty(), leg = legs[i], start = i === 0 ? t0 : 0;
    const gap = i < nLegs - 1 ? gaps[i] : null;
    const gMin = gap ? Math.max(0, gap[0]) : 0, gMax = gap ? gap[1] : 0;
    const isFocus = focus === null || focus.includes(i);
    for (let d = start; d < days; d++) {
      const own = leg[d] & (isFocus ? mask : 15);
      if (!own) continue;
      let v = isFocus ? own : 15;
      if (gap) {
        let acc = 0;
        const cap = isFocus ? own : 15;
        const hi = Math.min(days - 1, d + gMax);
        for (let n = d + gMin; n <= hi && acc !== cap; n++) acc |= reach[n] & cap;
        v = isFocus ? own & acc : acc;
      }
      cur[d] = v;
    }
    reach = cur;
  }
  store.chainCache.set(ck, reach);
  return reach;
}

/* The legs a via-trip's cabin filter should target: every leg at least half
   as long as the longest (so BLL→LON→TYO focuses the 9,500km long-haul, not
   the 700km hop, and a balanced NYC→LON→DXB focuses both). Great-circle via
   the places' g coords; a leg with no coords counts as length 0. */
function focusLegs(path) {
  const km = [];
  for (let i = 0; i < path.length - 1; i++) {
    const ga = store.bundle.places[path[i]]?.g, gb = store.bundle.places[path[i + 1]]?.g;
    if (!ga || !gb) { km.push(0); continue; }
    const rad = Math.PI / 180;
    const s = Math.sin(((gb[0] - ga[0]) * rad) / 2) ** 2 +
      Math.cos(ga[0] * rad) * Math.cos(gb[0] * rad) * Math.sin(((gb[1] - ga[1]) * rad) / 2) ** 2;
    km.push(2 * 6371 * Math.asin(Math.sqrt(s)));
  }
  const max = Math.max(...km);
  const legs = km.map((k, i) => (max > 0 && k >= max / 2 ? i : -1)).filter((i) => i >= 0);
  return legs.length ? legs : km.map((_, i) => i); // no coords anywhere → couple everything
}

/* The hub for a one-stop O→D when no direct route exists — with `both`, one
   that also gets you home (D→hub→O). First candidate wins (the graph is a
   star, so there's rarely more than one). null = direct exists or unreachable. */
function viaHub(o, d, { both = false } = {}) {
  if (store.bundle.routes[`${o}-${d}`]) return null;
  const out = resolveRoutings(o, d).filter((p) => p.length === 3).map((p) => p[1]);
  if (!both) return out[0] ?? null;
  const back = new Set(resolveRoutings(d, o).filter((p) => p.length === 3).map((p) => p[1]));
  return out.find((h) => back.has(h)) ?? null;
}

/* Stop length at the hub, in nights: the NEXT leg departs 1..conn days after
   the previous leg. The floor is 1 by design — we don't know flight times, so
   a same-day connection can't be promised, but any arrival today makes any
   departure tomorrow bookable. An overnight stop is the guarantee. URL
   ?conn= → session pref → 1 (exactly one night). */
function parseConn(s) { return /^[1-3]$/.test(s || "") ? Number(s) : null; }
function getConnPref() { try { return parseConn(sessionStorage.getItem("rf:conn")); } catch { return null; } }
function setConnPref(c) { try { sessionStorage.setItem("rf:conn", String(c)); } catch {} }
function activeConn(params = new URLSearchParams(location.search)) {
  return parseConn(params.get("conn")) ?? getConnPref() ?? 1;
}

/* Seat-count filtering is only honest at ONE consistent threshold: every leg
   of the chain must carry the seats layer, mirroring pairSeatsKnown. */
function chainSeatsKnown(path) {
  for (let i = 0; i < path.length - 1; i++)
    if (!store.seatRoutes.has(`${path[i]}-${path[i + 1]}`)) return false;
  return true;
}

/* Everywhere you can get to from o in at most one stop — the search's
   destination universe. Tiny even from the hub itself (≤ out-degree²·small). */
function reachableDests(o) {
  const direct = store.destsByOrigin.get(o) || [];
  const set = new Set(direct);
  for (const h of direct)
    for (const d2 of store.destsByOrigin.get(h) || []) if (d2 !== o) set.add(d2);
  return set;
}

/* ---------------- boot ---------------- */

const mainEl = $("#main");
const bannerEl = $("#banner");

async function boot() {
  let painted = false;

  // Fast path: render immediately from the localStorage snapshot.
  try {
    const raw = localStorage.getItem(SNAPSHOT_KEY);
    if (raw) {
      adoptBundle(JSON.parse(raw), true);
      painted = true;
      route();
    }
  } catch { try { localStorage.removeItem(SNAPSHOT_KEY); } catch {} }

  // Network refresh (browser HTTP cache + max-age=300 make this cheap).
  try {
    const fresh = await getJSON(`${dataBase}/availability.json`);
    const changedV = !store.bundle || store.bundle.v !== fresh.v;
    adoptBundle(fresh, false);
    try { localStorage.setItem(SNAPSHOT_KEY, JSON.stringify(fresh)); } catch {}
    hideBanner();
    // Re-render on first paint always; on a version change, only if it won't
    // wipe a search the user has already started typing.
    if (!painted || (changedV && !isTypingInSearch())) route();
    if (painted && changedV) { pulseFreshness(); announce(`Availability updated to ${freshLabel()}.`); }
  } catch (err) {
    if (!painted) {
      renderFatal(err);
      return;
    }
    showBanner(`Live data is unreachable right now — showing availability from ${freshLabel()}.`, true);
  }

  loadChanges();
  schedulePoll();
  refreshAlertCount();   // header link reflects what this device is watching

  // Register the service worker for EVERYONE (not just alert users): it
  // caches the shell offline-first, which is what keeps the app opening
  // instantly on airport-grade connections. Immediately: registration is
  // cheap (sw.js is ~1KB), precache runs post-activation so nothing that
  // awaits serviceWorker.ready queues behind it, and any delay here just
  // delays everything alerts-related on first visits.
  if ("serviceWorker" in navigator) {
    navigator.serviceWorker.register("/sw.js").catch(() => {});
  }

  // The instant connectivity returns: re-sync data and drain the outbox
  // rather than waiting out the poll interval. (Tab refocus already syncs
  // via visibilitychange; this covers the visible-tab-on-flapping-wifi case.)
  window.addEventListener("online", () => { checkForUpdate(); flushOutbox(); });
  // Drain anything queued by a previous session (covers browsers without
  // Background Sync, and syncs that fired while no page was open to hear).
  setTimeout(flushOutbox, 2500);
}

async function loadChanges() {
  // Pin to the current bundle version so a new generation bypasses the
  // 5-minute CDN cache and the feed stays consistent with the calendar.
  const v = store.bundle?.v ? `?v=${encodeURIComponent(store.bundle.v)}` : "";
  try {
    store.changes = await getJSON(`${dataBase}/changes/recent.json${v}`);
    if (current.page === "home") refreshHomeModules(); // enrich without stomping input
  } catch { /* keep whatever we had; feed is enrichment only */ }
}

/* ---------------- freshness ---------------- */

function freshLabel() {
  if (!store.bundle) return "…";
  const d = new Date(store.bundle.t * 1000);
  const today = new Date();
  return d.toDateString() === today.toDateString()
    ? `${fmtTime.format(d)} today` : fmtShort.format(d);
}

function renderFreshness() {
  const elx = $("#freshness");
  if (!store.bundle) { elx.textContent = "…"; return; }
  const ageMs = Date.now() - store.bundle.t * 1000;
  // The "data as of" prefix is a separate span so the phone header can drop it
  // (it stays in the title tooltip) and keep just the timestamp on one line.
  elx.innerHTML = `<span class="fr-prefix">data as of </span>${esc(freshLabel())}`;
  elx.classList.toggle("stale", ageMs > 24 * 3600 * 1000);
  elx.title = `Availability data generated from source updated ${timeAgo(store.bundle.t)}. Always verify with the airline.`;
}

function pulseFreshness() {
  const elx = $("#freshness");
  elx.classList.remove("pulse");
  void elx.offsetWidth;
  elx.classList.add("pulse");
}

let pollTimer = null;
function schedulePoll() {
  clearInterval(pollTimer);
  pollTimer = setInterval(checkForUpdate, MANIFEST_POLL_MS);
  syncServerClock().then(() => bucketCatchUp());
  setInterval(syncServerClock, 5 * 60 * 1000);
  scheduleBucketPoll();
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") { checkForUpdate(); bucketCatchUp(4); }
  });
}

async function checkForUpdate() {
  if (document.visibilityState !== "visible" || !store.bundle) return;
  try {
    // The ~5-minute-stale floor: raw's CDN caches this mutable URL for 300s
    // (the ?ts= busts only the BROWSER cache). It stays as the fallback under
    // the freshness buckets below, and as the cold-start path.
    const manifest = await getJSON(`${dataBase}/manifest.json?ts=${Math.floor(Date.now() / 60000)}`, { retries: 0 });
    await refreshFromManifest(manifest);
    hideBanner();
  } catch { /* silent — next poll retries */ }
}

/* Adopt a newer generation described by a manifest, fetching content from
   `base` (the main branch, or an immutable tag path — which is what makes a
   bucket refresh instantly fresh). The stale-replica guard stays: adopt only
   when the bundle we got back actually IS a new version, else a stale CDN
   copy would fire a false "updated" pulse + destructive re-render. */
async function refreshFromManifest(manifest, base = dataBase) {
  if (!manifest?.v || manifest.v === store.bundle.v) return false;
  // MONOTONIC adoption: with two discovery paths at different staleness (the
  // buckets ~30s, the fallback manifest ~5 min), "different version" is not
  // enough — after a bucket adopts a fresh generation, the stale fallback
  // would see the OLD version, refetch, and downgrade. Source time only ever
  // moves forward.
  if (Number.isFinite(manifest.t) && Number.isFinite(store.bundle.t) && manifest.t <= store.bundle.t) return false;
  // New generation: patch forward from the ~2KB changes feed when we can
  // PROVE equivalence, else fetch the full bundle.
  const fresh = await deltaOrFullBundle(manifest, base);
  if (!fresh.v || fresh.v === store.bundle.v) return false;
  if (Number.isFinite(fresh.t) && Number.isFinite(store.bundle.t) && fresh.t <= store.bundle.t) return false;
  const wasRoute = current.page === "route" || current.page === "trip";
  const scrollY = window.scrollY;
  adoptBundle(fresh, false);
  try { localStorage.setItem(SNAPSHOT_KEY, JSON.stringify(fresh)); } catch {}
  loadChanges();
  // Don't yank a half-typed search out from under the user.
  if (!isTypingInSearch()) {
    route();
    if (wasRoute) window.scrollTo({ top: scrollY });
    pulseFreshness();
    announce(`Availability updated to ${freshLabel()}.`);
  }
  return true;
}

/* ---------------- freshness buckets (design-time agreement) ----------------

   The publisher tags the data repo t-<unix/30> at every 30s boundary whose
   HEAD moved; we poll the JUST-CLOSED bucket's tag URL ~10s after its close.
   A 200 is fresh by construction (the URL couldn't be requested before the
   tag existed); a 404 is permanently true (the bucket is over), so the CDN
   caching either answer for 5 minutes can never lie to anyone. Typical
   commit-to-painted: ~30s, vs the fallback poll's ~5 minutes.

   The one way to break this is asking EARLY — a premature 404 would be
   cached at the edge right as the tag lands — so bucket arithmetic runs
   exclusively on server time learned from Date headers (getJSON syncs on
   every response), advanced by performance.now() (monotonic: immune to NTP
   steps and manual clock changes), with a hard guard at fire time. */

const TAG_BUCKET_MS = 30 * 1000;
const TAG_POLL_DELAY_MS = 10 * 1000; // Δ past close; measured tag visibility ~3s

let serverAnchorMs = null; // serverDate - performance.now() at last sync
function syncServerTime(dateMs) {
  if (Number.isFinite(dateMs)) serverAnchorMs = dateMs - performance.now();
}
function serverNowMs() {
  return serverAnchorMs === null ? null : serverAnchorMs + performance.now();
}

/* Date is NOT a CORS-safelisted response header, so the opportunistic sync in
   getJSON only works same-origin (dev) — a cross-origin data host (prod)
   exposes nothing. The authoritative clock therefore comes from OUR OWN
   origin: a HEAD to the site root (Cloudflare Pages static — unmetered, and
   its Date is fully readable same-origin). Boot + every 5 minutes;
   performance.now() carries us between syncs. */
async function syncServerClock() {
  try {
    const res = await fetch("/", { method: "HEAD", cache: "no-store" });
    const d = res.headers.get("date");
    if (d) syncServerTime(Date.parse(d));
  } catch { /* offline — the next sync or a same-origin response covers it */ }
}

/* Tag-path base: beside the branch ref in production, a /tags/ subtree on a
   local dev origin. */
function tagBase(bucket) {
  if (/\/main$/.test(dataBase)) return dataBase.replace(/\/main$/, `/refs/tags/t-${bucket}`);
  return `${dataBase}/tags/t-${bucket}`;
}

let bucketTimer = null;
let lastPolledBucket = 0;

function scheduleBucketPoll() {
  clearTimeout(bucketTimer);
  const now = serverNowMs();
  if (now === null) { bucketTimer = setTimeout(scheduleBucketPoll, 3000); return; }
  const bucket = Math.floor(now / TAG_BUCKET_MS); // the currently OPEN bucket
  const at = (bucket + 1) * TAG_BUCKET_MS + TAG_POLL_DELAY_MS + Math.random() * 3000;
  bucketTimer = setTimeout(() => { pollBucket(bucket).finally(scheduleBucketPoll); },
    Math.max(500, at - now));
}

async function pollBucket(bucket) {
  if (document.visibilityState !== "visible" || !store.bundle) return;
  const now = serverNowMs();
  // The hard premature-poll guard: this bucket must be closed + Δ in SERVER
  // time (2s tolerance for a Date-header resync between scheduling and
  // firing). Polling early would poison edges with cached 404s, so when in
  // doubt we skip — a missed bucket is recovered by the next one (tags point
  // at HEAD) or by the fallback manifest poll.
  if (now === null || now < (bucket + 1) * TAG_BUCKET_MS + TAG_POLL_DELAY_MS - 2000) return;
  if (bucket <= lastPolledBucket) return;
  lastPolledBucket = bucket;
  try {
    const manifest = await getJSON(`${tagBase(bucket)}/manifest.json`, { retries: 0, timeout: 6000 });
    await refreshFromManifest(manifest, tagBase(bucket));
  } catch { /* 404 = quiet bucket (permanently true); anything else, the fallback covers */ }
}

/* Catch-up: probe backward over recently-closed buckets (newest first, first
   200 wins — tags point at HEAD so one hit is total recovery). Runs at boot
   and when a tab returns to visibility; closed buckets are frozen, so these
   probes can never poison anything. The publisher prunes tags after ~10
   minutes, so probing further back than the window is pointless anyway. */
async function bucketCatchUp(maxBack = 10) {
  const now = serverNowMs();
  if (now === null || !store.bundle) return;
  const newest = Math.floor((now - TAG_POLL_DELAY_MS) / TAG_BUCKET_MS) - 1;
  for (let b = newest; b > newest - maxBack; b--) {
    if (b <= lastPolledBucket) return;
    try {
      const manifest = await getJSON(`${tagBase(b)}/manifest.json`, { retries: 0, timeout: 6000 });
      lastPolledBucket = Math.max(lastPolledBucket, b);
      await refreshFromManifest(manifest, tagBase(b));
      return;
    } catch { /* quiet or pruned bucket — keep probing older */ }
  }
}

function isTypingInSearch() {
  const a = document.activeElement;
  return !!(a && a.tagName === "INPUT" && a.closest(".search-card"));
}

/* Reach manifest.v by patching the in-memory snapshot with the changes feed
   (~2KB) instead of refetching the whole bundle (~27KB) — on a 20kbps link
   that's one second instead of eleven, with far fewer chances to stall.
   The patch is adopted ONLY when provably equivalent: the recount of
   route+date pairs must match the manifest's own counts.routeDates exactly.
   Every unprovable situation falls back to the full fetch. */
async function deltaOrFullBundle(manifest, base = dataBase) {
  const full = () => getJSON(`${base}/availability.json?v=${encodeURIComponent(manifest.v)}`);
  const old = store.bundle;
  try {
    if (typeof manifest.counts?.routeDates !== "number") return full();
    if (manifest.epoch !== old.epoch) return full();        // year rollover re-anchors everything
    // The feed reflects "a" bit changes only — a bundle carrying the seats
    // layer would silently desync, so delta is gated off until the feed
    // grows seat-transition entries.
    if (Object.values(old.routes).some((e) => e.s)) return full();

    const feed = await getJSON(`${base}/changes/recent.json?v=${encodeURIComponent(manifest.v)}`, { retries: 1 });
    if (feed.schema !== 1 || feed.v !== manifest.v) return full(); // stale CDN replica
    const all = feed.entries || [];
    const entries = all.filter((e) => e.t > old.t);
    // Overlap proof: if every entry is newer than our snapshot, older ones
    // may have rolled off the feed's 1000-entry cap — we can't know what we
    // missed.
    if (all.length && entries.length === all.length) return full();

    const next = structuredClone(old);
    for (let i = entries.length - 1; i >= 0; i--) {          // oldest first; newest wins
      const e = entries[i];
      const route = next.routes[e.r];
      const s = route?.a?.[e.al];
      if (typeof s !== "string") return full();              // new route/airline: not patchable
      const idx = dayIndexOf(e.d);
      if (!(Number.isInteger(idx) && idx >= 0 && idx < s.length)) return full(); // horizon moved
      const mask = e.k === "closed"
        ? 0
        : [...(e.c || "")].reduce((m, ch) => m | (CABIN_BIT[ch] || 0), 0);
      route.a[e.al] = s.slice(0, idx) + mask.toString(16).toUpperCase() + s.slice(idx + 1);
    }
    // The generator drops dates before its source commit's UTC date, and the
    // feed doesn't report past roll-off — mirror the drop before recounting
    // or every midnight would fail the invariant.
    const cutoffIdx = Math.floor((manifest.t * 1000 - store.epochMs) / DAY_MS);
    if (cutoffIdx > 0) {
      for (const entry of Object.values(next.routes)) {
        for (const [al, str] of Object.entries(entry.a)) {
          const upto = Math.min(cutoffIdx, str.length);
          if (str.slice(0, upto) !== "0".repeat(upto)) {
            entry.a[al] = "0".repeat(upto) + str.slice(upto);
          }
        }
      }
    }
    // Equivalence proof: count route+date pairs with availability (union
    // across airlines per day) and demand an exact match with the manifest.
    let routeDates = 0;
    for (const entry of Object.values(next.routes)) {
      const strs = Object.values(entry.a);
      const len = strs[0]?.length || 0;
      for (let i = 0; i < len; i++) {
        for (const str of strs) if (str[i] !== "0") { routeDates++; break; }
      }
    }
    if (routeDates !== manifest.counts.routeDates) return full();

    next.v = manifest.v;
    next.t = manifest.t;
    return next;
  } catch {
    return full();
  }
}

function showBanner(text, withRetry) {
  bannerEl.textContent = text;
  if (withRetry) {
    const btn = el(`<button type="button">Retry</button>`);
    btn.addEventListener("click", () => { bannerEl.hidden = true; boot(); });
    bannerEl.append(btn);
  }
  bannerEl.hidden = false;
}
function hideBanner() { bannerEl.hidden = true; bannerEl.textContent = ""; }

/* Polite screen-reader announcer for out-of-band changes (data refresh, page
   navigation) that aren't tied to a focus move. */
let liveRegion = null;
function announce(msg) {
  if (!liveRegion) {
    liveRegion = el(`<div class="sr-only" role="status" aria-live="polite" aria-atomic="true"></div>`);
    document.body.append(liveRegion);
  }
  liveRegion.textContent = "";
  // A microtask gap makes AT re-announce even identical consecutive messages.
  setTimeout(() => { liveRegion.textContent = msg; }, 60);
}

function renderFatal(err) {
  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="empty-state section-pad">
    <div class="big">The availability data didn't load.</div>
    <p>${esc(String(err.message || err))}</p>
    <p><button class="btn" type="button" id="retry-fatal">Try again</button></p>
  </div>`));
  $("#retry-fatal").addEventListener("click", () => location.reload());
}

/* ---------------- router ---------------- */

const current = { page: null, params: null };

function navigate(path) {
  history.pushState(null, "", path);
  route({ focus: true });
}

document.addEventListener("click", (e) => {
  const a = e.target.closest("a");
  if (!a || a.origin !== location.origin || a.target || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  e.preventDefault();
  navigate(a.pathname + a.search);
});
window.addEventListener("popstate", () => route({ focus: true }));

function route({ focus = false } = {}) {
  renderFreshness();
  closeDayPanel();
  const path = location.pathname;
  let m;
  if (path === "/" || path === "") renderHome();
  else if ((m = path.match(/^\/route\/([A-Z]{3})-([A-Z]{3})\/?$/i)))
    renderRoute(m[1].toUpperCase(), m[2].toUpperCase());
  else if ((m = path.match(/^\/trip\/([A-Z]{3})-([A-Z]{3})\/?$/i)))
    renderTrip(m[1].toUpperCase(), m[2].toUpperCase());
  else if ((m = path.match(/^\/from\/([A-Z]{3})\/?$/i)))
    renderFrom(m[1].toUpperCase());
  else if ((m = path.match(/^\/map\/([A-Z]{3})\/?$/i)))
    renderMap(m[1].toUpperCase());
  else if (/^\/alerts\/?$/.test(path)) renderAlerts();
  else renderNotFound();

  // On a real navigation, move focus into the new page so keyboard and screen-
  // reader users are repositioned and the change is announced (title alone is
  // not). Skipped on data-refresh re-renders, which pass no focus flag.
  if (focus && store.bundle) {
    const h1 = $("h1", mainEl);
    if (h1) h1.tabIndex = -1;
    (h1 || mainEl).focus?.({ preventScroll: false });
  }
}

function setTitle(t) { document.title = t ? `${t} — Reward Flights` : "Reward Flights — open award seat availability"; }

/* ---------------- autocomplete ---------------- */

function normalize(s) {
  return s.normalize("NFD").replace(/[̀-ͯ]/g, "").toLowerCase();
}

function matchPlaces(query, restrictTo) {
  const q = normalize(query.trim());
  if (!q) return [];
  const scored = [];
  for (const p of store.placeList) {
    if (restrictTo && !restrictTo.has(p.code)) continue;
    const code = p.code.toLowerCase();
    const name = normalize(p.name);
    let score = -1;
    if (code === q) score = 100;
    else if (name.startsWith(q)) score = 90;
    else if (code.startsWith(q)) score = 80;
    else if (p.search.some((s) => normalize(s).startsWith(q))) score = 70;
    else if (name.includes(q)) score = 50;
    else if (normalize(p.country).startsWith(q)) score = 30;
    else if (p.search.some((s) => normalize(s).includes(q))) score = 25;
    if (score >= 0) scored.push({ p, score });
  }
  scored.sort((a, b) => b.score - a.score || a.p.name.localeCompare(b.p.name));
  return scored.slice(0, 8).map((s) => s.p);
}

/* An input with a suggestion dropdown. Suggestion rows carry a 12-month
   availability sparkline computed in memory — the "calendar as you type"
   moment: by the time you pick, you've already seen the shape of the year. */
function attachAutocomplete(input, { getRestrict, sparkFor, onPick }) {
  const wrap = input.closest(".field");
  const listId = `${input.id}-listbox`;
  const list = el(`<div class="suggest" role="listbox" id="${listId}" hidden></div>`);
  const liveMsg = el(`<div class="sr-only" role="status" aria-live="polite"></div>`);
  wrap.append(list, liveMsg);
  let items = [], active = -1;

  input.setAttribute("role", "combobox");
  input.setAttribute("aria-autocomplete", "list");
  input.setAttribute("aria-expanded", "false");
  input.setAttribute("aria-controls", listId);
  input.autocomplete = "off";
  input.spellcheck = false;

  function close() {
    list.hidden = true;
    input.setAttribute("aria-expanded", "false");
    input.removeAttribute("aria-activedescendant"); // don't leave a dangling ref
    active = -1;
  }
  function open() {
    list.hidden = false; input.setAttribute("aria-expanded", "true");
  }

  function render() {
    const restrict = getRestrict ? getRestrict() : null;
    items = matchPlaces(input.value, restrict);
    list.innerHTML = "";
    if (!items.length) {
      if (input.value.trim()) {
        list.append(el(`<div class="sg-empty">No places match “${esc(input.value.trim())}”${restrict ? " from the chosen origin" : ""}.</div>`));
        liveMsg.textContent = `No places match ${input.value.trim()}`;
        input.removeAttribute("aria-activedescendant");
        open();
      } else close();
      return;
    }
    items.forEach((p, i) => {
      const counts = sparkFor ? sparkFor(p.code) : null;
      const total = counts ? counts.reduce((a, b) => a + b, 0) : 0;
      const max = counts ? Math.max(1, ...counts) : 1;
      const spark = counts
        ? `<span class="sg-spark" aria-hidden="true">${counts.map((c) =>
            `<i style="height:${Math.max(2, Math.round((c / max) * 26))}px"></i>`).join("")}</span>
           <span class="sg-days">${total}d</span>`
        : "";
      // Options are non-focusable (tabindex -1): focus stays on the input and
      // selection is managed via aria-activedescendant, per the combobox pattern.
      // Sparklines are deliberately party-agnostic (monthCounts); say so
      // when a party-size filter is active elsewhere so they don't overpromise.
      const label = counts
        ? `${p.name}, ${p.country}. ${total} days with seats in the next year${activePax() > 1 ? " (any party size)" : ""}.`
        : `${p.name}, ${p.country}`;
      const row = el(`<div class="sg-row" role="option" tabindex="-1" id="${input.id}-opt-${i}"
          aria-label="${esc(label)}">
        <span class="sg-code" aria-hidden="true">${esc(p.code)}</span>
        <span class="sg-name" aria-hidden="true"><span class="nm">${esc(p.name)}</span><br><span class="co">${esc(p.country)}</span></span>
        <span aria-hidden="true" style="display:flex;align-items:center">${spark}</span>
      </div>`);
      row.addEventListener("mousedown", (e) => e.preventDefault()); // keep focus on input
      row.addEventListener("click", () => { close(); onPick(p); });
      list.append(row);
    });
    liveMsg.textContent = `${items.length} place${items.length > 1 ? "s" : ""} found`;
    setActive(0);
    open();
  }

  function setActive(i) {
    active = i;
    [...list.children].forEach((c, j) => {
      const on = j === i;
      c.classList.toggle("active", on);
      if (c.getAttribute("role") === "option") c.setAttribute("aria-selected", on ? "true" : "false");
    });
    const opt = list.children[i];
    if (opt) {
      input.setAttribute("aria-activedescendant", opt.id || "");
      opt.scrollIntoView({ block: "nearest" });
    }
  }

  input.addEventListener("input", render);
  input.addEventListener("focus", () => { if (input.value.trim()) render(); });
  input.addEventListener("blur", () => setTimeout(close, 120));
  input.addEventListener("keydown", (e) => {
    // Tab must leave the field and close the list synchronously (options are
    // not in the tab order, so focus proceeds to the next control cleanly).
    if (e.key === "Tab") { close(); return; }
    if (list.hidden) return;
    if (e.key === "ArrowDown") { e.preventDefault(); setActive(Math.min(active + 1, items.length - 1)); }
    else if (e.key === "ArrowUp") { e.preventDefault(); setActive(Math.max(active - 1, 0)); }
    else if (e.key === "Enter") {
      e.preventDefault();
      // Capture before close(): close() resets `active` to -1, so reading
      // items[active] after it would hand onPick undefined.
      const pick = items[active];
      if (pick) { close(); onPick(pick); }
    }
    else if (e.key === "Escape") close();
  });
}

/* ---------------- pages: home ---------------- */

let homeSel = { origin: null, dest: null };
let homeTripMode = "trip"; // "trip" (round trip, the default) | "route" (one way)

function renderHome() {
  current.page = "home"; current.params = null;
  setTitle(null);
  if (!store.bundle) return; // static skeleton stays until data lands

  mainEl.innerHTML = "";
  const hero = el(`<section class="hero">
    <h1 class="hero-title">Find award seats<br>before they're gone.</h1>
    <p class="hero-sub">A live calendar of British Airways reward-flight availability on every route — built on free, open data.</p>
    <div class="search-card">
      <div class="sc-top">
        <div class="seg" role="group" aria-label="Trip type">
          <button type="button" class="seg-opt${homeTripMode === "trip" ? " on" : ""}" data-mode="trip"
            aria-pressed="${homeTripMode === "trip"}">Round trip</button>
          <button type="button" class="seg-opt${homeTripMode === "route" ? " on" : ""}" data-mode="route"
            aria-pressed="${homeTripMode === "route"}">One way</button>
        </div>
        <div class="home-nights" role="group" aria-label="Trip length in nights"${homeTripMode === "trip" ? "" : " hidden"}>
          ${NIGHTS_PRESETS.map(([label, lo, hi]) =>
            `<button type="button" class="np" data-lo="${lo}" data-hi="${hi}" aria-pressed="false">
               ${esc(label)} <span class="np-r">${lo}–${hi}</span></button>`).join("")}
        </div>
      </div>
      <div class="field">
        <label for="in-origin">From</label>
        <input id="in-origin" placeholder="City or airport" value="${homeSel.origin ? esc(placeName(homeSel.origin)) : ""}">
      </div>
      <button class="swap-btn" type="button" id="swap" title="Swap origin and destination" aria-label="Swap origin and destination">⇄</button>
      <div class="field">
        <label for="in-dest">To</label>
        <input id="in-dest" placeholder="City or airport" value="${homeSel.dest ? esc(placeName(homeSel.dest)) : ""}">
      </div>
    </div>
    <p class="home-secondary" id="home-hint"></p>
  </section>`);
  mainEl.append(hero);

  const originIn = $("#in-origin", hero), destIn = $("#in-dest", hero);

  const originsWithRoutes = new Set(store.origins);
  const allDests = new Set();
  for (const dests of store.destsByOrigin.values()) dests.forEach((d) => allDests.add(d));

  // Trip-type toggle + nights presets. The window rides the same sessionStorage
  // pref as /trip/ (rf:nights) and is carried in the URL, so what you set here
  // is exactly what the round-trip calendar opens with.
  const nightsRow = $(".home-nights", hero);
  const homeNights = () => getNightsPref() || NIGHTS_DEFAULT;
  function syncNights() {
    const [lo, hi] = homeNights();
    for (const b of nightsRow.querySelectorAll(".np")) {
      b.setAttribute("aria-pressed",
        Number(b.dataset.lo) === lo && Number(b.dataset.hi) === hi ? "true" : "false");
    }
  }
  nightsRow.querySelectorAll(".np").forEach((b) => b.addEventListener("click", () => {
    setNightsPref(Number(b.dataset.lo), Number(b.dataset.hi));
    syncNights();
  }));
  syncNights();
  hero.querySelectorAll(".seg-opt").forEach((b) => b.addEventListener("click", () => {
    homeTripMode = b.dataset.mode;
    hero.querySelectorAll(".seg-opt").forEach((x) => {
      const on = x === b;
      x.classList.toggle("on", on);
      x.setAttribute("aria-pressed", on ? "true" : "false");
    });
    nightsRow.hidden = homeTripMode !== "trip";
    updateHint(); // the map link carries ?nights= only in round-trip mode
  }));

  // Both ends picked → go. Unconditionally: the pair may exist in either
  // direction, only the reverse, or not at all — the target page's empty
  // state explains and links onward (/from/, the reverse trip), which beats
  // silently doing nothing here.
  function maybeGo() {
    if (!homeSel.origin || !homeSel.dest) { updateHint(); return; }
    const key = `${homeSel.origin}-${homeSel.dest}`;
    const px = activePax();
    const paxPart = px > 1 ? `pax=${px}` : "";
    if (homeTripMode === "trip") {
      const [lo, hi] = homeNights();
      navigate(`/trip/${key}?nights=${lo}-${hi}${paxPart ? `&${paxPart}` : ""}`);
    } else navigate(`/route/${key}${paxPart ? `?${paxPart}` : ""}`);
  }

  /* The from-only path: the map IS the answer to "where can I go?", so it
     gets the button; the list stays one quiet link away. */
  function mapHref() {
    const parts = [];
    if (homeTripMode === "trip") { const [lo, hi] = homeNights(); parts.push(`nights=${lo}-${hi}`); }
    const px = activePax();
    if (px > 1) parts.push(`pax=${px}`);
    return `/map/${homeSel.origin}${parts.length ? `?${parts.join("&")}` : ""}`;
  }
  function updateHint() {
    const hint = $("#home-hint");
    if (homeSel.origin && !homeSel.dest) {
      const n = (store.destsByOrigin.get(homeSel.origin) || []).length;
      const px = activePax();
      hint.innerHTML = `<a class="btn hint-map" href="${mapHref()}">Where can you go from
          ${esc(placeName(homeSel.origin))}? — open the map</a>
        <a class="hint-list" href="/from/${homeSel.origin}${px > 1 ? `?pax=${px}` : ""}">or list all ${n} destinations</a>`;
    } else hint.textContent = "";
  }

  /* Enter with a From but an empty To answers the question the empty field is
     asking — it opens the map rather than doing nothing. */
  function goMapIfFromOnly(e) {
    if (e.key !== "Enter" || e.defaultPrevented) return;
    if (homeSel.origin && !homeSel.dest && !destIn.value.trim()) navigate(mapHref());
  }

  attachAutocomplete(originIn, {
    getRestrict: () => originsWithRoutes,
    sparkFor: (code) => monthCounts("origin", code),
    onPick: (p) => { homeSel.origin = p.code; originIn.value = p.name; updateHint(); destIn.focus(); maybeGo(); },
  });
  attachAutocomplete(destIn, {
    // Everywhere reachable in one stop, not just direct routes — searching
    // BLL→TYO must resolve to the via-London trip, not "no places match".
    getRestrict: () => homeSel.origin ? reachableDests(homeSel.origin) : allDests,
    // Sparklines only for direct routes: a via pair's year-shape is the
    // chain's, not any single route's — no sparkline beats a wrong one.
    sparkFor: (code) => homeSel.origin
      ? ((store.destsByOrigin.get(homeSel.origin) || []).includes(code)
          ? monthCounts("route", `${homeSel.origin}-${code}`) : null)
      : null,
    onPick: (p) => { homeSel.dest = p.code; destIn.value = p.name; maybeGo(); },
  });
  originIn.addEventListener("input", () => { homeSel.origin = null; updateHint(); });
  destIn.addEventListener("input", () => { homeSel.dest = null; });
  // After the autocomplete's own keydown (which handles list picks): a plain
  // Enter with the list closed reaches these.
  originIn.addEventListener("keydown", goMapIfFromOnly);
  destIn.addEventListener("keydown", goMapIfFromOnly);
  $("#swap", hero).addEventListener("click", () => {
    [homeSel.origin, homeSel.dest] = [homeSel.dest, homeSel.origin];
    [originIn.value, destIn.value] = [destIn.value, originIn.value];
    updateHint(); maybeGo();
  });
  updateHint();

  // Modules mount to their own region so the changes feed can refresh them
  // later without rebuilding (and clearing) the search card above.
  const mount = el(`<div id="home-modules"></div>`);
  mainEl.append(mount);
  buildHomeModules(mount);
}

/* Re-render only the home modules region (recently-opened + deepest + stats),
   leaving the search inputs and their focus/typed text untouched. */
function refreshHomeModules() {
  const mount = $("#home-modules");
  if (mount) buildHomeModules(mount);
}

/* "Your alerts" — the one place to see everything this device is watching and
   switch any of it off. Without this, unsubscribing means remembering every
   route you ever subscribed to and visiting each one. Rendered async (it needs
   the push subscription) and simply absent when there's nothing to show. */
async function buildAlertsPanel(mount) {
  // Paint instantly from the last-known-good copy — waiting on the service
  // worker + a round-trip to the alerts service made this module pop in a
  // second late, the one thing this site promises never to do. The server
  // truth follows and reconciles in place (same pattern as the availability
  // snapshot). data.sub stays null in the fast paint; actions that need it
  // resolve the subscription at click time.
  const cached = Notification?.permission === "granted" ? loadWatchesCache() : null;
  const paint = (data) => {
    if (!data || !data.watches.length) { mount.innerHTML = ""; return; }
    mount.innerHTML = "";
    const sec = el(`<section class="module alerts-mod">
      <h2><span class="dot" aria-hidden="true"></span>Your alerts
        <a class="alerts-manage" href="/alerts">Manage →</a></h2>
      <div class="card-list"></div>
    </section>`);
    const list = $(".card-list", sec);
    const seen = loadSeen();
    for (const w of data.watches) list.append(alertRow(w, data, () => buildAlertsPanel(mount), { seen }));
    mount.append(sec);
  };
  if (cached?.length) paint({ sub: null, watches: cached, device: null });

  const data = await currentAlerts();
  // Reconcile only when the truth differs from what's on screen — a no-op
  // repaint would throw away hover/focus for nothing. (Rows resolve the
  // subscription at click time, so the cached paint is fully functional.)
  const shown = JSON.stringify(cached?.length ? cached : []);
  const actual = JSON.stringify(data?.watches || []);
  if (shown !== actual) paint(data);
}

/* One row describing a watch: what, when, and whether anything matches today.
   Shared by the home panel and the /alerts page. */
function alertRow(w, data, rerender, { editable = false, seen = null } = {}) {
  const [o, d] = w.route.split("-");
  const href = `${w.kind === "rt" ? "/trip" : "/route"}/${w.route}`;
  const arrow = w.kind === "rt" ? "⇄" : "→";
  const cabs = (w.cabins || []).map((c) => CABIN_BIT[c]).filter(Boolean);
  const problem = watchProblem(w);
  const m = problem ? null : matchesNow(w);
  // What's appeared since this device last looked — the news a notification
  // would have carried, shown here in case it never arrived.
  const fresh = problem || !seen ? null : newSinceSeen(w, seen);

  const row = el(`<div class="alert-row${problem ? " dead" : ""}${fresh?.count && fresh.everSeen ? " has-new" : ""}">
    <a class="alert-route" href="${href}">
      <span class="rc-route">${o} <span class="arrow" aria-hidden="true">${arrow}</span> ${d}</span>
      <span class="rc-cities">${esc(placeName(o))} to ${esc(placeName(d))}</span>
    </a>
    <span class="alert-cabs" aria-label="${esc(cabs.map(cabinLabel).join(", "))}">${
      cabs.map((bit) => `<span class="swatch ${bitClass(bit)}" title="${esc(cabinLabel(bit))}"></span>`).join("")
    }</span>
    <span class="alert-kind">${w.kind === "rt" ? "round trip" : "one way"}</span>
    <span class="alert-actions">
      ${editable ? `<a class="alert-edit" href="${href}?alert=1">Edit</a>` : ""}
      <button type="button" class="alert-off" aria-label="Turn off alerts for ${o} to ${d}">Turn off</button>
    </span>
    <span class="alert-when"><span class="aw-cabins">${
      cabs.length === cabinLegend().length ? "All cabins" : esc(cabs.map(cabinLabel).join(", "))
    }</span> · ${esc(watchSummary(w))}</span>
    <span class="alert-state">${problem
      ? `<span class="as-warn">⚠ ${esc(problem)}</span>`
      : fresh && fresh.count && fresh.everSeen
        ? `<span class="as-new">★ ${fresh.count} new since you last looked</span>
           <span class="as-ok">${m.pairs} match now</span>`
        : m.pairs
          ? `<span class="as-ok">● Armed · ${m.pairs} ${w.kind === "rt" ? "round trip" : "date"}${m.pairs > 1 ? "s" : ""} match right now</span>`
          : `<span class="as-idle">● Armed · nothing matches yet — we'll tell you</span>`}
      ${w.lastFiredAt ? `<span class="as-last">Last alert ${esc(timeAgo(w.lastFiredAt))}</span>` : ""}
    </span>
  </div>`);

  $(".alert-off", row).addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true; btn.textContent = "…";
    try {
      // The home panel's fast paint renders from cache with no subscription
      // in hand — resolve it at click time instead of at render time.
      if (!data.sub) data.sub = await getSubscription();
      await saveWatches(data.sub, data.watches.filter((x) => x.id !== w.id));
      announce(`Alerts off for ${o} to ${d}.`);
      refreshAlertCount();
      rerender();
    } catch (err) {
      if (err.queued) {
        // Offline: the change is in the outbox — reflect it, don't fail it.
        announce(`You're offline — alerts for ${o} to ${d} will switch off when you're back online.`);
        data.watches = data.watches.filter((x) => x.id !== w.id);
        rerender();
        return;
      }
      btn.disabled = false; btn.textContent = "Turn off";
      announce(String(err.message || err));
    }
  });
  return row;
}

function cabinLabel(bit) {
  return cabinLegend().find(([b]) => b === bit)?.[1] || "Other";
}

function buildHomeModules(mount) {
  mount.innerHTML = "";
  const alertsMount = el(`<div id="alerts-mod-mount"></div>`);
  mount.append(alertsMount);
  buildAlertsPanel(alertsMount);   // async; renders itself if there's anything
  // The modules honour the SHARED cabin filter (rf:filter — the same pref the
  // maps and calendars read), so "show me Business news" is one row of chips
  // and the choice rides into every page behind the cards.
  const legend = cabinLegend();
  const allMask = legend.reduce((m, [bit]) => m | bit, 0);
  let mask = getFilter() ?? allMask;
  mask &= allMask; if (!mask) mask = allMask;
  const chips = el(`<div class="chips module-chips" role="group" aria-label="Filter the lists below by cabin">${
    legend.map(([bit, label]) => `<button type="button" class="chip" data-bit="${bit}"
      aria-pressed="${!!(mask & bit)}"><span class="swatch ${bitClass(bit)}"></span>${esc(label)}</button>`).join("")}
  </div>`);
  chips.querySelectorAll(".chip").forEach((c) => c.addEventListener("click", () => {
    const next = mask ^ Number(c.dataset.bit);
    if (!next) return; // at least one cabin stays on
    setFilter(next);
    refreshHomeModules(); // rebuilds this row + both lists under the new mask
  }));
  const modules = el(`<div class="modules"></div>`);
  const opened = recentlyOpened(mask);
  if (opened.length) {
    const mod = el(`<section class="module"><h2><span class="dot" aria-hidden="true"></span>Recently opened</h2><div class="card-list"></div></section>`);
    const listEl = $(".card-list", mod);
    for (const g of opened) {
      const [o, d] = g.route.split("-");
      listEl.append(el(`<a class="route-card" href="/trip/${g.route}">
        <span class="rc-route">${o} <span class="arrow" aria-hidden="true">→</span> ${d}</span>
        <span class="rc-cities">${esc(placeName(o))} to ${esc(placeName(d))}</span>
        <span class="rc-meta"><span class="chg-opened">+${g.count} date${g.count > 1 ? "s" : ""}</span><br><span class="when">${esc(timeAgo(g.t))}</span></span>
      </a>`));
    }
    modules.append(mod);
  }
  const top = topRoutes(6, mask);
  if (top.length) {
    const mod = el(`<section class="module"><h2>Deepest availability</h2><div class="card-list"></div></section>`);
    const listEl = $(".card-list", mod);
    for (const { key, total } of top) {
      const [o, d] = key.split("-");
      listEl.append(el(`<a class="route-card" href="/trip/${key}">
        <span class="rc-route">${o} <span class="arrow" aria-hidden="true">→</span> ${d}</span>
        <span class="rc-cities">${esc(placeName(o))} to ${esc(placeName(d))}</span>
        <span class="rc-meta"><b>${total}</b> days<br><span class="when">next 12 months</span></span>
      </a>`));
    }
    modules.append(mod);
  }
  // The chips outlive their own filtering: if the current mask empties both
  // lists, the row must stay (with an honest note) or there'd be no way back.
  if (modules.children.length) mount.append(chips, modules);
  else if ((store.changes?.entries?.length || Object.keys(store.bundle.routes).length) && mask !== allMask) {
    mount.append(chips, el(`<p class="module-empty">Nothing to show in the selected cabins — pick another cabin above.</p>`));
  }

  const routeCount = Object.keys(store.bundle.routes).length;
  let dateCount = 0;
  for (const key of Object.keys(store.bundle.routes)) dateCount += routeTotals(key).total;
  mount.append(el(`<div class="stats-strip">
    <span><b>${routeCount}</b> routes tracked</span>
    <span><b>${dateCount.toLocaleString("en-GB")}</b> dates with seats</span>
    <span><b>${Object.keys(store.bundle.places).length}</b> cities</span>
    <span>data as of ${esc(freshLabel())}</span>
  </div>`));
}

function recentlyOpened(mask = 15) {
  if (!store.changes?.entries) return [];
  // The pinned floor reaches back beyond the contiguous window: per cabin,
  // the newest openings the busy feed has rolled off. With a cabin filter on,
  // "recently" honestly stretches to the last real news — each card carries
  // its own timestamp.
  const src = [...store.changes.entries, ...(store.changes.pinned || [])];
  const byRoute = new Map();
  for (const e of src) {
    if (e.k !== "opened") continue;
    // The entry's c is the date's new cabin set: it matches when any selected
    // cabin is among what opened.
    const bits = [...(e.c || "")].reduce((m, ch) => m | (CABIN_BIT[ch] || 0), 0);
    if (!(bits & mask)) continue;
    const g = byRoute.get(e.r) || { route: e.r, count: 0, t: 0 };
    g.count++; g.t = Math.max(g.t, e.t);
    byRoute.set(e.r, g);
  }
  return [...byRoute.values()]
    .filter((g) => store.bundle.routes[g.route])
    .sort((a, b) => b.t - a.t || b.count - a.count).slice(0, 6);
}

function topRoutes(n, mask = 15) {
  const t0 = Math.max(0, todayIndex());
  return Object.keys(store.bundle.routes)
    .map((key) => {
      const bits = routeBits(key);
      let total = 0;
      for (let i = t0; i < bits.length; i++) if (bits[i] & mask) total++;
      return { key, total };
    })
    .filter((r) => r.total > 0)
    .sort((a, b) => b.total - a.total).slice(0, n);
}

/* ---------------- cabin filter (shared, sticky per session) ---------------- */

function getFilter() {
  try { return JSON.parse(sessionStorage.getItem("rf:filter")) ?? null; } catch { return null; }
}
function setFilter(mask) {
  try { sessionStorage.setItem("rf:filter", JSON.stringify(mask)); } catch {}
}

function cabinChips(perCabin, onChange, chipTitle) {
  const legend = cabinLegend();
  const allMask = legend.reduce((m, [bit]) => m | bit, 0);
  let mask = getFilter() ?? allMask;
  mask &= allMask; if (!mask) mask = allMask;

  const wrap = el(`<div class="cabin-chips" role="group" aria-label="Filter by cabin"></div>`);
  for (const [bit, label] of legend) {
    const count = perCabin?.get(bit) || 0;
    const title = chipTitle ? chipTitle(count, label)
      : (count ? `${count} days with ${label} seats` : `No ${label} seats on this route right now`);
    const chip = el(`<button type="button" class="chip${count ? "" : " none"}" aria-pressed="${!!(mask & bit)}"
      title="${esc(title)}">
      <span class="swatch ${bitClass(bit)}"></span>${esc(label)}
      <span class="n">${count}</span>
    </button>`);
    chip.addEventListener("click", () => {
      mask ^= bit;
      if (!mask) mask = allMask; // never filter everything away
      setFilter(mask);
      [...wrap.children].forEach((c, i) => c.setAttribute("aria-pressed", !!(mask & legend[i][0])));
      onChange(mask);
    });
    wrap.append(chip);
  }
  onChange(mask);
  return wrap;
}

/* ---------------- trip-length window (shared, sticky per session) ---------------- */

const NIGHTS_DEFAULT = [1, 30];
const NIGHTS_PRESETS = [
  ["Short break", 2, 4],
  ["1 week", 5, 9],
  ["2 weeks", 10, 16],
  ["Flexible", 1, 30],
];

function parseNights(s) {
  const m = /^(\d{1,2})-(\d{1,2})$/.exec(s || "");
  if (!m) return null;
  const lo = Number(m[1]), hi = Number(m[2]);
  if (lo < 1 || lo > 60 || hi < 1 || hi > 60 || lo > hi) return null;
  return [lo, hi];
}
function getNightsPref() {
  try { return parseNights(sessionStorage.getItem("rf:nights")); } catch { return null; }
}
function setNightsPref(lo, hi) {
  try { sessionStorage.setItem("rf:nights", `${lo}-${hi}`); } catch {}
}

/* ---------------- party size (shared, sticky per session) ---------------- */

/* "?pax=N": a minimum party size, 2..4 only ("4+" means at least 4 — the top
   threshold the seats layer encodes). Absent means 1, which is exactly
   today's behaviour everywhere. Invalid input degrades silently. */
function parsePax(s) {
  return /^[2-4]$/.test(s || "") ? Number(s) : null;
}
function getPaxPref() {
  try { return parsePax(sessionStorage.getItem("rf:pax")); } catch { return null; }
}
function setPaxPref(n) {
  try { sessionStorage.setItem("rf:pax", String(n)); } catch {}
}

/* The party size in force: URL → session pref → 1. Forced to 1 while the
   adopted bundle carries no seat data anywhere (the control isn't rendered
   then); a URL-borne ?pax stays in the URL but inert — coverage may arrive
   with the next bundle poll and route() re-renders. */
function activePax(params = new URLSearchParams(location.search)) {
  if (!store.hasAnySeatData) return 1;
  return parsePax(params.get("pax")) ?? getPaxPref() ?? 1;
}

/* Mirror a pax change into the URL the same way nights does: replaceState
   (filter tweaks are not history entries), param deleted at the default. */
function mirrorPaxURL(n) {
  const u = new URL(location.href);
  if (n > 1) u.searchParams.set("pax", String(n));
  else u.searchParams.delete("pax");
  const q = u.searchParams.toString();
  history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
}

/* Compact party-size chip row (1 · 2 · 3 · 4+). Rendered ONLY when the
   adopted bundle carries seat data somewhere — a control that can never
   change anything reads as broken. Same pressed-chip pattern as the
   nights presets. */
function paxControl(pax, onChange) {
  // Chips carry their own class (.pxp, styled like .np): the nights presets'
  // sync/press logic — and tests — select .np document- or toolbar-wide, and
  // a pressed party chip must never read as a pressed nights preset.
  const wrap = el(`<div class="pax-ctl" role="group" aria-label="Passengers travelling together">
    <span class="nc-label">Passengers</span>
    ${[1, 2, 3, 4].map((n) => `<button type="button" class="pxp" data-pax="${n}"
      aria-pressed="${n === pax}">${n === 4 ? "4+" : n}</button>`).join("")}
  </div>`);
  wrap.querySelectorAll(".pxp").forEach((b) => b.addEventListener("click", () => {
    const n = Number(b.dataset.pax);
    wrap.querySelectorAll(".pxp").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
    onChange(n);
  }));
  return wrap;
}

/* Trip-length control (presets + custom range), shared by the direct and via
   round-trip views. getNights feeds the pressed state; onApply runs after the
   session pref and URL mirror are written, with the caller updating its own
   window and recounting. */
function nightsControlEl(getNights, onApply) {
  const wrap = el(`<div class="nights-ctl" role="group" aria-label="Trip length in nights">
    <span class="nc-label">Trip length</span>
    ${NIGHTS_PRESETS.map(([label, lo, hi]) =>
      `<button type="button" class="np" data-lo="${lo}" data-hi="${hi}" aria-pressed="false">
         ${esc(label)} <span class="np-r">${lo}–${hi}</span></button>`).join("")}
    <span class="np-custom">
      <input id="np-min" type="number" inputmode="numeric" min="1" max="60" aria-label="Minimum nights">
      <span aria-hidden="true">–</span>
      <input id="np-max" type="number" inputmode="numeric" min="1" max="60" aria-label="Maximum nights">
      <span class="np-unit">nights</span>
    </span>
  </div>`);
  const minIn = $("#np-min", wrap), maxIn = $("#np-max", wrap);
  function sync() {
    const [lo, hi] = getNights();
    for (const b of wrap.querySelectorAll(".np")) {
      b.setAttribute("aria-pressed",
        Number(b.dataset.lo) === lo && Number(b.dataset.hi) === hi ? "true" : "false");
    }
    minIn.value = lo; maxIn.value = hi;
  }
  function apply(lo, hi) {
    setNightsPref(lo, hi);
    const u = new URL(location.href);
    u.searchParams.set("nights", `${lo}-${hi}`);
    history.replaceState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
    onApply(lo, hi);
    sync();
  }
  const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
  function fromInputs(which) {
    const [curLo, curHi] = getNights();
    let lo = clamp(Math.round(Number(minIn.value)) || curLo, 1, 60);
    let hi = clamp(Math.round(Number(maxIn.value)) || curHi, 1, 60);
    if (lo > hi) { if (which === "min") hi = lo; else lo = hi; }
    apply(lo, hi);
  }
  wrap.querySelectorAll(".np").forEach((b) =>
    b.addEventListener("click", () => apply(Number(b.dataset.lo), Number(b.dataset.hi))));
  minIn.addEventListener("change", () => fromInputs("min"));
  maxIn.addEventListener("change", () => fromInputs("max"));
  sync();
  return wrap;
}

/* Connection-window control at the via hub — same chip pattern as the nights
   presets (and the same .np class family deliberately: one visual language
   for "narrow the trip"). */
function connControlEl(hub, getConn, onApply) {
  const wrap = el(`<div class="nights-ctl conn-ctl" role="group" aria-label="Overnight stop at ${esc(placeName(hub))}">
    <span class="nc-label">Stop at ${esc(placeName(hub))}</span>
    ${[["1 night", 1], ["≤2 nights", 2], ["≤3 nights", 3]].map(([label, c]) =>
      `<button type="button" class="np" data-conn="${c}" aria-pressed="false">${label}</button>`).join("")}
  </div>`);
  function sync() {
    for (const b of wrap.querySelectorAll(".np"))
      b.setAttribute("aria-pressed", Number(b.dataset.conn) === getConn() ? "true" : "false");
  }
  wrap.querySelectorAll(".np").forEach((b) => b.addEventListener("click", () => {
    const c = Number(b.dataset.conn);
    setConnPref(c);
    const u = new URL(location.href);
    if (c === 1) u.searchParams.delete("conn"); else u.searchParams.set("conn", String(c));
    const q = u.searchParams.toString();
    history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
    onApply(c);
    sync();
  }));
  sync();
  return wrap;
}

/* The honest per-route fallback line for pax >= 2 on a route whose data has
   no seat counts yet: presence rendering stays, this note says so. Copy rule:
   "no sign of N seats", never "0 seats" — absence means unknown. */
const SEAT_NOTE = "Seat counts aren't in this route's data yet — showing any-party availability.";

/* The current trip-length window as a query string for swap/cross links —
   the window is direction-agnostic, so it survives an origin/dest swap
   (picked dates do not: they belong to the old outbound direction).
   Also carries the party size when one is active, so the filter never
   silently resets on a navigation. */
function nightsQS() {
  const w = parseNights(new URLSearchParams(location.search).get("nights")) || getNightsPref();
  const parts = [];
  if (w) parts.push(`nights=${w[0]}-${w[1]}`);
  const px = activePax();
  if (px > 1) parts.push(`pax=${px}`);
  return parts.length ? `?${parts.join("&")}` : "";
}

/* Valid not-in-the-past day index for a URL-borne ISO date, or -1 (invalid
   params must degrade silently, never crash the page). */
function tripDayIndex(iso) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(iso || "")) return -1;
  const idx = dayIndexOf(iso);
  if (!Number.isInteger(idx) || idx < Math.max(0, todayIndex()) || idx >= store.bundle.days) return -1;
  return isoOf(dayDate(idx)) === iso ? idx : -1; // rejects overflow dates (e.g. 2026-02-31)
}

/* Alert bell for a route. `kind` is "rt" on /trip/ (round trips — the prize)
   or "ow" on /route/.

   Two principles, both load-bearing:
   1. INHERIT, DON'T ASK. By the time the bell is clicked the page already knows
      the cabins (the filter), the trip length, and often a chosen outbound day.
      Read all three; most people should never touch a date field.
   2. SHOW THE ANSWER WHILE THEY CHOOSE. The whole dataset is in memory, so every
      change re-answers "how many trips match right now?" instantly. That is what
      stops a legitimately-silent alert from looking broken, and it catches a
      too-narrow window before it is saved rather than after weeks of nothing. */
function alertBell(routeKey, kind, defaultMask, ctx = {}) {
  const [o, d] = routeKey.split("-");
  const wrap = el(`<div class="bell-wrap"></div>`);
  const btn = el(`<button type="button" class="btn bell-btn" aria-expanded="false"
    aria-haspopup="dialog">🔔 <span class="bell-label">Alert me</span></button>`);
  const pop = el(`<div class="bell-pop" role="dialog" aria-label="Alerts for this route" hidden></div>`);
  wrap.append(btn, pop);

  const legend = cabinLegend();
  const arrow = kind === "rt" ? "⇄" : "→";

  function setLabel(on) {
    $(".bell-label", btn).textContent = on ? "Alerts on" : "Alert me";
    btn.classList.toggle("on", !!on);
  }

  async function refreshLabel() {
    if (!pushSupported() || Notification.permission !== "granted") return;
    try {
      const sub = await getSubscription();
      if (!sub) return;
      const watches = await fetchWatches(sub);
      setLabel(watches.some((w) => w.route === routeKey && w.kind === kind));
    } catch { /* label is cosmetic; never break the page over it */ }
  }
  refreshLabel();

  function close() {
    pop.hidden = true; btn.setAttribute("aria-expanded", "false");
    document.removeEventListener("keydown", onKey);
    document.removeEventListener("click", onOutside, true);
  }
  const onKey = (e) => { if (e.key === "Escape") { close(); btn.focus(); } };
  const onOutside = (e) => { if (!wrap.contains(e.target)) close(); };

  async function open() {
    pop.hidden = false;
    btn.setAttribute("aria-expanded", "true");
    document.addEventListener("keydown", onKey);
    document.addEventListener("click", onOutside, true);

    const closeHTML = `<button type="button" class="bell-x" aria-label="Close" data-close>×</button>`;
    const wireClose = () => { const x = $("[data-close]", pop); if (x) x.addEventListener("click", () => { close(); btn.focus(); }); };
    if (!pushSupported()) {
      pop.innerHTML = closeHTML + (isIOS() && !isStandalone()
        ? `<p class="bell-note"><b>Add Reward Flights to your Home Screen</b> to get alerts on iPhone —
           tap Share, then “Add to Home Screen”, and open it from there.</p>`
        : `<p class="bell-note">This browser doesn't support push notifications.</p>`);
      wireClose();
      return;
    }
    if (Notification.permission === "denied") {
      pop.innerHTML = closeHTML + `<p class="bell-note">${permissionHelpHTML("denied")}</p>`;
      wireClose();
      return;
    }

    pop.innerHTML = `<div class="sk-line" style="height:150px"></div>`;
    let all = [], mine = null, unreachable = false;
    try {
      const sub = await getSubscription();               // no permission prompt yet
      if (sub) all = await fetchWatches(sub);
      mine = all.find((w) => w.route === routeKey && w.kind === kind) || null;
    } catch {
      // Service unreachable (apiFetch gave up after its timeout): fall back
      // to the last-known-good copy so the panel reflects reality, and say
      // so instead of silently presenting a fresh setup over an existing
      // watch.
      unreachable = true;
      all = loadWatchesCache() || [];
      mine = all.find((w) => w.route === routeKey && w.kind === kind) || null;
    }

    renderPanel(mine, all);
    if (unreachable) {
      const warn = el(`<p class="bell-note">The alerts service is unreachable right now — showing
        this device's last copy. Changes will be queued and synced when you're back online.</p>`);
      const title = $(".bell-title", pop);
      if (title) title.after(warn); else pop.prepend(warn);
    }
  }

  function renderPanel(mine, all) {
    // Seed from the existing watch if there is one; otherwise from what the
    // user has already told this page (cabin filter + trip length + picked day).
    const seedMask = mine ? watchMask(mine) : (defaultMask || 15);
    let nights = mine?.nights
      ? [mine.nights.min, mine.nights.max]
      : (kind === "rt" ? (ctx.nights || NIGHTS_ANY).slice() : null);
    let mode = mine?.out ? "exact" : (ctx.pickedOut && !mine ? "around" : "any");
    if (mine?.out && ctx.pickedOut && !mine.ret) mode = "exact";
    let flex = 7;
    let lead = mine?.leadDays || 0;   // "any time, but N days' notice" (rolling)
    let outFrom = mine?.out?.from || "", outTo = mine?.out?.to || "";
    // Party size. The row renders only when this ROUTE can answer "does a
    // party of N fit?" (both legs for a round trip) — a control that can
    // never fire is a broken promise — or when an existing watch already
    // carries a threshold (possibly saved when coverage existed): hiding it
    // then would lock the user out of lowering a dead watch. A fresh watch
    // inherits the page's pax (principle 1: inherit, don't ask).
    const routeHasSeats = kind === "rt"
      ? pairSeatsKnown(routeKey, reverseRoute(routeKey))
      : store.seatRoutes.has(routeKey);
    const partyRowShown = routeHasSeats || (mine?.minSeats || 0) >= 2;
    let party = mine ? (mine.minSeats || 1) : (routeHasSeats ? activePax() : 1);

    pop.innerHTML = "";
    const closeBtn = el(`<button type="button" class="bell-x" aria-label="Close">×</button>`);
    closeBtn.addEventListener("click", () => { close(); btn.focus(); });
    pop.append(
      closeBtn,
      el(`<p class="bell-title">${o} <span class="arrow" aria-hidden="true">${arrow}</span> ${d}</p>`),
      el(`<p class="bell-sub">${mine ? "Alerting you" : "Alert me"} when a
        ${kind === "rt" ? "round trip" : "one-way seat"}${ctx.via ? ` via ${esc(placeName(ctx.via))}` : ""} opens</p>`));

    /* --- cabins --- */
    const cabs = el(`<div class="bell-sec"><h3>Cabins</h3>
      <div class="bell-cabs" role="group" aria-label="Cabins to watch"></div></div>`);
    for (const [bit, label] of legend) {
      const on = !!(seedMask & bit);
      const row = el(`<button type="button" class="bell-cab" role="checkbox" aria-checked="${on}" data-bit="${bit}">
        <span class="swatch ${bitClass(bit)}" aria-hidden="true"></span>
        <span class="bell-cab-label">${esc(label)}</span>
      </button>`);
      row.addEventListener("click", () => {
        row.setAttribute("aria-checked", String(row.getAttribute("aria-checked") !== "true"));
        recount();
      });
      $(".bell-cabs", cabs).append(row);
    }
    pop.append(cabs);

    /* --- trip length (round trips) --- The return is derived from this, so
       showing it here answers "where did the N nights come from?" and lets
       the user change it for the watch (seeded from the page, not written
       back to it). --- */
    if (kind === "rt") pop.append(bellNightsControl());

    /* --- travelling as (party size) --- */
    if (partyRowShown) {
      const partySec = el(`<div class="bell-sec bell-party"><h3>Travelling as</h3>
        <div class="bell-flex" role="group" aria-label="Passengers travelling together">
          ${[1, 2, 3, 4].map((n) => `<button type="button" class="np" data-party="${n}"
            aria-pressed="${n === party}">${n === 4 ? "4+" : n}</button>`).join("")}
        </div>
      </div>`);
      partySec.querySelectorAll(".np").forEach((b) => b.addEventListener("click", () => {
        party = Number(b.dataset.party);
        partySec.querySelectorAll(".np").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
        recount();
      }));
      pop.append(partySec);
    }

    /* --- when can you travel? --- */
    const when = el(`<div class="bell-sec"><h3>When can you travel?</h3>
      <div class="bell-modes" role="radiogroup" aria-label="When can you travel">
        <button type="button" class="bell-mode" role="radio" data-mode="any">Any time</button>
        ${ctx.pickedOut ? `<button type="button" class="bell-mode" role="radio" data-mode="around">Around ${esc(fmtShort.format(dayDate(dayIndexOf(ctx.pickedOut))))}</button>` : ""}
        <button type="button" class="bell-mode" role="radio" data-mode="exact">Exact dates</button>
      </div>
      <div class="bell-when-body"></div>
    </div>`);
    pop.append(when);
    const whenBody = $(".bell-when-body", when);

    const countLine = el(`<p class="bell-count" role="status"></p>`);
    const actions = el(`<div class="bell-actions"></div>`);
    const save = el(`<button type="button" class="btn bell-save">${mine ? "Update alerts" : "Save alerts"}</button>`);
    actions.append(save);
    if (mine) {
      const off = el(`<button type="button" class="bell-offbtn">Turn off</button>`);
      off.addEventListener("click", () => commit(null, off));
      actions.append(off);
    }
    pop.append(countLine, actions);
    const note = el(`<p class="bell-note" role="status"></p>`);
    pop.append(note);

    function setMode(m) {
      mode = m;
      for (const b of when.querySelectorAll(".bell-mode")) {
        const on = b.dataset.mode === m;
        b.setAttribute("aria-checked", String(on));
        b.classList.toggle("on", on);
      }
      drawWhen();
      recount();
    }

    function drawWhen() {
      whenBody.innerHTML = "";
      if (mode === "any") {
        // A realistic middle ground between "any time" and pinned dates: any
        // time, but with enough lead time to actually arrange the trip. It's a
        // rolling floor — "at least N days from whenever an alert fires".
        const opts = [[0, "None"], [3, "3 days"], [7, "1 week"], [14, "2 weeks"], [30, "1 month"]];
        const notice = el(`<div class="bell-notice">
          <p class="bell-when-label">Minimum notice to arrange the trip</p>
          <div class="bell-flex" role="group" aria-label="Minimum notice">
            ${opts.map(([n, lbl]) => `<button type="button" class="np" data-lead="${n}"
              aria-pressed="${n === lead}">${esc(lbl)}</button>`).join("")}
          </div>
          <p class="bell-hint bell-lead-note"></p>
        </div>`);
        notice.querySelectorAll(".np").forEach((b) => b.addEventListener("click", () => {
          lead = Number(b.dataset.lead);
          notice.querySelectorAll(".np").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
          drawLeadNote();
          recount();
        }));
        whenBody.append(notice);
        drawLeadNote();
        return;
      }
      if (mode === "around") {
        const chips = el(`<div class="bell-flex" role="group" aria-label="How flexible are your dates">
          ${[3, 7, 14].map((n) => `<button type="button" class="np" data-flex="${n}"
            aria-pressed="${n === flex}">±${n} days</button>`).join("")}
        </div>`);
        chips.querySelectorAll(".np").forEach((b) => b.addEventListener("click", () => {
          flex = Number(b.dataset.flex);
          chips.querySelectorAll(".np").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
          drawDerived();
          recount();
        }));
        whenBody.append(chips, el(`<p class="bell-hint bell-derived"></p>`));
        drawDerived();
        return;
      }
      // exact
      const t0 = Math.max(0, todayIndex());
      // The picker reaches beyond the released horizon on purpose: exact-date
      // alerts for not-yet-on-sale dates are the point. The watch waits at
      // "nothing matches yet" until BA loads those dates.
      const minIso = idxToIso(t0), maxIso = idxToIso(t0 + EXACT_MAX_AHEAD_DAYS);
      whenBody.append(el(`<div class="bell-dates">
        <label>Leave between
          <span><input type="date" class="bd-of" min="${minIso}" max="${maxIso}" value="${esc(outFrom || defaultOut()[0])}">
          <input type="date" class="bd-ot" min="${minIso}" max="${maxIso}" value="${esc(outTo || defaultOut()[1])}"></span>
        </label>
        ${kind === "rt" ? `<p class="bell-return-note" role="status"></p>` : ""}
      </div>`));
      for (const i of whenBody.querySelectorAll("input")) {
        i.addEventListener("input", () => {
          outFrom = $(".bd-of", whenBody).value; outTo = $(".bd-ot", whenBody).value;
          drawReturnNote();
          recount();
        });
      }
      drawReturnNote();
    }

    /* Trip-length control inside the bell — the return everywhere (any-time
       pairing, the exact-dates derived note) hangs off this, so it belongs
       with the watch, not only on the page. Class-scoped inputs (not the
       page control's np-min/np-max ids). Watch-scoped: it seeds from the page
       but doesn't write the URL/pref back. */
    function bellNightsControl() {
      const wrap = el(`<div class="bell-sec bell-nights"><h3>Trip length</h3>
        <div class="nights-ctl" role="group" aria-label="Trip length in nights">
          ${NIGHTS_PRESETS.map(([label, lo, hi]) =>
            `<button type="button" class="np" data-lo="${lo}" data-hi="${hi}" aria-pressed="false">
               ${esc(label)} <span class="np-r">${lo}–${hi}</span></button>`).join("")}
          <span class="np-custom">
            <input type="number" class="bnp-min" inputmode="numeric" min="1" max="60" aria-label="Minimum nights">
            <span aria-hidden="true">–</span>
            <input type="number" class="bnp-max" inputmode="numeric" min="1" max="60" aria-label="Maximum nights">
            <span class="np-unit">nights</span>
          </span>
        </div></div>`);
      const minIn = $(".bnp-min", wrap), maxIn = $(".bnp-max", wrap);
      const sync = () => {
        for (const b of wrap.querySelectorAll(".np")) {
          b.setAttribute("aria-pressed",
            Number(b.dataset.lo) === nights[0] && Number(b.dataset.hi) === nights[1] ? "true" : "false");
        }
        minIn.value = nights[0]; maxIn.value = nights[1];
      };
      const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
      const apply = (lo, hi) => {
        nights = [lo, hi];
        sync();
        drawReturnNote(); // exact-mode derived return depends on nights
        recount();        // and so does the live match count
      };
      const fromInputs = (which) => {
        let lo = clamp(Math.round(Number(minIn.value)) || nights[0], 1, 60);
        let hi = clamp(Math.round(Number(maxIn.value)) || nights[1], 1, 60);
        if (lo > hi) { if (which === "min") hi = lo; else lo = hi; }
        apply(lo, hi);
      };
      wrap.querySelectorAll(".np").forEach((bb) =>
        bb.addEventListener("click", () => apply(Number(bb.dataset.lo), Number(bb.dataset.hi))));
      minIn.addEventListener("change", () => fromInputs("min"));
      maxIn.addEventListener("change", () => fromInputs("max"));
      sync();
      return wrap;
    }

    /* The return is DERIVED, not entered: you set the leave window and the
       trip length (nights), so we show the resulting return window — with the
       latest possible return called out — for a calendar sanity-check. */
    function drawReturnNote() {
      const note = $(".bell-return-note", whenBody);
      if (!note) return;
      const [minN, maxN] = nights || NIGHTS_ANY;
      const of = outFrom || defaultOut()[0], ot = outTo || defaultOut()[1];
      const ofi = isoToIdx(of), oti = isoToIdx(ot);
      if (!Number.isInteger(ofi) || !Number.isInteger(oti) || oti < ofi) { note.textContent = ""; return; }
      const earliest = idxToIso(ofi + minN), latest = idxToIso(oti + maxN);
      const tripLen = minN === maxN ? `${minN}-night` : `${minN}–${maxN} night`;
      note.innerHTML = `<b class="brn-lead">Home by ${esc(fmtRange(latest, latest))} at the latest.</b>
        You'd return between ${esc(fmtRange(earliest, earliest))} and ${esc(fmtRange(latest, latest))}
        (${tripLen} trips) — worth a check against your calendar.`;
    }

    function defaultOut() {
      const t0 = Math.max(0, todayIndex());
      const anchor = ctx.pickedOut ? dayIndexOf(ctx.pickedOut) : t0;
      return [idxToIso(Math.max(t0, anchor)), idxToIso(Math.min(store.bundle.days - 1, anchor + 30))];
    }

    function drawLeadNote() {
      const p = $(".bell-lead-note", whenBody);
      if (!p) return;
      // Only speak up when a lead is set — the match count below already covers
      // the "any date" case, so the default needs no extra prose.
      p.textContent = lead
        ? `Only trips at least ${leadPhrase(lead).replace("' notice", " ahead")}.`
        : "";
      p.hidden = !lead;
    }

    function drawDerived() {
      const w = build();
      const p = $(".bell-derived", whenBody);
      if (p && w?.out) {
        p.textContent = `Out ${fmtRange(w.out.from, w.out.to)}` +
          (w.kind === "rt" ? ` · back ${fmtRange(w.ret.from, w.ret.to)} · ${w.nights.min}–${w.nights.max} nights` : "");
      }
    }

    /* The watch the panel currently describes (null if no cabins ticked). */
    function build() {
      const cabins = [...pop.querySelectorAll('.bell-cab[aria-checked="true"]')]
        .map((r) => CABIN_CODE[Number(r.dataset.bit)]).filter(Boolean);
      if (!cabins.length) return null;
      const w = { route: routeKey, kind, cabins };
      // A via watch carries its hub + stop length; the cabin list constrains
      // the long-haul legs (the server couples them, hops just need space).
      if (ctx.via) { w.via = ctx.via; w.conn = ctx.conn || 1; }
      if (kind === "rt" && nights) w.nights = { min: nights[0], max: nights[1] };
      // Wire canonical form: minSeats present only when >= 2 (0/absent is the
      // one spelling of "one passenger", which keeps content ids stable).
      if (party >= 2) w.minSeats = party;

      if (mode === "any") {
        if (lead > 0) w.leadDays = lead;   // else fully unbounded ("any time")
      } else if (mode === "around" && ctx.pickedOut) {
        const t0 = Math.max(0, todayIndex()), last = store.bundle.days - 1;
        const a = dayIndexOf(ctx.pickedOut);
        w.out = { from: idxToIso(Math.max(t0, a - flex)), to: idxToIso(Math.min(last, a + flex)) };
        if (kind === "rt") {
          // The return window is implied — don't make them state it twice.
          const rf = Math.min(last, isoToIdx(w.out.from) + w.nights.min);
          const rt = Math.min(last, isoToIdx(w.out.to) + w.nights.max);
          w.ret = { from: idxToIso(rf), to: idxToIso(rt) };
        }
      } else if (mode === "exact") {
        const [df, dt] = defaultOut();
        w.out = { from: outFrom || df, to: outTo || dt };
        // The return is IMPLIED by the outbound window + trip length (nights)
        // — never a separate input. w.ret stays unset; the engine pairs each
        // outbound day D with returns in [D+minNights, D+maxNights], and the
        // panel shows the user the derived latest return to check.
      }
      return w;
    }

    /* Live feedback: what matches right now, plus why it can never match. */
    function recount() {
      const w = build();
      if (!w) {
        countLine.className = "bell-count warn";
        countLine.textContent = "Pick at least one cabin.";
        save.disabled = true;
        return;
      }
      const problem = watchProblem(w);
      if (problem) {
        countLine.className = "bell-count warn";
        countLine.textContent = problem;
        save.disabled = true;
        return;
      }
      save.disabled = false;
      const { pairs } = matchesNow(w);
      const what = kind === "rt" ? "round trip" : "date";
      // Party framing on the count line: "3 round trips match right now for
      // 3 passengers" — the count IS thresholded (matchesNow mirrors the
      // server), so say what it counts.
      const forParty = (w.minSeats || 0) >= 2
        ? ` for ${w.minSeats === 4 ? "4 or more" : w.minSeats} passengers` : "";
      countLine.className = pairs ? "bell-count ok" : "bell-count none";
      countLine.textContent = pairs
        ? `${pairs} ${what}${pairs > 1 ? "s" : ""} match right now${forParty}`
        : `Nothing matches right now${forParty} — we'll tell you the moment something opens.`;
    }

    async function commit(w, btnEl) {
      btnEl.disabled = true;
      const was = btnEl.textContent;
      btnEl.textContent = w ? "Saving…" : "Turning off…";
      note.textContent = "";
      try {
        const sub = await getSubscription({ create: true });
        // Read-modify-write against the SERVER list, falling back to the
        // last-known-good local copy when the network is down. With neither,
        // refuse: editing a guessed list would sync away the other watches.
        const list = (await fetchWatches(sub).catch(() => {
          const cached = loadWatchesCache();
          if (cached === null) {
            throw new Error("Can't reach the alerts service, and this device has no local copy of your alerts to edit safely — try again once you're back online.");
          }
          return cached;
        })).filter((x) => !(x.route === routeKey && x.kind === kind));
        if (w) list.push(w);
        const saved = await saveWatches(sub, list);
        // Editing the party size changes the watch's match set under a NEW
        // content id: re-baseline rf:seen for it (and drop the orphaned old
        // entry) so the next "new since you last looked" diff runs against
        // the edited threshold instead of reporting phantom news.
        if (w && mine && (mine.minSeats || 0) !== (w.minSeats || 0)) {
          const savedW = saved.find((x) => x.route === routeKey && x.kind === kind);
          if (savedW?.id) {
            const seen = loadSeen();
            if (mine.id) delete seen[mine.id];
            seen[savedW.id] = { pairs: matchesNow(savedW, { list: true }).list, t: Math.floor(Date.now() / 1000) };
            saveSeen(seen);
          }
        }
        setLabel(!!w);
        refreshAlertCount();
        note.textContent = w
          ? "Armed. We'll tell you the moment new space opens in your dates."
          : "Alerts off for this route.";
        announce(note.textContent);
        setTimeout(close, 1500);
      } catch (err) {
        if (err instanceof QueuedOffline) {
          // Not a failure: the change is in the outbox and will sync itself.
          setLabel(!!w);
          note.textContent = w
            ? "You're offline — alert saved on this device; it'll sync the moment you're back online."
            : "You're offline — the change is saved on this device and will sync automatically.";
          announce(note.textContent);
          setTimeout(close, 2500);
          return;
        }
        btnEl.disabled = false; btnEl.textContent = was;
        if (err instanceof PermissionError) {
          note.innerHTML = permissionHelpHTML(err.state);
          announce("Your browser hid the notification permission prompt.");
        } else {
          note.textContent = String(err.message || err);
        }
      }
    }

    save.addEventListener("click", () => {
      const w = build();
      if (w) commit(w, save);
    });

    when.querySelectorAll(".bell-mode").forEach((b) =>
      b.addEventListener("click", () => setMode(b.dataset.mode)));
    setMode(mode);
    $(".bell-cab", pop)?.focus();
  }

  btn.addEventListener("click", () => (pop.hidden ? open() : close()));
  // /alerts "Edit" deep-links here with ?alert=1 — open the panel on arrival so
  // editing a watch is one click, using the same component that created it.
  if (new URLSearchParams(location.search).get("alert") === "1") {
    setTimeout(() => { if (pop.hidden) open(); }, 0);
  }
  return wrap;
}

/* ---------------- pages: route ---------------- */

/* Segmented [Round trip | One way]: NAVIGATES between /trip/X-Y and /route/X-Y
   so there is one implementation of each view and the URL stays honest. */
function tripSegHTML(o, d, active) {
  const opt = (href, label, on) =>
    `<a class="seg-opt${on ? " on" : ""}" href="${href}"${on ? ' aria-current="page"' : ""}>${label}</a>`;
  return `<nav class="seg" aria-label="Trip type">
    ${opt(`/trip/${o}-${d}`, "Round trip", active === "trip")}
    ${opt(`/route/${o}-${d}`, "One way", active === "route")}
  </nav>`;
}

/* One-line return-leg depth note — the "can I even get home?" glance. */
function revDepthNoteHTML(o, d) {
  const revKey = `${d}-${o}`;
  const { total, perCabin } = store.bundle.routes[revKey]
    ? routeTotals(revKey) : { total: 0, perCabin: new Map() };
  if (!total) return `<p class="rev-note">Return ${d}→${o}: no award space seen in this snapshot</p>`;
  const parts = cabinLegend()
    .filter(([bit]) => perCabin.get(bit))
    .map(([bit, label]) => `${perCabin.get(bit)} ${esc(label)}`);
  return `<p class="rev-note">Return ${d}→${o}: ${parts.join(" · ")} days · next 12 months</p>`;
}

function renderRoute(o, d) {
  current.page = "route"; current.params = { o, d };
  const key = `${o}-${d}`;
  setTitle(`${o} → ${d}`);
  if (!store.bundle) { renderRouteSkeleton(o, d); return; }

  // Same via upgrade as /trip/: a one-stop chain beats a dead end. One-way
  // only needs the outbound direction to resolve.
  if (!store.bundle.routes[key]) {
    const hub = viaHub(o, d);
    if (hub) { renderViaRoute(o, d, hub); return; }
  }

  mainEl.innerHTML = "";
  const route = store.bundle.routes[key];
  const head = el(`<div class="route-head">
    <p class="crumbs"><a href="/">Search</a> · <a href="/from/${o}">All from ${esc(placeName(o))}</a></p>
    <div class="route-title-row">
      <h1 class="route-title" aria-label="${o} to ${d}">${o} <a class="arrow arrow-swap" href="/route/${d}-${o}"
        title="Flip direction — view ${d} → ${o}" aria-label="Flip direction — view ${d} to ${o}">→</a> ${d}</h1>
      ${tripSegHTML(o, d, "route")}
      <div class="head-actions">
        <a class="btn" href="/route/${d}-${o}" title="View the reverse one-way calendar (${d} to ${o})">⇄ View ${d} → ${o}</a>
      </div>
    </div>
    <p class="route-cities">${esc(placeName(o))}${placeCountry(o) ? `, ${esc(placeCountry(o))}` : ""}
      <span class="via">to</span>
      ${esc(placeName(d))}${placeCountry(d) ? `, ${esc(placeCountry(d))}` : ""}</p>
    ${revDepthNoteHTML(o, d)}
  </div>`);
  mainEl.append(head);

  if (!route) {
    mainEl.append(el(`<div class="empty-state">
      <div class="big">No award seats seen on ${o} → ${d}.</div>
      <p>This route has no reward availability in the current data
         (or isn't flown). It'll appear here as soon as seats open.</p>
      <p><a class="btn" href="/from/${o}">Everywhere with seats from ${esc(placeName(o))}</a></p>
    </div>`));
    return;
  }

  // Party size: URL → session pref → 1, mirrored back like nights on /trip/.
  const params = new URLSearchParams(location.search);
  let pax = activePax(params);
  const hasSeats = store.seatRoutes.has(key);
  // Effective threshold: seats-layer routes filter at the party size; a route
  // without the layer keeps exact presence rendering (plus the honest note).
  const effPax = () => (pax > 1 && hasSeats ? pax : 1);

  const bits = routeBits(key); // presence — stays authoritative for dim/empty
  const container = el(`<div></div>`);
  const toolbar = el(`<div class="route-toolbar"></div>`);
  const paxNote = el(`<p class="pax-note" hidden>${esc(SEAT_NOTE)}</p>`);
  const body = el(`<div></div>`);
  mainEl.append(toolbar, paxNote, container, body);

  let mask = 0, firstDraw = true, chipsEl = null;
  function rebuildChips() {
    const ep = effPax();
    const fresh = cabinChips(routeTotals(key, ep).perCabin, (m) => { mask = m; drawCalendars(); },
      ep > 1 ? (count, label) => (count
        ? `${count} days with ${label} seats for ${pax} travelling together`
        : `No sign of ${pax} ${label} seats together on this route right now`) : null);
    if (chipsEl) chipsEl.replaceWith(fresh); else toolbar.prepend(fresh);
    chipsEl = fresh;
  }
  function syncPaxNote() { paxNote.hidden = !(pax > 1 && !hasSeats); }
  rebuildChips(); // synchronously sets mask + first drawCalendars()
  syncPaxNote();
  if (routeHasNew(key)) {
    toolbar.append(el(`<span class="new-legend"><span class="new-dot" aria-hidden="true"></span>opened in the last 48h</span>`));
  }
  if (store.hasAnySeatData) {
    toolbar.append(paxControl(pax, (n) => {
      pax = n;
      setPaxPref(n);
      mirrorPaxURL(n);
      syncPaxNote();
      rebuildChips(); // chips' onChange redraws the calendars
    }));
  }
  toolbar.append(alertBell(key, "ow", mask));

  function drawCalendars() {
    // The year-strip grow animation is an entrance moment — it plays once,
    // not on every filter toggle.
    const animate = firstDraw;
    firstDraw = false;
    container.innerHTML = ""; body.innerHTML = "";
    const months = next12Months();
    const t0 = todayIndex();
    // Year-strip bars, month counts and lit cells must all read the SAME
    // array, or the strip would visibly disagree with the grid.
    const ep = effPax();
    const tb = ep > 1 ? (routeBitsAtLeast(key, ep) || bits) : bits;
    const paxCtx = ep > 1 ? { pax: ep, userPax: pax, tbits: tb } : (pax > 1 ? { pax: 1, userPax: pax, tbits: bits } : null);

    // Year strip (in-page scroll shortcuts, not tabs → role=group)
    const strip = el(`<div class="year-strip" role="group" aria-label="Jump to month"></div>`);
    months.forEach((mo, mi) => {
      const c = countIn(mo);
      const now = new Date();
      const isCur = mo.y === now.getFullYear() && mo.m === now.getMonth();
      const btn = el(`<button type="button" class="ys-month${isCur ? " current" : ""}" aria-label="${fmtMonth.format(utcDate(mo.y, mo.m, 1))}: ${c} days with seats${ep > 1 ? ` for ${pax} travelling together` : ""}">
        <span class="ys-label">${mo.label}</span>
        <span class="ys-bars" aria-hidden="true"></span>
        <span class="ys-count">${c || "·"}</span>
      </button>`);
      const bars = $(".ys-bars", btn);
      // one bar per cabin present that month, height by its day count
      for (const [bit] of cabinLegend()) {
        if (!(bit & mask)) continue;
        let n = 0;
        for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, tb.length); i++)
          if (tb[i] & bit) n++;
        if (!n) continue;
        const bar = document.createElement("i");
        bar.className = bitClass(bit);
        bar.style.height = `${Math.max(2, Math.round((n / Math.max(1, daysInMonth(mo))) * 30))}px`;
        if (animate) bar.style.animationDelay = `${mi * 30}ms`;
        else bar.style.animation = "none";
        bars.append(bar);
      }
      btn.addEventListener("click", () => {
        const target = $(`#month-${mo.y}-${mo.m}`, body);
        if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
      });
      strip.append(btn);
    });
    container.append(strip);

    // Month grids
    const grid = el(`<div class="months"></div>`);
    for (const mo of months) {
      grid.append(monthCal(key, bits, mo, mask, t0, paxCtx));
    }
    body.append(grid);

    function countIn(mo) {
      let n = 0;
      for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, tb.length); i++)
        if (tb[i] & mask) n++;
      return n;
    }
  }
}

function daysInMonth(mo) { return mo.end - mo.start; }

/* paxCtx (optional): {pax, userPax, tbits} — tbits is the thresholded array
   (same shape as bits) when a party size >= 2 is active on a route with seat
   data. `bits` stays the PRESENCE array: a day with seats but fewer than the
   party must render dim (with honest copy), never as empty. */
/* Beyond the data horizon (the furthest day BA has loaded, ~355 days out)
   there is no data at all — so a blank there means "not on sale yet", not
   "sold out". These render that distinctly on the calendar itself, replacing
   the old blanket note below it. A whole month past the horizon becomes a
   "Not released yet" card; the boundary month keeps its grid with the beyond-
   horizon days hatched. Both are 0-availability by construction (no data). */
function unreleasedMonthCard(mo, first) {
  return el(`<section class="month month-unreleased" id="month-${mo.y}-${mo.m}"
      aria-label="${fmtMonth.format(first)} — not released yet">
    <h3>${fmtMonth.format(first)}</h3>
    <p class="month-unrel">Not released yet</p>
    <p class="month-unrel-sub">British Airways opens award seats about 355 days before departure.
      Set an alert and we'll tell you when these dates go on sale.</p>
  </section>`);
}
function unreleasedDayCell(dnum, idx) {
  return el(`<span class="day unreleased"
      aria-label="${esc(fmtDate.format(dayDate(idx)))} — not released yet"
      title="Not released yet — BA opens award seats about 355 days ahead">
    <span class="num">${dnum}</span></span>`);
}
/* Caption for a month that STRADDLES the horizon: it renders a grid (so the
   hatched cells alone would have no explanation on this, the last month), so
   spell it out. idx is the first unreleased day of the month. */
function unreleasedCaptionEl(idx) {
  const d = dayDate(idx);
  return el(`<p class="month-unrel-inline">Dates from ${d.getUTCDate()} ${fmtMonthShort.format(d)}
    aren't released yet — BA opens award seats about 355 days ahead. Set an alert to hear when they open.</p>`);
}

function monthCal(routeKey, bits, mo, mask, t0, paxCtx = null) {
  const first = utcDate(mo.y, mo.m, 1);
  const tbits = paxCtx ? paxCtx.tbits : bits;
  const pax = paxCtx ? paxCtx.pax : 1;
  const horizon = bits.length; // last released day; beyond it = not on sale yet
  if (mo.start >= horizon) return unreleasedMonthCard(mo, first);

  // Months with nothing to show collapse to a compact card — a full grid of
  // empty cells is noise when the answer is simply "no". The collapse check
  // is presence-based on purpose: under-threshold days render as dim cells.
  // A month straddling the horizon never collapses: its unreleased tail must
  // stay visible (and distinct from the sold-out days before it).
  let any = false, anyShown = false;
  for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, bits.length); i++) {
    if (bits[i]) { any = true; if (bits[i] & mask) { anyShown = true; break; } }
  }
  if (!anyShown && mo.end <= horizon) {
    return el(`<section class="month month-compact" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
      <h3>${fmtMonth.format(first)}</h3>
      <p class="month-empty">${any ? "No seats in the selected cabins" : "No seats this month"}</p>
    </section>`);
  }

  const box = el(`<section class="month" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
    <h3>${fmtMonth.format(first)} <span class="mc"></span></h3>
    <div class="dow">${["Mo","Tu","We","Th","Fr","Sa","Su"].map((d) => `<span>${d}</span>`).join("")}</div>
    <div class="grid"></div>
  </section>`);
  const grid = $(".grid", box);
  const lead = (first.getUTCDay() + 6) % 7; // Monday-first
  for (let i = 0; i < lead; i++) grid.append(el(`<span class="day pad"></span>`));

  let monthDays = 0;
  const nDays = daysInMonth(mo);
  for (let dnum = 1; dnum <= nDays; dnum++) {
    const idx = mo.start + dnum - 1;
    if (idx >= horizon) { grid.append(unreleasedDayCell(dnum, idx)); continue; }
    const v = (idx >= 0 && idx < bits.length) ? bits[idx] : 0;
    const tv = (idx >= 0 && idx < tbits.length) ? tbits[idx] : 0;
    const shown = tv & mask;
    const isPast = idx < t0;
    if (!v || isPast) {
      grid.append(el(`<span class="day${isPast ? " past" : ""}"><span class="num">${dnum}</span><span class="stack"></span></span>`));
      continue;
    }
    monthDays += shown ? 1 : 0;
    const iso = isoOf(dayDate(idx));
    const fresh = isNew(routeKey, iso);
    // A day open in your cabins but without evidence of enough seats for the
    // party dims (never empties), and says why.
    const lowSeats = pax > 1 && !shown && (v & mask);
    const aria = lowSeats
      ? `${esc(fmtDate.format(dayDate(idx)))}: ${esc(cabNames(v))} open — no sign of ${pax} seats together${fresh ? ", newly opened" : ""}`
      : `${esc(fmtDate.format(dayDate(idx)))}: ${esc(cabNames(v))} available${shown && pax > 1 ? ` for ${pax} travelling together` : ""}${fresh ? ", newly opened" : ""}`;
    // Lit day: lanes show the cabins passing the filter. Dim day (space only
    // in filtered-out cabins, or not enough seats for the party): lanes show
    // what's there, grayed by CSS.
    const cell = el(`<button type="button" class="day has${shown ? "" : " dim"}${fresh ? " new" : ""}"
        aria-label="${aria}">
      <span class="num">${dnum}</span>
      ${stackHTML(shown || v)}
    </button>`);
    const tipNote = lowSeats ? `no sign of ${pax} seats together` : null;
    const userPax = paxCtx ? paxCtx.userPax : 1;
    cell.addEventListener("click", () => openDayPanel(routeKey, idx, userPax));
    cell.addEventListener("mouseenter", (e) => showTip(e.currentTarget, idx, bits[idx], tipNote));
    cell.addEventListener("mouseleave", hideTip);
    cell.addEventListener("focus", (e) => showTip(e.currentTarget, idx, bits[idx], tipNote));
    cell.addEventListener("blur", hideTip);
    grid.append(cell);
  }
  $(".mc", box).textContent = `${monthDays}d`;
  if (mo.end > horizon) box.append(unreleasedCaptionEl(horizon));
  return box;
}

function cabNames(v) {
  return cabinLegend().filter(([bit]) => v & bit).map(([, label]) => label).join(", ") || "no cabins";
}

function isNew(routeKey, iso) {
  if (!store.changes?.entries) return false;
  const cutoff = Date.now() / 1000 - NEW_BADGE_MS / 1000;
  return store.changes.entries.some((e) =>
    e.k === "opened" && e.r === routeKey && e.d === iso && e.t >= cutoff);
}

function routeHasNew(routeKey) {
  if (!store.changes?.entries) return false;
  const cutoff = Date.now() / 1000 - NEW_BADGE_MS / 1000;
  return store.changes.entries.some((e) => e.k === "opened" && e.r === routeKey && e.t >= cutoff);
}

function renderRouteSkeleton(o, d, arrow = "→") {
  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="route-head">
    <div class="route-title-row"><h1 class="route-title">${esc(o)} <span class="arrow">${arrow}</span> ${esc(d)}</h1></div>
    <div class="months" style="margin-top:34px">${Array.from({ length: 3 }, () => `
      <section class="month"><div class="sk-line" style="height:18px;width:60%;margin-bottom:12px"></div>
      <div class="grid">${Array.from({ length: 35 }, () => `<span class="day sk-cell"></span>`).join("")}</div></section>`).join("")}
    </div>
  </div>`));
}

/* ---------------- pages: trip (round trip — the default surface) ---------------- */

function renderTrip(o, d) {
  current.page = "trip"; current.params = { o, d };
  const key = `${o}-${d}`, revKey = `${d}-${o}`;
  setTitle(`${o} ⇄ ${d}`);
  if (!store.bundle) { renderRouteSkeleton(o, d, "⇄"); return; }

  // No direct route but a one-stop routing exists both ways → the via-trip
  // calendar (BLL→TYO becomes BLL→LON→TYO), not an empty state.
  if (!store.bundle.routes[key]) {
    const hub = viaHub(o, d, { both: true });
    if (hub) { renderViaTrip(o, d, hub); return; }
  }

  mainEl.innerHTML = "";
  const routeData = store.bundle.routes[key];
  mainEl.append(el(`<div class="route-head">
    <p class="crumbs"><a href="/">Search</a> · <a href="/from/${o}">All from ${esc(placeName(o))}</a></p>
    <div class="route-title-row">
      <h1 class="route-title" aria-label="${o} to ${d} round trip">${o} <a class="arrow arrow-swap" href="/trip/${d}-${o}${nightsQS()}"
        title="Swap — plan ${d} ⇄ ${o} instead" aria-label="Swap origin and destination — plan ${d} to ${o} round trips">⇄</a> ${d}</h1>
      ${tripSegHTML(o, d, "trip")}
    </div>
    <p class="route-cities">${esc(placeName(o))}${placeCountry(o) ? `, ${esc(placeCountry(o))}` : ""}
      <span class="via">to</span>
      ${esc(placeName(d))}${placeCountry(d) ? `, ${esc(placeCountry(d))}` : ""}</p>
    ${revDepthNoteHTML(o, d)}
  </div>`));

  if (!routeData) {
    const revExists = !!store.bundle.routes[revKey];
    mainEl.append(el(`<div class="empty-state">
      <div class="big">No award seats seen on ${o} → ${d}.</div>
      <p>A round trip needs award space on both legs, and the outbound leg has
         none in the current data${revExists ? ` — though the other direction does` : ""}.</p>
      <p>${revExists ? `<a class="btn" href="/trip/${d}-${o}">View ${d} ⇄ ${o} round trips</a> ` : ""}
         <a class="btn" href="/from/${o}">Everywhere with seats from ${esc(placeName(o))}</a></p>
    </div>`));
    return;
  }

  // URL params win; sessionStorage prefs fill in; invalid input degrades.
  const params = new URLSearchParams(location.search);
  let nights = parseNights(params.get("nights")) || getNightsPref() || NIGHTS_DEFAULT.slice();
  // Party size rides the same seed order (?pax= → rf:pax → 1) and the same
  // replaceState mirroring as nights.
  let pax = activePax(params);
  // A round trip can only be seat-filtered when BOTH legs carry the seats
  // layer; otherwise everything renders at any-party presence + the note.
  const pairKnown = pairSeatsKnown(key, revKey);
  const ep = () => (pax > 1 && pairKnown ? pax : 1);

  const outBits = routeBits(key);
  const t0 = todayIndex();
  const legend = cabinLegend();
  const allMask = legend.reduce((m, [bit]) => m | bit, 0);

  const toolbar = el(`<div class="route-toolbar"></div>`);
  const paxNote = el(`<p class="pax-note" hidden>${esc(SEAT_NOTE)}</p>`);
  const container = el(`<div></div>`);
  const body = el(`<div></div>`);
  mainEl.append(toolbar, paxNote, container, body);

  let mask = 0, firstDraw = true, chipsEl = null;

  /* Chip counts are ROUND-TRIPPABLE outbound days (recounted from the
     engine), not raw one-way availability. */
  function perCabinRT() {
    const rt = roundTripBits(key, revKey, allMask, nights[0], nights[1], ep());
    const per = new Map();
    for (let i = Math.max(0, t0); i < rt.length; i++) {
      const v = rt[i];
      if (!v) continue;
      for (const [bit] of legend) if (v & bit) per.set(bit, (per.get(bit) || 0) + 1);
    }
    return per;
  }

  function rebuildChips() {
    const forN = ep() > 1 ? ` for ${pax} travelling together` : "";
    const fresh = cabinChips(perCabinRT(), (m) => { mask = m; drawCalendars(); },
      (count, label) => count
        ? `${count} outbound days with a same-cabin ${label} return${forN}`
        : `No ${label} round trips${forN} within ${nights[0]}–${nights[1]} nights`);
    if (chipsEl) chipsEl.replaceWith(fresh); else toolbar.prepend(fresh);
    chipsEl = fresh;
  }
  function syncPaxNote() { paxNote.hidden = !(pax > 1 && !pairKnown); }

  toolbar.append(nightsControlEl(() => nights, (lo, hi) => {
    nights = [lo, hi];
    rebuildChips(); // recounts chips; chips' onChange redraws the calendars
  }));
  if (store.hasAnySeatData) {
    toolbar.append(paxControl(pax, (n) => {
      pax = n;
      setPaxPref(n);
      mirrorPaxURL(n);
      syncPaxNote();
      rebuildChips(); // recounts chips; chips' onChange redraws the calendars
    }));
  }
  rebuildChips(); // synchronously sets mask + first drawCalendars()
  syncPaxNote();
  // The prize: "tell me when a Business round trip opens on this pair". The
  // bell inherits the trip length and the day they've clicked, so most people
  // never touch a date field.
  toolbar.append(alertBell(key, "rt", mask, {
    get nights() { return nights.slice(); },
    get pickedOut() { return params.get("out") || null; },
  }));

  function drawCalendars() {
    const animate = firstDraw;
    firstDraw = false;
    container.innerHTML = ""; body.innerHTML = "";
    const n = ep();
    const rb = roundTripBits(key, revKey, mask, nights[0], nights[1], n);
    // All-cabin recount alongside the masked one, so dim cells can honestly
    // distinguish "no same-cabin return in the window" from "a round trip
    // exists — your cabin filter is hiding it".
    const rbAll = mask === allMask ? rb : roundTripBits(key, revKey, allMask, nights[0], nights[1], n);
    // Third honesty axis at pax >= 2: an any-party recount so a dim cell can
    // say "a round trip exists for a smaller party" distinctly from "hidden
    // by your cabin filter" and "no return in window". Each recount runs at
    // ONE consistent threshold — mixing produces lying labels.
    const rbAny = n > 1 ? roundTripBits(key, revKey, allMask, nights[0], nights[1], 1) : null;
    const months = next12Months();

    // Year strip: recounted from roundBits, like every number on this page.
    const strip = el(`<div class="year-strip" role="group" aria-label="Jump to month"></div>`);
    months.forEach((mo, mi) => {
      const c = countIn(mo);
      const now = new Date();
      const isCur = mo.y === now.getFullYear() && mo.m === now.getMonth();
      const btn = el(`<button type="button" class="ys-month${isCur ? " current" : ""}" aria-label="${fmtMonth.format(utcDate(mo.y, mo.m, 1))}: ${c} days with round trips${n > 1 ? ` for ${pax} travelling together` : ""}">
        <span class="ys-label">${mo.label}</span>
        <span class="ys-bars" aria-hidden="true"></span>
        <span class="ys-count">${c || "·"}</span>
      </button>`);
      const bars = $(".ys-bars", btn);
      for (const [bit] of legend) {
        if (!(bit & mask)) continue;
        let n = 0;
        for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, rb.length); i++)
          if (rb[i] & bit) n++;
        if (!n) continue;
        const bar = document.createElement("i");
        bar.className = bitClass(bit);
        bar.style.height = `${Math.max(2, Math.round((n / Math.max(1, daysInMonth(mo))) * 30))}px`;
        if (animate) bar.style.animationDelay = `${mi * 30}ms`;
        else bar.style.animation = "none";
        bars.append(bar);
      }
      btn.addEventListener("click", () => {
        const target = $(`#month-${mo.y}-${mo.m}`, body);
        if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
      });
      strip.append(btn);
    });
    container.append(strip);

    const grid = el(`<div class="months"></div>`);
    for (const mo of months) grid.append(monthCalTrip(mo, rb, rbAll, rbAny, n));
    body.append(grid);

    function countIn(mo) {
      let n = 0;
      for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, rb.length); i++)
        if (rb[i]) n++;
      return n;
    }
  }

  /* Round-trip month: LIT = round-trippable (stack shows the cabins you can
     complete the trip in), DIM = outbound space but no same-cabin return in
     the window (still clickable — the panel explains and offers one-ways),
     empty = no outbound space at all. A dim day whose round trip exists only
     in filtered-out cabins says so — never "no return" when one is there.
     At a party size >= 2 (n > 1) a third dim cause exists: a round trip is
     open for a smaller party but there's no sign of n seats together —
     rbAny (the any-party recount) tells them apart. The month-compact check
     stays presence-based so a month never collapses to "no outbound seats"
     when the truth is "not enough seats". */
  function monthCalTrip(mo, rb, rbAll, rbAny, n) {
    const first = utcDate(mo.y, mo.m, 1);
    const horizon = outBits.length; // beyond it = not released yet, not sold out
    if (mo.start >= horizon) return unreleasedMonthCard(mo, first);
    let anyOut = false;
    for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, outBits.length); i++) {
      if (outBits[i]) { anyOut = true; break; }
    }
    if (!anyOut && mo.end <= horizon) {
      return el(`<section class="month month-compact" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
        <h3>${fmtMonth.format(first)}</h3>
        <p class="month-empty">No outbound seats this month</p>
      </section>`);
    }

    const box = el(`<section class="month" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
      <h3>${fmtMonth.format(first)} <span class="mc"></span></h3>
      <div class="dow">${["Mo","Tu","We","Th","Fr","Sa","Su"].map((dw) => `<span>${dw}</span>`).join("")}</div>
      <div class="grid"></div>
    </section>`);
    const grid = $(".grid", box);
    const lead = (first.getUTCDay() + 6) % 7; // Monday-first
    for (let i = 0; i < lead; i++) grid.append(el(`<span class="day pad"></span>`));

    let monthDays = 0;
    const nDays = daysInMonth(mo);
    for (let dnum = 1; dnum <= nDays; dnum++) {
      const idx = mo.start + dnum - 1;
      if (idx >= horizon) { grid.append(unreleasedDayCell(dnum, idx)); continue; }
      const vOut = (idx >= 0 && idx < outBits.length) ? outBits[idx] : 0;
      const v = (idx >= 0 && idx < rb.length) ? rb[idx] : 0;
      const isPast = idx < t0;
      if (!vOut || isPast) {
        grid.append(el(`<span class="day${isPast ? " past" : ""}"><span class="num">${dnum}</span><span class="stack"></span></span>`));
        continue;
      }
      if (v) monthDays++;
      const dateLabel = esc(fmtDate.format(dayDate(idx)));
      const hiddenBits = v ? 0 : ((idx >= 0 && idx < rbAll.length) ? rbAll[idx] : 0);
      // Only consulted when the day fails at the party threshold outright:
      // does a round trip exist here for ANY party size?
      const smallerParty = !v && !hiddenBits && n > 1 && rbAny
        ? ((idx >= 0 && idx < rbAny.length) ? rbAny[idx] : 0) : 0;
      const aria = v
        ? `${dateLabel}: round trip available in ${esc(cabNames(v))}${n > 1 ? ` for ${pax} travelling together` : ""}`
        : hiddenBits
          ? `${dateLabel}: round trip available in ${esc(cabNames(hiddenBits))} — hidden by your cabin filter`
          : smallerParty
            ? `${dateLabel}: round trip open in ${esc(cabNames(smallerParty))} for a smaller party — no sign of ${pax} seats together`
            : `${dateLabel}: outbound only — no return within ${nights[0]}–${nights[1]} nights`;
      const cell = el(`<button type="button" class="day has${v ? "" : " dim"}" aria-label="${aria}">
        <span class="num">${dnum}</span>
        ${stackHTML(v || vOut)}
      </button>`);
      const tipNote = v ? null : hiddenBits
        ? `round trip open in ${cabNames(hiddenBits)} — hidden by your cabin filter`
        : smallerParty
          ? `round trip open for a smaller party — no sign of ${pax} seats together`
          : "outbound only — no same-cabin return in window";
      cell.addEventListener("click", () => pickOutbound(idx));
      cell.addEventListener("mouseenter", (e) => showTip(e.currentTarget, idx, v, tipNote));
      cell.addEventListener("mouseleave", hideTip);
      cell.addEventListener("focus", (e) => showTip(e.currentTarget, idx, v, tipNote));
      cell.addEventListener("blur", hideTip);
      grid.append(cell);
    }
    $(".mc", box).textContent = `${monthDays}d`;
    if (mo.end > horizon) box.append(unreleasedCaptionEl(horizon));
    return box;
  }

  /* Picking an outbound day is a history entry (Back = undo the pick). */
  function pickOutbound(idx) {
    const iso = isoOf(dayDate(idx));
    const u = new URL(location.href);
    u.searchParams.set("out", iso);
    u.searchParams.delete("ret");
    history.pushState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
    openPairPanel(o, d, idx, nights, mask, null, pax);
  }

  // A pinned outbound in the URL (?out=…) restores the pair panel; an
  // invalid or past date, or a day with no outbound space, degrades silently.
  // The gate is presence-based on purpose: a shared pax=4 link onto a day
  // that only seats 2 still opens the panel, which explains — it never
  // silently drops the pick.
  const outIdx = tripDayIndex(params.get("out") || "");
  if (outIdx >= 0 && outBits[outIdx]) {
    const retP = params.get("ret") || "";
    openPairPanel(o, d, outIdx, nights, mask, tripDayIndex(retP) >= 0 ? retP : null, pax);
  }
}

/* ---------------- tooltip ---------------- */

let tipEl = null;
function showTip(anchor, idx, v, note) {
  hideTip();
  const legend = cabinLegend().filter(([bit]) => v & bit);
  tipEl = el(`<div class="tip" role="tooltip">
    <div class="t-date">${fmtDate.format(dayDate(idx))}</div>
    ${legend.map(([bit, label]) =>
      `<div class="t-cab"><span class="swatch ${bitClass(bit)}"></span>${esc(label)}</div>`).join("")}
    ${note ? `<div class="t-note">${esc(note)}</div>` : ""}
  </div>`);
  document.body.append(tipEl);
  const r = anchor.getBoundingClientRect(), tr = tipEl.getBoundingClientRect();
  let x = r.left + r.width / 2 - tr.width / 2;
  x = Math.max(8, Math.min(x, innerWidth - tr.width - 8));
  let y = r.top - tr.height - 8;
  if (y < 8) y = r.bottom + 8;
  tipEl.style.left = `${x}px`; tipEl.style.top = `${y}px`;
}
function hideTip() { tipEl?.remove(); tipEl = null; }

/* ---------------- day detail panel ---------------- */

const panelEl = $("#day-panel"), scrimEl = $("#scrim");
let panelReturnFocus = null;
/* Run only on USER-initiated closes (×, Esc, scrim, mobile step-1) — never on
   the programmatic close that route() performs mid-navigation, which would
   mangle the freshly-pushed URL. */
let panelCloseHook = null;

function openDayPanel(routeKey, idx, pax = 1) {
  const [o, d] = routeKey.split("-");
  const iso = isoOf(dayDate(idx));
  const bits = routeBits(routeKey);
  panelReturnFocus = document.activeElement;
  panelCloseHook = null;

  // At a party size >= 2, annotate each cabin row with whether the seats
  // layer shows the party fitting on one flight (threshold codes, no extra
  // fetching); a route without the layer gets one honest caveat line.
  const codes = pax > 1 ? seatCodes(routeKey, idx) : null;
  const paxMark = (bit) => {
    if (pax <= 1) return "";
    if (!codes) return "";
    return codes[bit] >= pax - 1
      ? `<span class="dp-cab-pax fits">seats for ${pax}</span>`
      : `<span class="dp-cab-pax">no sign of ${pax} seats together</span>`;
  };
  const legend = cabinLegend().filter(([bit]) => bits[idx] & bit);
  panelEl.setAttribute("role", "dialog");
  panelEl.setAttribute("aria-modal", "true");
  panelEl.setAttribute("aria-labelledby", "dp-title");
  panelEl.innerHTML = "";
  panelEl.append(el(`<div>
    <button class="dp-close" type="button" aria-label="Close">×</button>
    <div class="dp-head">
      <div>
        <p class="dp-date">${esc(fmtDate.format(dayDate(idx)))}</p>
        <p class="dp-route" id="dp-title">${o} <span style="color:var(--gold)" aria-hidden="true">→</span> ${d}</p>
      </div>
      ${stackHTML(bits[idx], { size: "row" })}
    </div>
    <p class="dp-lead">Award space seen in this snapshot — search British Airways to book:</p>
    ${pax > 1 && !codes ? `<p class="pax-note">${esc(SEAT_NOTE)}</p>` : ""}
    <div class="dp-cabs">${legend.map(([bit, label]) => `
      <a class="dp-cab" data-bit="${bit}" target="_blank" rel="noopener noreferrer"
         href="${baBookingURL(o, d, iso, bit, null, pax)}">
        <span class="swatch ${bitClass(bit)}" aria-hidden="true"></span>
        <span class="dp-cab-label">${esc(label)}${paxMark(bit)}</span>
        <span class="dp-cab-go">Search one way ↗</span>
      </a>`).join("")}
    </div>
    <p class="dp-trip-link">Planning a round trip?
      <a href="/trip/${o}-${d}">${o} ⇄ ${d} round-trip calendar →</a></p>
    <div class="dp-flights" id="dp-flights"></div>
    <p class="dp-note">Links open BA's Avios redemption search for the whole city pair.
      Seen in data as of ${esc(freshLabel())}. Availability moves fast — verify before planning.</p>
  </div>`));
  showPanelChrome();
  loadFlightDetail(o, d, iso);
}

/* ---------------- pair-picker panel (round trips) ---------------- */

/* The /trip/ day panel: the outbound day is pinned, every in-window return is
   a radio row (shares-a-selected-cabin first, then nights ascending), and the
   sticky summary offers one CTA per cabin open on BOTH legs — BA applies one
   CabinCode to the whole booking, so a link is never offered for a cabin
   missing on either leg (no empty-BA-result deep links, ever). */
function openPairPanel(o, d, idx, nights, mask, retIso = null, pax = 1) {
  const key = `${o}-${d}`, revKey = `${d}-${o}`;
  const outBitsAll = routeBits(key);
  const vOut = outBitsAll[idx];
  const [lo, hi] = nights;
  const outIso = isoOf(dayDate(idx));
  // Party-size annotation inputs (threshold codes from the bundle, no extra
  // fetching): null when either leg lacks the seats layer — then rows keep
  // today's presence rendering plus one honest caveat line.
  const outTh = pax > 1 ? routeBitsAtLeast(key, pax) : null;
  const revTh = pax > 1 ? routeBitsAtLeast(revKey, pax) : null;
  const paxKnown = !!(outTh && revTh);
  const vOutTh = paxKnown ? outTh[idx] : 0;
  panelReturnFocus = document.activeElement;
  panelEl.setAttribute("role", "dialog");
  panelEl.setAttribute("aria-modal", "true");
  panelEl.setAttribute("aria-labelledby", "dp-title");
  panelEl.innerHTML = "";
  // A user close keeps the calendar but unpins the picks from the URL, so a
  // refresh doesn't resurrect a panel the user dismissed.
  panelCloseHook = () => {
    const u = new URL(location.href);
    if (!u.searchParams.has("out") && !u.searchParams.has("ret")) return;
    u.searchParams.delete("out"); u.searchParams.delete("ret");
    const q = u.searchParams.toString();
    history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
  };

  const revBits = store.bundle.routes[revKey] ? routeBits(revKey) : null;
  const rows = [];
  if (revBits) {
    const rEnd = Math.min(revBits.length - 1, idx + hi);
    for (let r = idx + Math.max(1, lo); r <= rEnd; r++) {
      if (revBits[r]) {
        rows.push({
          idx: r, iso: isoOf(dayDate(r)), bits: revBits[r], n: r - idx,
          shares: (revBits[r] & vOut & mask) ? 1 : 0,
          // Cabins with evidence of >= pax seats on BOTH legs (0 when the
          // seats layer is absent — unknown, not "none").
          fit: paxKnown ? (revTh[r] & vOutTh) : 0,
        });
      }
    }
  }
  rows.sort((a, b) => b.shares - a.shares || a.n - b.n);
  const anyPair = rows.some((r) => r.shares);

  if (!anyPair) {
    // No round trip under the CURRENT filter — but be honest about why: a
    // same-cabin round trip may exist in cabins the user has deselected
    // (offer to show all cabins), or truly not exist within this window
    // (say so plainly, offer the one-ways, hint the widen fix).
    const pairAllBits = rows.reduce((acc, r) => acc | (r.bits & vOut), 0);
    const legend = cabinLegend().filter(([bit]) => vOut & bit);
    const alreadyWide = lo === NIGHTS_DEFAULT[0] && hi === NIGHTS_DEFAULT[1];
    const lead = pairAllBits
      ? `A same-cabin round trip is open in ${esc(cabNames(pairAllBits))} —
        it's hidden by your cabin filter.`
      : `Outbound award space is open in ${esc(cabNames(vOut))}, but there's
        no same-cabin return within ${lo}–${hi} nights.`;
    const fixHint = pairAllBits
      ? `<p><button type="button" class="btn" id="pp-allcabs">Show all cabins</button></p>`
      : alreadyWide ? ""
        : `<p><button type="button" class="btn" id="pp-widen">Widen trip length to ${NIGHTS_DEFAULT[0]}–${NIGHTS_DEFAULT[1]} nights</button></p>`;
    panelEl.append(el(`<div>
      <button class="dp-close" type="button" aria-label="Close">×</button>
      <div class="dp-head">
        <div>
          <p class="dp-date">${esc(fmtDate.format(dayDate(idx)))}</p>
          <p class="dp-route" id="dp-title">${o} <span style="color:var(--gold)" aria-hidden="true">⇄</span> ${d}</p>
        </div>
        ${stackHTML(vOut, { size: "row" })}
      </div>
      <p class="dp-lead">${lead}</p>
      ${fixHint}
      ${pax > 1 && !paxKnown ? `<p class="pax-note">${esc(SEAT_NOTE)}</p>` : ""}
      <p class="dp-lead">Search the outbound one way:</p>
      <div class="dp-cabs">${legend.map(([bit, label]) => `
        <a class="dp-cab" target="_blank" rel="noopener noreferrer"
           href="${baBookingURL(o, d, outIso, bit, null, pax)}">
          <span class="swatch ${bitClass(bit)}" aria-hidden="true"></span>
          <span class="dp-cab-label">${esc(label)}</span>
          <span class="dp-cab-go">Search one way ↗</span>
        </a>`).join("")}
      </div>
      <p class="dp-note">Award space seen in this snapshot (as of ${esc(freshLabel())}).
        Availability moves fast — verify with British Airways before planning.</p>
    </div>`));
    showPanelChrome();
    $("#pp-widen", panelEl)?.addEventListener("click", () => {
      setNightsPref(NIGHTS_DEFAULT[0], NIGHTS_DEFAULT[1]);
      const u = new URL(location.href);
      u.searchParams.set("nights", `${NIGHTS_DEFAULT[0]}-${NIGHTS_DEFAULT[1]}`);
      history.replaceState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
      route(); // re-render with the wide window; ?out= reopens this panel
    });
    $("#pp-allcabs", panelEl)?.addEventListener("click", () => {
      setFilter(cabinLegend().reduce((m, [bit]) => m | bit, 0));
      route(); // re-render with every cabin selected; ?out= reopens this panel
    });
    return;
  }

  panelEl.append(el(`<div>
    <button class="dp-close" type="button" aria-label="Close">×</button>
    <div class="pp-steps">
      <button type="button" class="pp-step" id="pp-step-out" title="Change the outbound day"><b>1</b> Out</button>
      <span class="pp-step on" id="pp-step-back"><b>2</b> Back</span>
      <span class="pp-step" id="pp-step-book"><b>3</b> Book</span>
    </div>
    <div class="dp-head">
      <div>
        <p class="dp-date">${esc(fmtDate.format(dayDate(idx)))} — pick your return</p>
        <p class="dp-route" id="dp-title">${o} <span style="color:var(--gold)" aria-hidden="true">→</span> ${d} <span style="color:var(--gold)" aria-hidden="true">→</span> ${o}</p>
        <p class="pp-sub">round trip · ${lo}–${hi} nights</p>
      </div>
    </div>
    <div class="pp-out">
      <span class="pp-out-label">Outbound</span>
      <span class="pp-out-date">${esc(fmtRet.format(dayDate(idx)))}</span>
      ${stackHTML(vOut, { size: "row" })}
    </div>
    <p class="pp-ret-label" id="pp-ret-label">Return (${d} → ${o})</p>
    ${pax > 1 && !paxKnown ? `<p class="pax-note">${esc(SEAT_NOTE)}</p>` : ""}
    <div class="pp-rows" role="radiogroup" aria-labelledby="pp-ret-label"></div>
    <div class="pp-summary${retIso ? "" : " pp-unrevealed"}">
      <p class="pp-sum-line" id="pp-sum-line"></p>
      <div class="pp-ctas" id="pp-ctas"></div>
      <p class="pp-oneways" id="pp-oneways"></p>
    </div>
    <div class="dp-flights" id="dp-flights"></div>
    <p class="dp-note">Award space seen in this snapshot (as of ${esc(freshLabel())}).
      Availability moves fast — verify with British Airways before planning.</p>
  </div>`));

  const rowsWrap = $(".pp-rows", panelEl);
  for (const r of rows) {
    // Party-size honesty on each return row: which cabins show the party
    // fitting on both legs. Under-threshold rows stay listed and bookable —
    // annotated, never hidden.
    const fitAria = pax > 1 && paxKnown
      ? (r.fit & r.bits & vOut)
        ? `, seats for ${pax} together in ${esc(cabNames(r.fit & r.bits & vOut))}`
        : `, no sign of ${pax} seats together`
      : "";
    const fitHTML = pax > 1 && paxKnown
      ? `<span class="pp-row-pax${(r.fit & r.bits & vOut) ? " fits" : ""}">${
          (r.fit & r.bits & vOut) ? `fits ${pax}` : `not for ${pax}`}</span>`
      : "";
    rowsWrap.append(el(`<button type="button" class="pp-row" role="radio" aria-checked="false"
        tabindex="-1" data-iso="${r.iso}"
        aria-label="Return ${esc(fmtRet.format(dayDate(r.idx)))}, ${r.n} night${r.n === 1 ? "" : "s"}, ${esc(cabNames(r.bits))} open${fitAria}">
      <span class="pp-radio" aria-hidden="true"></span>
      <span class="pp-row-date">${esc(fmtRet.format(dayDate(r.idx)))}</span>
      <span class="pp-row-n">${r.n} night${r.n === 1 ? "" : "s"}${fitHTML}</span>
      ${stackHTML(r.bits, { size: "row" })}
    </button>`));
  }
  const rowEls = [...rowsWrap.children];
  const summaryEl = $(".pp-summary", panelEl);
  const stepBack = $("#pp-step-back", panelEl), stepBook = $("#pp-step-book", panelEl);
  let sel = Math.max(0, rows.findIndex((r) => r.iso === retIso));

  function updateSummary() {
    const r = rows[sel];
    const shared = vOut & r.bits & mask;
    $("#pp-sum-line", panelEl).textContent =
      `${o}→${d} ${fmtRet.format(dayDate(idx))} · ${d}→${o} ${fmtRet.format(dayDate(r.idx))} · ${r.n} night${r.n === 1 ? "" : "s"}`;
    const ctas = $("#pp-ctas", panelEl);
    ctas.innerHTML = "";
    if (shared) {
      for (const [bit, label] of cabinLegend()) {
        if (!(shared & bit)) continue;
        // Annotate (never hide) party fit per cabin: the codes are thresholds
        // ("no sign of"), not proofs of absence, so the link stays offered.
        const fitNote = pax > 1 && paxKnown
          ? `<span class="pp-cta-pax${(r.fit & bit) ? " fits" : ""}">${
              (r.fit & bit) ? `fits ${pax}` : `no sign of ${pax} seats`}</span>`
          : "";
        ctas.append(el(`<a class="pp-cta" target="_blank" rel="noopener noreferrer"
            href="${baBookingURL(o, d, outIso, bit, r.iso, pax)}">
          <span class="swatch ${bitClass(bit)}" aria-hidden="true"></span>
          Search ${esc(label)} round trip${fitNote}
          <span class="pp-cta-go" aria-hidden="true">↗</span></a>`));
      }
    } else {
      ctas.append(el(`<p class="pp-none">No single cabin is open both ways on these dates — book two one-ways:</p>`));
    }
    $("#pp-oneways", panelEl).innerHTML =
      `${shared ? "or " : ""}book each leg one-way:
       <a href="${baBookingURL(o, d, outIso, 0, null, pax)}" target="_blank" rel="noopener noreferrer">${o}→${d} ${esc(fmtRet.format(dayDate(idx)))} ↗</a> ·
       <a href="${baBookingURL(d, o, r.iso, 0, null, pax)}" target="_blank" rel="noopener noreferrer">${d}→${o} ${esc(fmtRet.format(dayDate(r.idx)))} ↗</a>`;
  }

  function reveal() { // mobile step 3: selecting a return uncovers the booking summary
    summaryEl.classList.remove("pp-unrevealed");
    stepBack.classList.remove("on");
    stepBook.classList.add("on");
  }

  function selectRow(i, user) {
    sel = i;
    rowEls.forEach((rEl, j) => {
      rEl.setAttribute("aria-checked", j === i ? "true" : "false");
      rEl.tabIndex = j === i ? 0 : -1;
    });
    updateSummary();
    if (!user) return;
    reveal();
    // First pick adds a history entry (Back = undo the pick); subsequent
    // re-picks replace it, so browsing returns doesn't spam history.
    const u = new URL(location.href);
    const had = u.searchParams.has("ret");
    u.searchParams.set("out", outIso);
    u.searchParams.set("ret", rows[i].iso);
    const url = `${u.pathname}?${u.searchParams.toString()}`;
    if (had) history.replaceState(null, "", url);
    else history.pushState(null, "", url);
  }

  rowEls.forEach((rEl, i) => rEl.addEventListener("click", () => selectRow(i, true)));
  rowsWrap.addEventListener("keydown", (e) => {
    const n = rowEls.length;
    let to = -1;
    if (e.key === "ArrowDown" || e.key === "ArrowRight") to = (sel + 1) % n;
    else if (e.key === "ArrowUp" || e.key === "ArrowLeft") to = (sel - 1 + n) % n;
    else if (e.key === "Home") to = 0;
    else if (e.key === "End") to = n - 1;
    if (to < 0) return;
    e.preventDefault();
    selectRow(to, true);
    rowEls[to].focus();
  });

  selectRow(sel, false);
  if (retIso && rows[sel].iso === retIso) reveal();
  $("#pp-step-out", panelEl).addEventListener("click", closeDayPanel);
  showPanelChrome();
  loadFlightDetail(o, d, outIso);
}

/* Shared modal chrome: show panel + scrim, make the page inert, trap Tab. */
/* ---------------- via trips (one-stop multi-city) ---------------- */

/* Shared honesty line for every via view: the two facts a user must not
   discover at the BA checkout. The overnight stop is a design rule, not an
   option: flight times aren't in the data, so a same-day connection can't be
   promised — a night at the hub can. */
function viaNoteHTML(o, d, hub) {
  return `<p class="rev-note">No direct route — this journey stops overnight at
    ${esc(placeName(hub))}, as separate award bookings per leg. Dates are matched on
    award space; still verify timings when booking (a long first leg can land the
    next day).</p>`;
}

/* Month year-strip for the via calendars — same recounted-from-the-engine
   rule as every number on the trip pages. */
function viaYearStrip(months, rb, mask, legend, t0, body, animate, ariaFor) {
  const strip = el(`<div class="year-strip" role="group" aria-label="Jump to month"></div>`);
  months.forEach((mo, mi) => {
    let c = 0;
    for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, rb.length); i++) if (rb[i]) c++;
    const now = new Date();
    const isCur = mo.y === now.getFullYear() && mo.m === now.getMonth();
    const btn = el(`<button type="button" class="ys-month${isCur ? " current" : ""}" aria-label="${esc(ariaFor(mo, c))}">
      <span class="ys-label">${mo.label}</span>
      <span class="ys-bars" aria-hidden="true"></span>
      <span class="ys-count">${c || "·"}</span>
    </button>`);
    const bars = $(".ys-bars", btn);
    for (const [bit] of legend) {
      if (!(bit & mask)) continue;
      let n = 0;
      for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, rb.length); i++)
        if (rb[i] & bit) n++;
      if (!n) continue;
      const bar = document.createElement("i");
      bar.className = bitClass(bit);
      bar.style.height = `${Math.max(2, Math.round((n / Math.max(1, daysInMonth(mo))) * 30))}px`;
      if (animate) bar.style.animationDelay = `${mi * 30}ms`;
      else bar.style.animation = "none";
      bars.append(bar);
    }
    btn.addEventListener("click", () => {
      const target = $(`#month-${mo.y}-${mo.m}`, body);
      if (target) target.scrollIntoView({ behavior: "smooth", block: "start" });
    });
    strip.append(btn);
  });
  return strip;
}

/* Round-trip calendar for a pair with NO direct route, chained through a hub:
   BLL ⇄ TYO renders as BLL→LON→TYO out and TYO→LON→BLL home. Same page shape
   as renderTrip; the engine is chainBits with the cabin filter on the
   long-haul legs (focusLegs), and the honesty notes name the hub. */
function renderViaTrip(o, d, hub) {
  current.page = "trip"; current.params = { o, d, hub };
  setTitle(`${o} ⇄ ${d} via ${hub}`);

  const fullPath = [o, hub, d, hub, o];
  const outPath = [o, hub, d];
  const focus = focusLegs(fullPath);
  const focusOut = focusLegs(outPath);

  const params = new URLSearchParams(location.search);
  let nights = parseNights(params.get("nights")) || getNightsPref() || NIGHTS_DEFAULT.slice();
  let pax = activePax(params);
  let conn = activeConn(params);
  const seatsKnown = chainSeatsKnown(fullPath);
  const ep = () => (pax > 1 && seatsKnown ? pax : 1);
  const gapsFor = (c) => [[1, c], [Math.max(1, nights[0]), nights[1]], [1, c]];

  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="route-head">
    <p class="crumbs"><a href="/">Search</a> · <a href="/from/${o}">All from ${esc(placeName(o))}</a></p>
    <div class="route-title-row">
      <h1 class="route-title" aria-label="${o} to ${d} round trip via ${esc(placeName(hub))}">${o} <a class="arrow arrow-swap" href="/trip/${d}-${o}${nightsQS()}"
        title="Swap — plan ${d} ⇄ ${o} instead" aria-label="Swap origin and destination — plan ${d} to ${o} round trips">⇄</a> ${d}
        <span class="via-badge">via ${esc(placeName(hub))}</span></h1>
      ${tripSegHTML(o, d, "trip")}
    </div>
    <p class="route-cities">${esc(placeName(o))}${placeCountry(o) ? `, ${esc(placeCountry(o))}` : ""}
      <span class="via">to</span>
      ${esc(placeName(d))}${placeCountry(d) ? `, ${esc(placeCountry(d))}` : ""}</p>
    ${viaNoteHTML(o, d, hub)}
  </div>`));

  const t0 = todayIndex();
  const legend = cabinLegend();
  const allMask = legend.reduce((m, [bit]) => m | bit, 0);

  const toolbar = el(`<div class="route-toolbar"></div>`);
  const paxNote = el(`<p class="pax-note" hidden>${esc(SEAT_NOTE)}</p>`);
  const container = el(`<div></div>`);
  const body = el(`<div></div>`);
  mainEl.append(toolbar, paxNote, container, body);

  let mask = 0, firstDraw = true, chipsEl = null;

  function perCabinVia() {
    const rb = chainBits(fullPath, allMask, gapsFor(conn), ep(), focus);
    const per = new Map();
    for (let i = Math.max(0, t0); i < rb.length; i++) {
      const v = rb[i];
      if (!v) continue;
      for (const [bit] of legend) if (v & bit) per.set(bit, (per.get(bit) || 0) + 1);
    }
    return per;
  }

  function rebuildChips() {
    const forN = ep() > 1 ? ` for ${pax} travelling together` : "";
    const fresh = cabinChips(perCabinVia(), (m) => { mask = m; drawCalendars(); },
      (count, label) => count
        ? `${count} departure days completing the whole trip with ${label} on the long-haul legs${forN}`
        : `No ${label} trips${forN} within ${nights[0]}–${nights[1]} nights`);
    if (chipsEl) chipsEl.replaceWith(fresh); else toolbar.prepend(fresh);
    chipsEl = fresh;
  }
  function syncPaxNote() { paxNote.hidden = !(pax > 1 && !seatsKnown); }

  toolbar.append(nightsControlEl(() => nights, (lo, hi) => { nights = [lo, hi]; rebuildChips(); }));
  toolbar.append(connControlEl(hub, () => conn, (c) => { conn = c; rebuildChips(); }));
  if (store.hasAnySeatData) {
    toolbar.append(paxControl(pax, (n) => {
      pax = n;
      setPaxPref(n);
      mirrorPaxURL(n);
      syncPaxNote();
      rebuildChips();
    }));
  }
  rebuildChips(); // synchronously sets mask + first drawCalendars()
  syncPaxNote();
  // The same bell as direct trips, carrying the hub + stop length: the server
  // evaluates the whole chain, so "tell me when a Club trip to Tokyo opens"
  // works from Billund too.
  toolbar.append(alertBell(`${o}-${d}`, "rt", mask, {
    via: hub,
    get conn() { return conn; },
    get nights() { return nights.slice(); },
    get pickedOut() { return params.get("out") || null; },
  }));

  function drawCalendars() {
    const animate = firstDraw;
    firstDraw = false;
    container.innerHTML = ""; body.innerHTML = "";
    const n = ep();
    const gaps = gapsFor(conn);
    const rb = chainBits(fullPath, mask, gaps, n, focus);
    const rbAll = mask === allMask ? rb : chainBits(fullPath, allMask, gaps, n, focus);
    const rbAny = n > 1 ? chainBits(fullPath, allMask, gaps, 1, focus) : null;
    // The presence base for dim/empty: can the OUTBOUND journey (both legs)
    // even happen that day, any cabin, any party?
    const outAny = chainBits(outPath, allMask, [[1, conn]], 1, focusOut);
    const months = next12Months();

    container.append(viaYearStrip(months, rb, mask, legend, Math.max(0, t0), body, animate,
      (mo, c) => `${fmtMonth.format(utcDate(mo.y, mo.m, 1))}: ${c} days with complete trips${n > 1 ? ` for ${pax} travelling together` : ""}`));

    const grid = el(`<div class="months"></div>`);
    for (const mo of months) grid.append(monthCalVia(mo, rb, rbAll, rbAny, outAny, n));
    body.append(grid);
  }

  /* LIT = the whole chain completes (stack = long-haul cabins), DIM = the
     outbound journey works but the trip doesn't complete (the panel explains),
     empty = you can't even reach ${d} that day. Same three honesty axes as the
     direct trip calendar. */
  function monthCalVia(mo, rb, rbAll, rbAny, outAny, n) {
    const first = utcDate(mo.y, mo.m, 1);
    const horizon = outAny.length;
    if (mo.start >= horizon) return unreleasedMonthCard(mo, first);
    let anyOut = false;
    for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, outAny.length); i++) {
      if (outAny[i]) { anyOut = true; break; }
    }
    if (!anyOut && mo.end <= horizon) {
      return el(`<section class="month month-compact" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
        <h3>${fmtMonth.format(first)}</h3>
        <p class="month-empty">No outbound journeys this month</p>
      </section>`);
    }

    const box = el(`<section class="month" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
      <h3>${fmtMonth.format(first)} <span class="mc"></span></h3>
      <div class="dow">${["Mo","Tu","We","Th","Fr","Sa","Su"].map((dw) => `<span>${dw}</span>`).join("")}</div>
      <div class="grid"></div>
    </section>`);
    const grid = $(".grid", box);
    const lead = (first.getUTCDay() + 6) % 7;
    for (let i = 0; i < lead; i++) grid.append(el(`<span class="day pad"></span>`));

    let monthDays = 0;
    const nDays = daysInMonth(mo);
    for (let dnum = 1; dnum <= nDays; dnum++) {
      const idx = mo.start + dnum - 1;
      if (idx >= horizon) { grid.append(unreleasedDayCell(dnum, idx)); continue; }
      const vOut = (idx >= 0 && idx < outAny.length) ? outAny[idx] : 0;
      const v = (idx >= 0 && idx < rb.length) ? rb[idx] : 0;
      const isPast = idx < Math.max(0, t0);
      if (!vOut || isPast) {
        grid.append(el(`<span class="day${isPast ? " past" : ""}"><span class="num">${dnum}</span><span class="stack"></span></span>`));
        continue;
      }
      if (v) monthDays++;
      const dateLabel = esc(fmtDate.format(dayDate(idx)));
      const hiddenBits = v ? 0 : ((idx >= 0 && idx < rbAll.length) ? rbAll[idx] : 0);
      const smallerParty = !v && !hiddenBits && n > 1 && rbAny
        ? ((idx >= 0 && idx < rbAny.length) ? rbAny[idx] : 0) : 0;
      const aria = v
        ? `${dateLabel}: whole trip possible with ${esc(cabNames(v))} on the long-haul legs${n > 1 ? ` for ${pax} travelling together` : ""}`
        : hiddenBits
          ? `${dateLabel}: whole trip possible in ${esc(cabNames(hiddenBits))} — hidden by your cabin filter`
          : smallerParty
            ? `${dateLabel}: trip open in ${esc(cabNames(smallerParty))} for a smaller party — no sign of ${pax} seats together`
            : `${dateLabel}: outbound journey only — no return completes within ${nights[0]}–${nights[1]} nights`;
      const cell = el(`<button type="button" class="day has${v ? "" : " dim"}" aria-label="${aria}">
        <span class="num">${dnum}</span>
        ${stackHTML(v || vOut)}
      </button>`);
      const tipNote = v ? "long-haul cabins — hops just need any award space" : hiddenBits
        ? `whole trip open in ${cabNames(hiddenBits)} — hidden by your cabin filter`
        : smallerParty
          ? `trip open for a smaller party — no sign of ${pax} seats together`
          : "outbound journey only — no return completes in window";
      cell.addEventListener("click", () => pickOutbound(idx));
      cell.addEventListener("mouseenter", (e) => showTip(e.currentTarget, idx, v, tipNote));
      cell.addEventListener("mouseleave", hideTip);
      cell.addEventListener("focus", (e) => showTip(e.currentTarget, idx, v, tipNote));
      cell.addEventListener("blur", hideTip);
      grid.append(cell);
    }
    $(".mc", box).textContent = `${monthDays}d`;
    if (mo.end > horizon) box.append(unreleasedCaptionEl(horizon));
    return box;
  }

  function pickOutbound(idx) {
    const iso = isoOf(dayDate(idx));
    const u = new URL(location.href);
    u.searchParams.set("out", iso);
    u.searchParams.delete("ret");
    history.pushState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
    openViaPanel(o, hub, d, idx, nights, conn, mask, null, pax);
  }

  const outIdx = tripDayIndex(params.get("out") || "");
  if (outIdx >= 0) {
    const outAny = chainBits(outPath, allMask, [[1, conn]], 1, focusOut);
    if (outAny[outIdx]) {
      const retP = params.get("ret") || "";
      openViaPanel(o, hub, d, outIdx, nights, conn, mask, tripDayIndex(retP) >= 0 ? retP : null, pax);
    }
  }
}

/* The via-trip day panel: the first-leg departure is pinned; the long-haul
   dates that connect are shown per leg with their own booking links (separate
   redemptions — BA can't book a multi-city award online), and every return
   long-haul day that completes the trip is a radio row. */
function openViaPanel(o, hub, d, idx, nights, conn, mask, retIso = null, pax = 1) {
  const kOut1 = `${o}-${hub}`, kOut2 = `${hub}-${d}`, kRet1 = `${d}-${hub}`, kRet2 = `${hub}-${o}`;
  const b1 = routeBits(kOut1), b2 = routeBits(kOut2), r1 = routeBits(kRet1), r2 = routeBits(kRet2);
  const days = store.bundle.days;
  const [lo, hi] = nights;
  const minN = Math.max(1, lo);
  const outIso = isoOf(dayDate(idx));
  panelReturnFocus = document.activeElement;
  panelEl.setAttribute("role", "dialog");
  panelEl.setAttribute("aria-modal", "true");
  panelEl.setAttribute("aria-labelledby", "dp-title");
  panelEl.innerHTML = "";
  panelCloseHook = () => {
    const u = new URL(location.href);
    if (!u.searchParams.has("out") && !u.searchParams.has("ret")) return;
    u.searchParams.delete("out"); u.searchParams.delete("ret");
    const q = u.searchParams.toString();
    history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
  };

  const legLink = (from, to, dayIdx) =>
    `<a class="pp-leg-go" target="_blank" rel="noopener noreferrer"
       href="${baBookingURL(from, to, isoOf(dayDate(dayIdx)), 0, null, pax)}">Book ↗</a>`;
  const legRow = (from, to, dayIdx, bits, extra = "") => `
    <div class="pp-leg">
      <span class="pp-leg-route">${from}→${to}</span>
      <span class="pp-leg-date">${esc(fmtRet.format(dayDate(dayIdx)))}${extra}</span>
      ${stackHTML(bits, { size: "row" })}
      ${legLink(from, to, dayIdx)}
    </div>`;

  // Long-haul departures reachable from the pinned hop day — kept only when
  // the whole trip can still complete from them. Rows are PRESENCE-based
  // (the cabin filter orders them, never hides them).
  const rowsFor = (n) => {
    const rows = [];
    const rEnd = Math.min(days - 1, n + hi);
    for (let r = n + minN; r <= rEnd; r++) {
      if (!r1[r]) continue;
      const home = [];
      for (let h = r + 1; h <= Math.min(days - 1, r + conn); h++) if (r2[h]) home.push(h);
      if (!home.length) continue;
      rows.push({ r, iso: isoOf(dayDate(r)), bits: r1[r], n: r - n, home,
        shares: (r1[r] & b2[n] & mask) ? 1 : 0 });
    }
    rows.sort((a, b) => b.shares - a.shares || a.n - b.n);
    return rows;
  };
  const nOpts = [];
  for (let n = idx + 1; n <= Math.min(days - 1, idx + conn); n++) {
    if (!b2[n] || !b1[idx]) continue;
    const rows = rowsFor(n);
    if (rows.length) nOpts.push({ n, rows });
  }

  if (!nOpts.length) {
    // Honest dead end: name which junction fails and offer the fix.
    let lhAny = false;
    for (let n = idx + 1; n <= Math.min(days - 1, idx + conn); n++) if (b2[n]) lhAny = true;
    const alreadyWide = lo === NIGHTS_DEFAULT[0] && hi === NIGHTS_DEFAULT[1];
    const lead = !lhAny
      ? `No ${hub}→${d} award space within your ${conn}-night stop after this departure.`
      : `The outbound journey works, but no return completes within ${lo}–${hi} nights.`;
    const fixHint = !lhAny && conn < 3
      ? `<p><button type="button" class="btn" id="vp-conn2">Allow up to 3 nights at ${esc(placeName(hub))}</button></p>`
      : lhAny && !alreadyWide
        ? `<p><button type="button" class="btn" id="vp-widen">Widen trip length to ${NIGHTS_DEFAULT[0]}–${NIGHTS_DEFAULT[1]} nights</button></p>`
        : "";
    panelEl.append(el(`<div>
      <button class="dp-close" type="button" aria-label="Close">×</button>
      <div class="dp-head">
        <div>
          <p class="dp-date">${esc(fmtDate.format(dayDate(idx)))}</p>
          <p class="dp-route" id="dp-title">${o} <span style="color:var(--gold)" aria-hidden="true">→</span> ${hub} <span style="color:var(--gold)" aria-hidden="true">→</span> ${d}</p>
        </div>
        ${stackHTML(b1[idx], { size: "row" })}
      </div>
      <p class="dp-lead">${lead}</p>
      ${fixHint}
      <p class="dp-lead">Book what's open one way:</p>
      <div class="pp-legs">${legRow(o, hub, idx, b1[idx])}</div>
      <p class="dp-note">Award space seen in this snapshot (as of ${esc(freshLabel())}).
        Availability moves fast — verify with British Airways before planning.</p>
    </div>`));
    showPanelChrome();
    $("#vp-widen", panelEl)?.addEventListener("click", () => {
      setNightsPref(NIGHTS_DEFAULT[0], NIGHTS_DEFAULT[1]);
      const u = new URL(location.href);
      u.searchParams.set("nights", `${NIGHTS_DEFAULT[0]}-${NIGHTS_DEFAULT[1]}`);
      history.replaceState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
      route();
    });
    $("#vp-conn2", panelEl)?.addEventListener("click", () => {
      setConnPref(3);
      const u = new URL(location.href);
      u.searchParams.set("conn", "3");
      history.replaceState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
      route();
    });
    return;
  }

  panelEl.append(el(`<div>
    <button class="dp-close" type="button" aria-label="Close">×</button>
    <div class="pp-steps">
      <button type="button" class="pp-step" id="pp-step-out" title="Change the outbound day"><b>1</b> Out</button>
      <span class="pp-step on" id="pp-step-back"><b>2</b> Back</span>
      <span class="pp-step" id="pp-step-book"><b>3</b> Book</span>
    </div>
    <div class="dp-head">
      <div>
        <p class="dp-date">${esc(fmtDate.format(dayDate(idx)))} — pick your return</p>
        <p class="dp-route" id="dp-title">${o} <span style="color:var(--gold)" aria-hidden="true">→</span> ${d} <span style="color:var(--gold)" aria-hidden="true">→</span> ${o}</p>
        <p class="pp-sub">via ${esc(placeName(hub))} · ${lo}–${hi} nights · separate bookings per leg</p>
      </div>
    </div>
    <p class="pp-ret-label">Outbound</p>
    <div class="pp-legs" id="vp-out-legs"></div>
    <div id="vp-lh-choice"></div>
    <p class="pp-ret-label" id="pp-ret-label">Return (${d} → ${o})</p>
    ${pax > 1 && !chainSeatsKnown([o, hub, d, hub, o]) ? `<p class="pax-note">${esc(SEAT_NOTE)}</p>` : ""}
    <div class="pp-rows" role="radiogroup" aria-labelledby="pp-ret-label"></div>
    <div class="pp-summary${retIso ? "" : " pp-unrevealed"}">
      <p class="pp-sum-line" id="pp-sum-line"></p>
      <p class="pp-ret-label">Book the return legs</p>
      <div class="pp-legs" id="vp-ret-legs"></div>
      <p class="pp-oneways" id="vp-cabin-line"></p>
    </div>
    <p class="dp-note">Four separate award bookings, matched on award space (not flight
      times — verify connections). Seen in data as of ${esc(freshLabel())}.</p>
  </div>`));

  let ni = 0, sel = 0;
  const rowsWrap = $(".pp-rows", panelEl);
  const summaryEl = $(".pp-summary", panelEl);
  const stepBack = $("#pp-step-back", panelEl), stepBook = $("#pp-step-book", panelEl);

  function drawOutLegs() {
    const { n } = nOpts[ni];
    $("#vp-out-legs", panelEl).innerHTML =
      legRow(o, hub, idx, b1[idx]) +
      legRow(hub, d, n, b2[n], ` <small>(${n - idx} night${n - idx > 1 ? "s" : ""} in ${esc(placeName(hub))})</small>`);
    const choice = $("#vp-lh-choice", panelEl);
    choice.innerHTML = "";
    if (nOpts.length > 1) {
      const seg = el(`<div class="nights-ctl vp-lh" role="group" aria-label="${hub} to ${d} departure day">
        <span class="nc-label">${hub}→${d} on</span>
        ${nOpts.map(({ n: nn }, i) =>
          `<button type="button" class="np" data-i="${i}" aria-pressed="${i === ni}">${esc(fmtRet.format(dayDate(nn)))}</button>`).join("")}
      </div>`);
      seg.querySelectorAll(".np").forEach((btn) => btn.addEventListener("click", () => {
        ni = Number(btn.dataset.i);
        sel = 0;
        drawOutLegs(); drawRows(false);
      }));
      choice.append(seg);
    }
  }

  function updateSummary() {
    const { n, rows } = nOpts[ni];
    const r = rows[sel];
    $("#pp-sum-line", panelEl).textContent =
      `${o}→${d} ${fmtRet.format(dayDate(idx))} · ${d}→${o} ${fmtRet.format(dayDate(r.r))} · ${r.n} night${r.n === 1 ? "" : "s"} in ${placeName(d)}`;
    $("#vp-ret-legs", panelEl).innerHTML =
      legRow(d, hub, r.r, r.bits) +
      legRow(hub, o, r.home[0], r2[r.home[0]]) +
      (r.home.length > 1
        ? `<p class="pp-oneways">or ${hub}→${o} on ${r.home.slice(1).map((h) =>
            `<a target="_blank" rel="noopener noreferrer" href="${baBookingURL(hub, o, isoOf(dayDate(h)), 0, null, pax)}">${esc(fmtRet.format(dayDate(h)))} ↗</a>`).join(" · ")}</p>`
        : "");
    const shared = b2[n] & r.bits & mask;
    const sharedAll = b2[n] & r.bits;
    $("#vp-cabin-line", panelEl).textContent = shared
      ? `${cabNames(shared)} open on both long-haul legs.`
      : sharedAll
        ? `No filtered cabin is open on both long-haul legs — out: ${cabNames(b2[n])}, back: ${cabNames(r.bits)}.`
        : `No single cabin both ways — out: ${cabNames(b2[n])}, back: ${cabNames(r.bits)}.`;
  }

  function reveal() {
    summaryEl.classList.remove("pp-unrevealed");
    stepBack.classList.remove("on");
    stepBook.classList.add("on");
  }

  function drawRows(restore) {
    const { rows } = nOpts[ni];
    rowsWrap.innerHTML = "";
    for (const r of rows) {
      rowsWrap.append(el(`<button type="button" class="pp-row" role="radio" aria-checked="false"
          tabindex="-1" data-iso="${r.iso}"
          aria-label="Return ${esc(fmtRet.format(dayDate(r.r)))}, ${r.n} night${r.n === 1 ? "" : "s"}, ${esc(cabNames(r.bits))} open">
        <span class="pp-radio" aria-hidden="true"></span>
        <span class="pp-row-date">${esc(fmtRet.format(dayDate(r.r)))}</span>
        <span class="pp-row-n">${r.n} night${r.n === 1 ? "" : "s"}</span>
        ${stackHTML(r.bits, { size: "row" })}
      </button>`));
    }
    const rowEls = [...rowsWrap.children];
    if (restore && retIso) {
      const i = rows.findIndex((r) => r.iso === retIso);
      if (i >= 0) { sel = i; reveal(); }
    }
    function selectRow(i, user) {
      sel = i;
      rowEls.forEach((rEl, j) => {
        rEl.setAttribute("aria-checked", j === i ? "true" : "false");
        rEl.tabIndex = j === i ? 0 : -1;
      });
      updateSummary();
      if (!user) return;
      reveal();
      const u = new URL(location.href);
      const had = u.searchParams.has("ret");
      u.searchParams.set("out", outIso);
      u.searchParams.set("ret", rows[i].iso);
      const url = `${u.pathname}?${u.searchParams.toString()}`;
      if (had) history.replaceState(null, "", url);
      else history.pushState(null, "", url);
    }
    rowEls.forEach((rEl, i) => rEl.addEventListener("click", () => selectRow(i, true)));
    rowsWrap.onkeydown = (e) => {
      const total = rowEls.length;
      let to = -1;
      if (e.key === "ArrowDown" || e.key === "ArrowRight") to = (sel + 1) % total;
      else if (e.key === "ArrowUp" || e.key === "ArrowLeft") to = (sel - 1 + total) % total;
      else if (e.key === "Home") to = 0;
      else if (e.key === "End") to = total - 1;
      if (to < 0) return;
      e.preventDefault();
      selectRow(to, true);
      rowEls[to].focus();
    };
    selectRow(sel, false);
  }

  drawOutLegs();
  drawRows(true);
  $("#pp-step-out", panelEl).addEventListener("click", closeDayPanel);
  showPanelChrome();
}

/* One-way calendar for a pair with no direct route: the two-leg chain
   (o→hub→d) with the cabin filter on the long-haul leg. */
function renderViaRoute(o, d, hub) {
  current.page = "route"; current.params = { o, d, hub };
  setTitle(`${o} → ${d} via ${hub}`);

  const path = [o, hub, d];
  const focus = focusLegs(path);
  const params = new URLSearchParams(location.search);
  let pax = activePax(params);
  let conn = activeConn(params);
  const seatsKnown = chainSeatsKnown(path);
  const ep = () => (pax > 1 && seatsKnown ? pax : 1);

  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="route-head">
    <p class="crumbs"><a href="/">Search</a> · <a href="/from/${o}">All from ${esc(placeName(o))}</a></p>
    <div class="route-title-row">
      <h1 class="route-title" aria-label="${o} to ${d} via ${esc(placeName(hub))}">${o} <a class="arrow arrow-swap" href="/route/${d}-${o}"
        title="Flip direction — view ${d} → ${o}" aria-label="Flip direction — view ${d} to ${o}">→</a> ${d}
        <span class="via-badge">via ${esc(placeName(hub))}</span></h1>
      ${tripSegHTML(o, d, "route")}
    </div>
    <p class="route-cities">${esc(placeName(o))}${placeCountry(o) ? `, ${esc(placeCountry(o))}` : ""}
      <span class="via">to</span>
      ${esc(placeName(d))}${placeCountry(d) ? `, ${esc(placeCountry(d))}` : ""}</p>
    ${viaNoteHTML(o, d, hub)}
  </div>`));

  const t0 = Math.max(0, todayIndex());
  const legend = cabinLegend();
  const allMask = legend.reduce((m, [bit]) => m | bit, 0);

  const toolbar = el(`<div class="route-toolbar"></div>`);
  const paxNote = el(`<p class="pax-note" hidden>${esc(SEAT_NOTE)}</p>`);
  const container = el(`<div></div>`);
  const body = el(`<div></div>`);
  mainEl.append(toolbar, paxNote, container, body);

  let mask = 0, firstDraw = true, chipsEl = null;
  const gapsFor = (c) => [[1, c]];

  function rebuildChips() {
    const rb = chainBits(path, allMask, gapsFor(conn), ep(), focus);
    const per = new Map();
    for (let i = t0; i < rb.length; i++) {
      const v = rb[i];
      if (!v) continue;
      for (const [bit] of legend) if (v & bit) per.set(bit, (per.get(bit) || 0) + 1);
    }
    const forN = ep() > 1 ? ` for ${pax} travelling together` : "";
    const fresh = cabinChips(per, (m) => { mask = m; draw(); },
      (count, label) => count
        ? `${count} days with the whole journey in ${label} on the long-haul leg${forN}`
        : `No ${label} journeys${forN} right now`);
    if (chipsEl) chipsEl.replaceWith(fresh); else toolbar.prepend(fresh);
    chipsEl = fresh;
  }
  function syncPaxNote() { paxNote.hidden = !(pax > 1 && !seatsKnown); }

  toolbar.append(connControlEl(hub, () => conn, (c) => { conn = c; rebuildChips(); }));
  if (store.hasAnySeatData) {
    toolbar.append(paxControl(pax, (n) => {
      pax = n;
      setPaxPref(n);
      mirrorPaxURL(n);
      syncPaxNote();
      rebuildChips();
    }));
  }
  rebuildChips();
  syncPaxNote();
  toolbar.append(alertBell(`${o}-${d}`, "ow", mask, {
    via: hub,
    get conn() { return conn; },
  }));

  function draw() {
    const animate = firstDraw;
    firstDraw = false;
    container.innerHTML = ""; body.innerHTML = "";
    const n = ep();
    const rb = chainBits(path, mask, gapsFor(conn), n, focus);
    const rbAll = mask === allMask ? rb : chainBits(path, allMask, gapsFor(conn), n, focus);
    const rbAny = n > 1 ? chainBits(path, allMask, gapsFor(conn), 1, focus) : null;
    const reachable = n > 1 && rbAny ? rbAny : rbAll; // presence base: any cabin (any party)
    const months = next12Months();

    container.append(viaYearStrip(months, rb, mask, legend, t0, body, animate,
      (mo, c) => `${fmtMonth.format(utcDate(mo.y, mo.m, 1))}: ${c} days with the whole journey${n > 1 ? ` for ${pax} travelling together` : ""}`));

    const grid = el(`<div class="months"></div>`);
    for (const mo of months) {
      const first = utcDate(mo.y, mo.m, 1);
      const horizon = rb.length;
      if (mo.start >= horizon) { grid.append(unreleasedMonthCard(mo, first)); continue; }
      let any = false;
      for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, reachable.length); i++)
        if (reachable[i]) { any = true; break; }
      if (!any && mo.end <= horizon) {
        grid.append(el(`<section class="month month-compact" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
          <h3>${fmtMonth.format(first)}</h3>
          <p class="month-empty">No journeys this month</p>
        </section>`));
        continue;
      }
      const box = el(`<section class="month" id="month-${mo.y}-${mo.m}" aria-label="${fmtMonth.format(first)}">
        <h3>${fmtMonth.format(first)} <span class="mc"></span></h3>
        <div class="dow">${["Mo","Tu","We","Th","Fr","Sa","Su"].map((dw) => `<span>${dw}</span>`).join("")}</div>
        <div class="grid"></div>
      </section>`);
      const g = $(".grid", box);
      const leadPad = (first.getUTCDay() + 6) % 7;
      for (let i = 0; i < leadPad; i++) g.append(el(`<span class="day pad"></span>`));
      let monthDays = 0;
      const nDays = daysInMonth(mo);
      for (let dnum = 1; dnum <= nDays; dnum++) {
        const idx = mo.start + dnum - 1;
        if (idx >= horizon) { g.append(unreleasedDayCell(dnum, idx)); continue; }
        const can = (idx >= 0 && idx < reachable.length) ? reachable[idx] : 0;
        const v = (idx >= 0 && idx < rb.length) ? rb[idx] : 0;
        const isPast = idx < t0;
        if (!can || isPast) {
          g.append(el(`<span class="day${isPast ? " past" : ""}"><span class="num">${dnum}</span><span class="stack"></span></span>`));
          continue;
        }
        if (v) monthDays++;
        const dateLabel = esc(fmtDate.format(dayDate(idx)));
        const hiddenBits = v ? 0 : rbAll[idx];
        const smallerParty = !v && !hiddenBits && n > 1 && rbAny ? rbAny[idx] : 0;
        const aria = v
          ? `${dateLabel}: journey possible with ${esc(cabNames(v))} on the long-haul leg${n > 1 ? ` for ${pax} travelling together` : ""}`
          : hiddenBits
            ? `${dateLabel}: journey possible in ${esc(cabNames(hiddenBits))} — hidden by your cabin filter`
            : `${dateLabel}: journey open in ${esc(cabNames(smallerParty))} for a smaller party — no sign of ${pax} seats together`;
        const cell = el(`<button type="button" class="day has${v ? "" : " dim"}" aria-label="${aria}">
          <span class="num">${dnum}</span>
          ${stackHTML(v || can)}
        </button>`);
        const tipNote = v ? "long-haul cabins — the hop just needs any award space" : hiddenBits
          ? `journey open in ${cabNames(hiddenBits)} — hidden by your cabin filter`
          : `open for a smaller party — no sign of ${pax} seats together`;
        cell.addEventListener("click", () => openViaOWPanel(o, hub, d, idx, conn, pax));
        cell.addEventListener("mouseenter", (e) => showTip(e.currentTarget, idx, v, tipNote));
        cell.addEventListener("mouseleave", hideTip);
        cell.addEventListener("focus", (e) => showTip(e.currentTarget, idx, v, tipNote));
        cell.addEventListener("blur", hideTip);
        g.append(cell);
      }
      $(".mc", box).textContent = `${monthDays}d`;
      if (mo.end > horizon) box.append(unreleasedCaptionEl(horizon));
      grid.append(box);
    }
    body.append(grid);
  }
}

/* One-way via day panel: both legs with their feasible dates, each its own
   booking link. */
function openViaOWPanel(o, hub, d, idx, conn, pax = 1) {
  const b1 = routeBits(`${o}-${hub}`), b2 = routeBits(`${hub}-${d}`);
  const days = store.bundle.days;
  panelReturnFocus = document.activeElement;
  panelCloseHook = null;
  panelEl.setAttribute("role", "dialog");
  panelEl.setAttribute("aria-modal", "true");
  panelEl.setAttribute("aria-labelledby", "dp-title");
  panelEl.innerHTML = "";

  const lhRows = [];
  for (let n = idx + 1; n <= Math.min(days - 1, idx + conn); n++)
    if (b2[n]) lhRows.push(n);

  const legRow = (from, to, dayIdx, bits, extra = "") => `
    <div class="pp-leg">
      <span class="pp-leg-route">${from}→${to}</span>
      <span class="pp-leg-date">${esc(fmtRet.format(dayDate(dayIdx)))}${extra}</span>
      ${stackHTML(bits, { size: "row" })}
      <a class="pp-leg-go" target="_blank" rel="noopener noreferrer"
         href="${baBookingURL(from, to, isoOf(dayDate(dayIdx)), 0, null, pax)}">Book ↗</a>
    </div>`;

  panelEl.append(el(`<div>
    <button class="dp-close" type="button" aria-label="Close">×</button>
    <div class="dp-head">
      <div>
        <p class="dp-date">${esc(fmtDate.format(dayDate(idx)))}</p>
        <p class="dp-route" id="dp-title">${o} <span style="color:var(--gold)" aria-hidden="true">→</span> ${hub} <span style="color:var(--gold)" aria-hidden="true">→</span> ${d}</p>
      </div>
      ${stackHTML(b1[idx], { size: "row" })}
    </div>
    <p class="dp-lead">Two separate award bookings, matched on award space — verify
      connection timings before booking:</p>
    <div class="pp-legs">
      ${legRow(o, hub, idx, b1[idx])}
      ${lhRows.map((n) => legRow(hub, d, n, b2[n], ` <small>(${n - idx} night${n - idx > 1 ? "s" : ""} in ${esc(placeName(hub))})</small>`)).join("")}
    </div>
    <p class="dp-note">Award space seen in this snapshot (as of ${esc(freshLabel())}).
      Availability moves fast — verify with British Airways before planning.</p>
  </div>`));
  showPanelChrome();
}

function showPanelChrome() {
  panelEl.hidden = false; scrimEl.hidden = false;
  document.body.classList.add("modal-open");
  setInert(true);
  $(".dp-close", panelEl).addEventListener("click", closeDayPanel);
  panelEl.addEventListener("keydown", trapTab);
  $(".dp-close", panelEl).focus();
}

/* Flight-level detail into #dp-flights, only where the data says it exists
   (no 404 probing — raw.githubusercontent caches 404s for five minutes). */
async function loadFlightDetail(o, d, iso) {
  const route = store.bundle.routes[`${o}-${d}`];
  const month = iso.slice(0, 7);
  if (!route?.fm?.includes(month)) return;
  const holder = $("#dp-flights", panelEl);
  if (!holder) return;
  holder.innerHTML = `<h4>Flights</h4><div class="sk-line" style="height:52px"></div>`;
  try {
    const data = await getJSON(`${dataBase}/flights/${o}/${d}/${month}.json`);
    const dayKey = iso.slice(8, 10);
    const perAirline = data.days?.[dayKey] || {};
    holder.innerHTML = "<h4>Flights</h4>";
    const flights = Object.entries(perAirline).flatMap(([al, arr]) => arr.map((f) => ({ al, ...f })));
    if (!flights.length) {
      holder.append(el(`<p class="dp-note">Flight-by-flight detail isn't in yet for this date.</p>`));
    }
    for (const f of flights) {
      holder.append(el(`<div class="flight">
        <div class="fl-head"><span>${esc(f.fn.join(" + "))}</span>
          <span>${esc((store.bundle.airlines[f.al]?.name) || f.al)}</span></div>
        <div class="fl-times">${esc(f.dep)} → ${esc(f.arr)}
          ${f.via?.length ? `<span class="via">via ${esc(f.via.join(", "))}</span>` : ""}</div>
        <div class="fl-seats">${Object.entries(f.seats || {})
          .filter(([, n]) => n > 0)
          .map(([code, n]) => {
            const bit = { M: 1, W: 2, C: 4, F: 8 }[code] || 0;
            return `<span class="s"><span class="swatch ${bitClass(bit)}"></span>${n} × ${esc(code)}</span>`;
          }).join("")}</div>
        <div class="fl-tags">
          ${f.peak ? `<span class="tag${f.peak === "peak" ? " peak" : ""}">${esc(f.peak)}</span>` : ""}
          ${f.rfs ? `<span class="tag">Reward Flight Saver</span>` : ""}
        </div>
      </div>`));
    }
  } catch {
    holder.innerHTML = `<h4>Flights</h4><p class="dp-note">Flight detail is pending — it'll load next time.</p>`;
  }
}

function closeDayPanel(userEv) {
  if (panelEl.hidden) return;
  panelEl.hidden = true; scrimEl.hidden = true;
  panelEl.removeEventListener("keydown", trapTab);
  document.body.classList.remove("modal-open");
  setInert(false);
  const hook = panelCloseHook;
  panelCloseHook = null;
  if (userEv) hook?.();
  panelReturnFocus?.focus?.();
}

/* Make the page behind the modal inert (unfocusable + hidden from AT). */
function setInert(on) {
  for (const sel of [".skip-link", ".topbar", "#banner", "#main", ".footer"]) {
    const node = $(sel);
    if (node) node.inert = on;
  }
}

/* Cycle Tab within the open panel so focus can't wander onto the scrim-covered
   page behind it. Candidates are filtered to ACTUALLY focusable nodes: a
   display:none region (e.g. the pair panel's unrevealed summary on mobile)
   has no client rects, and a hidden `last` would be an unfocusable trap
   endpoint — Tab past it would escape the dialog. */
function trapTab(e) {
  if (e.key !== "Tab") return;
  const focusable = [...panelEl.querySelectorAll(
    'a[href], button:not([disabled]):not([tabindex="-1"]), input, [tabindex]:not([tabindex="-1"])')]
    .filter((n) => n.getClientRects().length);
  if (!focusable.length) return;
  const first = focusable[0], last = focusable[focusable.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
}

scrimEl.addEventListener("click", closeDayPanel);
document.addEventListener("keydown", (e) => { if (e.key === "Escape") { closeDayPanel(e); hideTip(); } });

/* ---------------- pages: explore ---------------- */

function renderFrom(o) {
  current.page = "from"; current.params = { o };
  setTitle(`From ${placeName(o)}`);
  if (!store.bundle) return;

  mainEl.innerHTML = "";
  const dests = (store.destsByOrigin.get(o) || []).slice();
  const allVias = viaDestsFrom(o);
  // Maximum stops: 0 = nonstop only, 1 (default) = everything. URL-borne and
  // carried across the List/Map toggle, mirroring the map's control.
  const stops = new URLSearchParams(location.search).get("stops") === "0" ? 0 : 1;
  const vias = stops ? allVias : new Map();
  // The list respects an active party size (URL → pref → 1) so its counts
  // agree with the map and the calendars behind the cards. Routes without
  // seat data keep any-party counts, with the honest note below.
  const pax = activePax();
  const conn = getConnPref() ?? 1; // the calendar behind a via card reads the same pref
  const mapQS = [pax > 1 ? `pax=${pax}` : "", stops === 0 ? "stops=0" : ""].filter(Boolean).join("&");
  mainEl.append(el(`<div class="section-pad">
    <p class="crumbs"><a href="/">Search</a></p>
    <h1 class="page-title">Everywhere from ${esc(placeName(o))}</h1>
    <div class="view-toggle" role="group" aria-label="View">
      <span class="vt-on" aria-current="page">List</span><a href="/map/${o}${mapQS ? `?${mapQS}` : ""}">Map</a>
    </div>
    <p class="page-sub">${dests.length + vias.size} destination${dests.length + vias.size === 1 ? "" : "s"} with award seats in the next year${
      vias.size ? ` — ${dests.length} nonstop, ${vias.size} with one overnight stop` : ""}${
      !stops && allVias.size ? ` (nonstop only — ${allVias.size} more with one stop)` : ""}.
      Bars show days with round trips per month (any cabin, ${NIGHTS_DEFAULT[0]}–${NIGHTS_DEFAULT[1]} nights${pax > 1 ? `, ${pax} travelling together` : ""}).</p>
    <div class="nights-ctl stops-ctl" role="group" aria-label="Maximum stops">
      <span class="nc-label">Stops</span>
      <button type="button" class="np" data-stops="0" aria-pressed="${stops === 0}">Nonstop</button>
      <button type="button" class="np" data-stops="1" aria-pressed="${stops === 1}">≤1 stop</button>
    </div>
    ${pax > 1 ? `<p class="pax-note">Routes without seat counts in the data yet are shown for any party size.</p>` : ""}
  </div>`));

  mainEl.querySelectorAll(".stops-ctl .np").forEach((b) => b.addEventListener("click", () => {
    const u = new URL(location.href);
    if (Number(b.dataset.stops) === 1) u.searchParams.delete("stops");
    else u.searchParams.set("stops", "0");
    const q = u.searchParams.toString();
    history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
    route(); // rebuild the list under the new scope
  }));

  if (!dests.length && !allVias.size) {
    mainEl.append(el(`<div class="empty-state">
      <div class="big">No destinations right now.</div>
      <p>No award seats from ${esc(placeName(o))} in the current data.</p></div>`));
    return;
  }

  if (!dests.length && !vias.size) {
    // Everything reachable needs a stop, and the filter hides those.
    mainEl.append(el(`<div class="empty-state">
      <div class="big">No nonstop destinations.</div>
      <p>${allVias.size} destination${allVias.size > 1 ? "s are" : " is"} reachable with one overnight stop.</p>
      <p><a class="btn" href="/from/${o}">Show 1-stop journeys</a></p>
    </div>`));
    return;
  }

  // Counts are ROUND-TRIPPABLE days over the default window (any cabin): a day
  // counts only when its outbound has a valid same-cabin return — for a via
  // destination, when the whole chain completes (cabins coupled on the long
  // legs, hops any space, overnight stops at the hub). Destinations with
  // outbound space but zero such days keep their card, grayed and badged.
  const t0 = Math.max(0, todayIndex());
  const allMask = cabinLegend().reduce((m, [bit]) => m | bit, 0);
  const months = next12Months();
  const tally = (rt) => {
    let total = 0, union = 0;
    for (let i = t0; i < rt.length; i++) if (rt[i]) { total++; union |= rt[i]; }
    const counts = months.map((mo) => {
      let n = 0;
      for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, rt.length); i++) if (rt[i]) n++;
      return n;
    });
    return { total, union, counts };
  };
  const cards = dests.map((d) => {
    const key = `${o}-${d}`;
    // Per-route honesty: a pair without seat data on both legs counts at
    // any-party (never silently vanishing at pax >= 2) and gets a badge.
    const known = pax <= 1 || pairSeatsKnown(key, `${d}-${o}`);
    const cardPax = known ? pax : 1;
    const rt = roundTripBits(key, `${d}-${o}`, allMask, NIGHTS_DEFAULT[0], NIGHTS_DEFAULT[1], cardPax);
    const unverified = pax > 1 && !known;
    // Outbound totals follow the SAME coverage rule as the round-trip
    // numbers: a card badged "shown for any party" must not mix in
    // pax-thresholded outbound swatches or sort rank.
    return { d, key, via: null, ...tally(rt), unverified, out: routeTotals(key, cardPax) };
  }).concat([...vias].map(([d, { hub, both }]) => {
    const fullPath = [o, hub, d, hub, o];
    const outPath = [o, hub, d];
    // Hop legs carry no seat data, so via counts are any-party (badged below).
    const rt = both
      ? chainBits(fullPath, allMask, [[1, conn], [NIGHTS_DEFAULT[0], NIGHTS_DEFAULT[1]], [1, conn]], 1, focusLegs(fullPath))
      : new Uint8Array(store.bundle.days);
    const ob = chainBits(outPath, allMask, [[1, conn]], 1, focusLegs(outPath));
    let obTotal = 0, obUnion = 0;
    for (let i = t0; i < ob.length; i++) if (ob[i]) { obTotal++; obUnion |= ob[i]; }
    return { d, key: `${o}-${d}`, via: hub, ...tally(rt), unverified: pax > 1,
      out: { total: obTotal, union: obUnion } };
  })).sort((a, b) => b.total - a.total || b.out.total - a.out.total);

  const grid = el(`<div class="dest-grid"></div>`);
  const max = Math.max(1, ...cards.flatMap((c) => c.counts));
  for (const c of cards) {
    // Only a VIA candidate with nothing at all disappears; a direct
    // destination keeps its card (grayed/badged) per the contract above.
    if (c.via && c.total === 0 && c.out.total === 0) continue;
    const ow = c.total === 0; // outbound space only — nothing round-trippable
    const cabs = cabinLegend().filter(([bit]) => (ow ? c.out.union : c.union) & bit)
      .map(([bit, label]) => `<span class="swatch ${bitClass(bit)}" title="${esc(label)}"></span>`).join("");
    grid.append(el(`<a class="dest-card${ow ? " dc-ow" : ""}" href="/trip/${c.key}${pax > 1 ? `?pax=${pax}` : ""}">
      <span class="dc-head"><span class="dc-code">${c.d}</span>
        <span class="dc-name">${esc(placeName(c.d))}</span>
        <span class="dc-country">${esc(placeCountry(c.d))}</span>
        ${c.via ? `<span class="dc-badge dc-via">via ${c.via}</span>` : ""}</span>
      <span class="dc-spark" aria-hidden="true">${c.counts.map((n) =>
        `<i style="height:${Math.max(2, Math.round((n / max) * 30))}px"></i>`).join("")}</span>
      <span class="dc-meta">${ow
        ? `<span class="dc-badge">outbound only — no return space seen${pax > 1 && !c.unverified ? ` for ${pax}` : ""}</span>`
        : `<span>${c.total} days with round trips${pax > 1 && !c.unverified ? ` for ${pax}` : ""}</span>`}
        ${c.unverified ? `<span class="dc-badge">seat counts pending — shown for any party</span>` : ""}
        <span class="dc-cabs">${cabs}</span></span>
    </a>`));
  }
  mainEl.append(grid);
}

/* ---------------- pages: destination map ---------------- */

/* The world base (land outline + projection constants) is a 57KB static asset
   used only here, so it loads on first map visit, not at boot. */
let _worldLoad = null;
function loadWorld() {
  if (window.WORLD1) return Promise.resolve();
  if (!_worldLoad) {
    _worldLoad = new Promise((resolve, reject) => {
      const s = document.createElement("script");
      s.src = "/assets/world-1.js?v=1";
      s.onload = () => resolve();
      s.onerror = () => { _worldLoad = null; reject(new Error("map asset unreachable")); };
      document.head.append(s);
    });
  }
  return _worldLoad;
}

/* Natural Earth I forward projection (d3-geo's naturalEarth1Raw polynomial).
   Must match mkworld.mjs exactly — the land path was pre-projected with it. */
function ne1Raw(l, p) {
  const p2 = p * p, p4 = p2 * p2;
  return [
    l * (0.8707 - 0.131979 * p2 + p4 * (-0.013791 + p4 * (0.003971 * p2 - 0.001529 * p4))),
    p * (1.007226 + p2 * (0.015085 + p4 * (-0.044475 + 0.028874 * p2 - 0.005916 * p4))),
  ];
}
/* Great-circle arc between two [lat, lon], sampled and projected, split into
   runs where the longitude wraps so an antimeridian crossing never draws a
   line across the whole map. */
function gcRuns(g1, g2, steps = 40) {
  const rad = Math.PI / 180;
  const vec = ([lat, lon]) =>
    [Math.cos(lat * rad) * Math.cos(lon * rad), Math.cos(lat * rad) * Math.sin(lon * rad), Math.sin(lat * rad)];
  const a = vec(g1), b = vec(g2);
  const w = Math.acos(Math.max(-1, Math.min(1, a[0] * b[0] + a[1] * b[1] + a[2] * b[2])));
  if (w < 1e-9) return [];
  const runs = [];
  let run = [], prevLon = null;
  for (let i = 0; i <= steps; i++) {
    const t = i / steps;
    const s1 = Math.sin((1 - t) * w) / Math.sin(w), s2 = Math.sin(t * w) / Math.sin(w);
    const x = s1 * a[0] + s2 * b[0], y = s1 * a[1] + s2 * b[1], z = s1 * a[2] + s2 * b[2];
    const lat = Math.asin(Math.max(-1, Math.min(1, z))) / rad;
    const lon = Math.atan2(y, x) / rad;
    if (prevLon !== null && Math.abs(lon - prevLon) > 180) {
      if (run.length > 1) runs.push(run);
      run = [];
    }
    prevLon = lon;
    run.push(projectLL(lat, lon));
  }
  if (run.length > 1) runs.push(run);
  return runs;
}

function projectLL(lat, lon) {
  const W = window.WORLD1, r = Math.PI / 180;
  const [x, y] = ne1Raw(lon * r, lat * r);
  return [W.tx + W.k * x, W.ty - W.k * y];
}

function renderMap(o) {
  current.page = "map"; current.params = { o };
  setTitle(`Map from ${placeName(o)}`);
  if (!store.bundle) return;

  const dests = (store.destsByOrigin.get(o) || []).slice();
  const vias = viaDestsFrom(o);
  // Direct dots first, then via dots: later SVG siblings paint on top, and a
  // nonstop option must never be buried under its one-stop neighbours.
  const spots = dests.map((d) => ({ d, via: null, both: true }))
    .concat([...vias].map(([d, { hub, both }]) => ({ d, via: hub, both })));
  // Maximum stops: 0 = nonstop only, 1 (default) = the whole reachable world.
  // URL-borne (no session pref — it's a per-exploration scope, like month)
  // and carried across the List/Map toggle so switching views keeps it.
  let stops = new URLSearchParams(location.search).get("stops") === "0" ? 0 : 1;
  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="section-pad">
    <p class="crumbs"><a href="/">Search</a></p>
    <h1 class="page-title">Where can you go from ${esc(placeName(o))}?</h1>
    <div class="view-toggle" role="group" aria-label="View">
      <a href="/from/${o}${stops === 0 ? "?stops=0" : ""}">List</a><span class="vt-on" aria-current="page">Map</span>
    </div>
    <p class="page-sub">Every dot is a destination with bookable round trips —
      tap one for its calendar.</p>
  </div>`));

  if (!spots.length) {
    mainEl.append(el(`<div class="empty-state">
      <div class="big">No destinations right now.</div>
      <p>No award seats from ${esc(placeName(o))} in the current data.</p></div>`));
    return;
  }

  /* Controls: cabin chips (always visible — a map that hides cabins is the
     top competitor complaint), a month narrower (the "school holidays" ask;
     default stays the whole year, not a 2-week window), and the same
     trip-length window as the route pages. Cabins and nights are both seeded
     URL → shared session pref → default, and both write back to each, so the
     filter survives reload/share and rides along into the calendar pages
     (which read the same rf:filter pref). */
  const allMask = cabinLegend().reduce((m, [bit]) => m | bit, 0);
  let mask = parseCabins(new URLSearchParams(location.search).get("cabins"))
    ?? (getFilter() ?? allMask);
  mask &= allMask; if (!mask) mask = allMask;
  setFilter(mask); // a shared map link's filter should also govern the pages behind its dots
  // -1 = next 12 months. Seeded from ?month=YYYY-MM like nights/cabins — a
  // calendar month, not an index, so a shared URL keeps meaning as months
  // roll over; one that has rolled out of the window quietly falls back to
  // "any time".
  let monthIdx = -1;
  {
    const mq = new URLSearchParams(location.search).get("month");
    if (/^\d{4}-\d{2}$/.test(mq || "")) {
      const mi = next12Months().findIndex((mo) => monthISO(mo) === mq);
      if (mi >= 0) monthIdx = mi;
    }
  }
  let nights = parseNights(new URLSearchParams(location.search).get("nights"))
    || getNightsPref() || NIGHTS_DEFAULT.slice();
  // Party size: same seed order and, like cabins, a shared map link's pax
  // governs the pages behind its dots via the session pref.
  let pax = activePax();
  const conn = getConnPref() ?? 1; // via metrics match the calendar behind the dot
  if (store.hasAnySeatData) setPaxPref(pax);
  const controls = el(`<div class="section-pad map-controls">
    <div class="chips" role="group" aria-label="Cabins">${cabinLegend().map(([bit, label]) =>
      `<button type="button" class="chip" aria-pressed="${!!(mask & bit)}" data-bit="${bit}">
        <span class="swatch ${bitClass(bit)}"></span>${esc(label)}</button>`).join("")}
    </div>
    <label class="map-month-wrap">When
      <select class="map-month" aria-label="Travel month">
        <option value="-1"${monthIdx < 0 ? " selected" : ""}>Any time (next 12 months)</option>
        ${next12Months().map((mo, i) =>
          `<option value="${i}"${i === monthIdx ? " selected" : ""}>${esc(mo.label)}</option>`).join("")}
      </select>
    </label>
    <div class="nights-ctl stops-ctl" role="group" aria-label="Maximum stops">
      <span class="nc-label">Stops</span>
      <button type="button" class="np" data-stops="0" aria-pressed="${stops === 0}">Nonstop</button>
      <button type="button" class="np" data-stops="1" aria-pressed="${stops === 1}">≤1 stop</button>
    </div>
    <div class="nights-ctl" role="group" aria-label="Trip length in nights">
      <span class="nc-label">Trip length</span>
      ${NIGHTS_PRESETS.map(([label, lo, hi]) =>
        `<button type="button" class="np" data-lo="${lo}" data-hi="${hi}" aria-pressed="false">
           ${esc(label)} <span class="np-r">${lo}–${hi}</span></button>`).join("")}
      <span class="np-custom">
        <input id="np-min" type="number" inputmode="numeric" min="1" max="60" aria-label="Minimum nights">
        <span aria-hidden="true">–</span>
        <input id="np-max" type="number" inputmode="numeric" min="1" max="60" aria-label="Maximum nights">
        <span class="np-unit">nights</span>
      </span>
    </div>
    <p class="map-count" role="status"></p>
  </div>`);
  // The pax control renders only when the bundle carries seat data anywhere
  // (a control that never changes the map reads as broken).
  let onPaxChange = () => {};
  if (store.hasAnySeatData) {
    $(".map-count", controls).before(paxControl(pax, (n) => {
      pax = n;
      setPaxPref(n);
      mirrorPaxURL(n);
      onPaxChange();
    }));
  }
  mainEl.append(controls);

  /* Same behavior as the trip page's control: presets + clamped custom
     inputs, persisted to the session pref and mirrored into the URL so the
     map stays shareable and dot links inherit the window via nightsQS(). */
  const minIn = $("#np-min", controls), maxIn = $("#np-max", controls);
  let onNightsChange = () => {};
  function syncNights() {
    // [data-lo]: defensive — only nights presets, never other chip rows.
    for (const b of controls.querySelectorAll(".np[data-lo]")) {
      b.setAttribute("aria-pressed",
        Number(b.dataset.lo) === nights[0] && Number(b.dataset.hi) === nights[1] ? "true" : "false");
    }
    minIn.value = nights[0]; maxIn.value = nights[1];
  }
  function applyNights(lo, hi) {
    nights = [lo, hi];
    setNightsPref(lo, hi);
    const u = new URL(location.href);
    u.searchParams.set("nights", `${lo}-${hi}`);
    history.replaceState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
    syncNights();
    onNightsChange();
  }
  const clampN = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
  function nightsFromInputs(which) {
    let lo = clampN(Math.round(Number(minIn.value)) || nights[0], 1, 60);
    let hi = clampN(Math.round(Number(maxIn.value)) || nights[1], 1, 60);
    if (lo > hi) { if (which === "min") hi = lo; else lo = hi; }
    applyNights(lo, hi);
  }
  controls.querySelectorAll(".np[data-lo]").forEach((b) =>
    b.addEventListener("click", () => applyNights(Number(b.dataset.lo), Number(b.dataset.hi))));
  minIn.addEventListener("change", () => nightsFromInputs("min"));
  maxIn.addEventListener("change", () => nightsFromInputs("max"));
  syncNights();

  const panel = el(`<div class="map-panel">
    <div class="map-loading"><div class="sk-line" style="height:200px"></div></div>
  </div>`);
  mainEl.append(panel, el(`<p class="map-legend">Dot colour is the best cabin
    available; dot size is how many days have round trips. Hover a dot to see
    its route — one-stop journeys show their overnight connection. Drag to
    pan, scroll or pinch to zoom.</p>`));

  loadWorld().then(() => {
    if (current.page !== "map" || current.params.o !== o) return; // navigated away
    drawMap();
  }).catch(() => {
    panel.innerHTML = "";
    panel.append(el(`<div class="empty-state"><div class="big">The map couldn't load.</div>
      <p><a href="/from/${o}">The list view</a> has every destination.</p></div>`));
  });

  /* Days-with-round-trips for one destination inside the active month window,
     plus the best (highest) cabin seen — that cabin colors the dot. At a
     party size >= 2, a pair without seat data on both legs stays on the map
     at any-party counts with `unverified` set (excluding it would make the
     map lie by omission; including it unmarked, by assertion). */
  function metric(spot) {
    const d = spot.d;
    let rt, known;
    if (spot.via) {
      // The whole chain must complete: cabins coupled on the long legs, hops
      // any space, overnight stops at the hub. No hop seat data -> any-party.
      known = pax <= 1;
      const fullPath = [o, spot.via, d, spot.via, o];
      rt = spot.both
        ? chainBits(fullPath, mask, [[1, conn], [nights[0], nights[1]], [1, conn]], 1, focusLegs(fullPath))
        : new Uint8Array(store.bundle.days);
    } else {
      known = pax <= 1 || pairSeatsKnown(`${o}-${d}`, `${d}-${o}`);
      rt = roundTripBits(`${o}-${d}`, `${d}-${o}`, mask, nights[0], nights[1], known ? pax : 1);
    }
    const t0 = Math.max(0, todayIndex());
    const mo = monthIdx >= 0 ? next12Months()[monthIdx] : null;
    const from = mo ? Math.max(mo.start, t0) : t0;
    const to = mo ? Math.min(mo.end, rt.length) : rt.length;
    let days = 0, union = 0;
    for (let i = from; i < to; i++) if (rt[i]) { days++; union |= rt[i]; }
    let best = 0;
    for (let bit = 8; bit >= 1; bit >>= 1) if (union & bit) { best = bit; break; }
    return { days, best, unverified: pax > 1 && !known };
  }

  function drawMap() {
    const W = window.WORLD1;
    panel.innerHTML = "";
    const svg = el(`<svg class="map-svg" viewBox="0 0 ${W.w} ${W.h}" role="group"
        aria-label="World map of destinations from ${esc(placeName(o))}">
      <path class="map-sea" d="${W.sphere}"></path>
      <path class="map-land" d="${W.land}"></path>
      <g class="map-paths" aria-hidden="true"></g>
      <g class="map-dots"></g>
    </svg>`);
    const tip = el(`<div class="tip map-tip" hidden></div>`);
    const zoom = el(`<div class="map-zoom" role="group" aria-label="Zoom">
      <button type="button" data-z="in" aria-label="Zoom in">+</button>
      <button type="button" data-z="out" aria-label="Zoom out">−</button>
      <button type="button" data-z="reset" aria-label="Reset view">⌂</button>
    </div>`);
    panel.append(svg, tip, zoom);

    const dotsG = $(".map-dots", svg);
    const view = { x: 0, y: 0, w: W.w }; // viewBox subset; height tracks aspect
    const kOf = () => W.w / view.w;
    const og = store.bundle.places[o]?.g;

    /* el() parses in the HTML namespace, so circles are built as one string
       and parsed by the <g> itself (SVG context). */
    function redraw() {
      let shown = 0, unplaced = 0;
      const parts = [];
      let originDot = "";
      if (og) {
        const [ox, oy] = projectLL(og[0], og[1]);
        originDot = `<circle class="map-origin" cx="${ox}" cy="${oy}" r="${4 / kOf()}"></circle>`;
      }
      let unverifiedShown = 0, viaShown = 0;
      const viaParts = [];
      const poolSize = stops ? spots.length : spots.length - vias.size;
      for (const spot of spots) {
        if (!stops && spot.via) continue;
        const d = spot.d;
        const g = store.bundle.places[d]?.g;
        const { days, best, unverified } = metric(spot);
        if (!days) continue;
        if (!g) { unplaced++; continue; }
        shown++;
        if (unverified) unverifiedShown++;
        if (spot.via) viaShown++;
        const [x, y] = projectLL(g[0], g[1]);
        // Radius grows with the square root of match-days so area ~ days.
        const r = (2.1 + Math.sqrt(days) * 0.38) / kOf();
        (spot.via ? viaParts : parts).push(`<circle class="map-dot ${bitClass(best)}${unverified ? " map-dot-unv" : ""}${spot.via ? " map-dot-via" : ""}" cx="${x}" cy="${y}" r="${r}"
          tabindex="0" role="link" data-code="${d}" data-days="${days}" data-best="${best}"${unverified ? ` data-unv="1"` : ""}${spot.via ? ` data-via="${spot.via}"` : ""}
          aria-label="${esc(placeName(d))}: ${days} day${days > 1 ? "s" : ""} with round trips${spot.via ? ` via ${esc(placeName(spot.via))} (overnight stop)` : ""}${
            pax > 1 ? (unverified ? " — seat counts pending, shown for any party" : ` for ${pax} travelling together`) : ""} — open calendar"></circle>`);
      }
      // Paint order (SVG: later wins): via dots underneath, nonstop dots
      // above them, the origin marker on top.
      dotsG.innerHTML = viaParts.join("") + parts.join("") + originDot;
      pathsG.innerHTML = ""; // hover state doesn't survive a redraw
      const monthLabel = monthIdx >= 0 ? ` in ${next12Months()[monthIdx].label}` : "";
      const nightsLabel = nights[0] === NIGHTS_DEFAULT[0] && nights[1] === NIGHTS_DEFAULT[1]
        ? "" : ` of ${nights[0]}–${nights[1]} nights`;
      const paxLabel = pax > 1 ? ` for ${pax} travelling together` : "";
      $(".map-count", controls).textContent =
        `${shown} of ${poolSize} destination${poolSize === 1 ? " has" : "s have"} round trips${paxLabel}${nightsLabel}${monthLabel} in the cabins you picked` +
        (!stops && vias.size ? ` (nonstop only — ${vias.size} more with one stop)` : "") +
        (viaShown ? ` — ${viaShown} with an overnight stop` : "") +
        (unverifiedShown ? ` (${unverifiedShown} shown for any party — seat counts pending)` : "") +
        (unplaced ? ` (${unplaced} more can't be placed on the map yet — see the list)` : "") + ".";
    }

    /* Tooltip follows hover/focus; the dot itself is the link. */
    function showMapTip(dot) {
      const d = dot.dataset.code, days = dot.dataset.days, best = Number(dot.dataset.best);
      const label = (cabinLegend().find(([bit]) => bit === best) || [0, ""])[1];
      tip.innerHTML = `<div class="t-date">${esc(placeName(d))} <span class="map-tip-code">${d}</span></div>
        <div class="t-cab"><span class="swatch ${bitClass(best)}"></span>
          ${days} day${days === "1" ? "" : "s"} with round trips${pax > 1 && !dot.dataset.unv ? ` for ${pax}` : ""} · best: ${esc(label)}</div>
        ${dot.dataset.via ? `<div class="t-note">via ${esc(placeName(dot.dataset.via))} — overnight stop each way</div>` : ""}
        ${dot.dataset.unv ? `<div class="t-note">seat counts not yet available — shown for any party</div>` : ""}`;
      tip.hidden = false;
      const pr = panel.getBoundingClientRect(), dr = dot.getBoundingClientRect();
      const x = Math.min(pr.width - tip.offsetWidth - 8, Math.max(8, dr.left - pr.left + dr.width / 2 - tip.offsetWidth / 2));
      const above = dr.top - pr.top - tip.offsetHeight - 8;
      tip.style.left = `${x}px`;
      tip.style.top = `${above > 4 ? above : dr.bottom - pr.top + 8}px`;
    }
    /* Hovering (or focusing) a dot draws its route: one great-circle arc
       nonstop, two arcs with a ringed connection point for a via journey —
       the routing appears exactly when you ask about a destination, instead
       of a permanent visual treatment on the dot. */
    const pathsG = $(".map-paths", svg);
    function showPath(dot) {
      hidePath();
      if (!og) return;
      const dg = store.bundle.places[dot.dataset.code]?.g;
      if (!dg) return;
      // The route owns the map while you ask about it: every dot off the
      // route dims so the arcs read through dense clusters (the hub dot —
      // always also a direct destination — stays lit with the hovered one).
      dotsG.classList.add("routing");
      dot.classList.add("route-on");
      if (dot.dataset.via) {
        dotsG.querySelector(`.map-dot[data-code="${dot.dataset.via}"]`)?.classList.add("route-on");
      }
      const cab = bitClass(Number(dot.dataset.best));
      const hubG = dot.dataset.via ? store.bundle.places[dot.dataset.via]?.g : null;
      const segs = hubG ? [[og, hubG], [hubG, dg]] : [[og, dg]];
      const w = 1.3 / kOf();
      let html = "";
      for (const [a, b] of segs) {
        for (const run of gcRuns(a, b)) {
          html += `<polyline class="map-path ${cab}" stroke-width="${w}" points="${run.map(([x, y]) => `${x},${y}`).join(" ")}"></polyline>`;
        }
      }
      if (hubG) {
        const [hx, hy] = projectLL(hubG[0], hubG[1]);
        html += `<circle class="map-path-hub" cx="${hx}" cy="${hy}" r="${2.6 / kOf()}" stroke-width="${w}"></circle>`;
      }
      pathsG.innerHTML = html;
    }
    function hidePath() {
      pathsG.innerHTML = "";
      dotsG.classList.remove("routing");
      for (const d of dotsG.querySelectorAll(".route-on")) d.classList.remove("route-on");
    }
    dotsG.addEventListener("pointerover", (e) => { const d = e.target.closest(".map-dot"); if (d) { showMapTip(d); showPath(d); } });
    dotsG.addEventListener("pointerout", () => { tip.hidden = true; hidePath(); });
    dotsG.addEventListener("focusin", (e) => { const d = e.target.closest(".map-dot"); if (d) { showMapTip(d); showPath(d); } });
    dotsG.addEventListener("focusout", () => { tip.hidden = true; hidePath(); });
    /* A click that wobbles across two overlapping dots presses on one and
       releases on the other, so the browser fires the click on their common
       ancestor (the group) — the dot the press started on is the intent. */
    let downDot = null;
    svg.addEventListener("pointerdown", (e) => { downDot = e.target.closest(".map-dot"); });
    svg.addEventListener("click", (e) => {
      if (panned) return;
      const d = e.target.closest(".map-dot") || downDot;
      if (d) navigate(`/trip/${o}-${d.dataset.code}${nightsQS()}`);
    });
    dotsG.addEventListener("keydown", (e) => {
      const d = e.target.closest(".map-dot");
      if (d && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); navigate(`/trip/${o}-${d.dataset.code}${nightsQS()}`); }
    });

    /* Pan/zoom: wheel + drag + pinch + buttons, clamped to the sphere. */
    function applyView() {
      const h = W.h * (view.w / W.w);
      view.x = Math.max(0, Math.min(W.w - view.w, view.x));
      view.y = Math.max(0, Math.min(W.h - h, view.y));
      svg.setAttribute("viewBox", `${view.x} ${view.y} ${view.w} ${h}`);
      redraw();
    }
    function zoomAt(factor, px, py) { // px,py in current viewBox coords
      const w2 = Math.max(70, Math.min(W.w, view.w / factor));
      const f = w2 / view.w;
      view.x = px - (px - view.x) * f;
      view.y = py - (py - view.y) * f;
      view.w = w2;
      applyView();
    }
    const toBox = (e) => {
      const r = svg.getBoundingClientRect();
      return [view.x + ((e.clientX - r.left) / r.width) * view.w,
              view.y + ((e.clientY - r.top) / r.height) * (W.h * view.w / W.w)];
    };
    svg.addEventListener("wheel", (e) => {
      e.preventDefault();
      const [px, py] = toBox(e);
      zoomAt(e.deltaY < 0 ? 1.3 : 1 / 1.3, px, py);
    }, { passive: false });
    zoom.addEventListener("click", (e) => {
      const b = e.target.closest("button");
      if (!b) return;
      if (b.dataset.z === "reset") { view.x = 0; view.y = 0; view.w = W.w; applyView(); return; }
      const h = W.h * (view.w / W.w);
      zoomAt(b.dataset.z === "in" ? 1.5 : 1 / 1.5, view.x + view.w / 2, view.y + h / 2);
    });
    /* Drag to pan; pinch to zoom. `panned` suppresses the click that ends a
       REAL drag so letting go over a dot doesn't teleport you to its route —
       but ordinary click jitter (a few px of mouse or finger wobble) must
       still count as a click, so it only trips past a distance budget. */
    let panned = false, dragDist = 0;
    const ptrs = new Map();
    let pinchD = 0;
    /* Capturing on pointerdown would retarget the eventual click to the svg
       (pointer capture redirects the click event to the capturing element),
       which silently killed dot clicks. So the pointer is captured only once
       a drag has actually begun — a plain click never captures at all. */
    const captureAll = () => { for (const id of ptrs.keys()) { try { svg.setPointerCapture(id); } catch {} } };
    svg.addEventListener("pointerdown", (e) => {
      ptrs.set(e.pointerId, [e.clientX, e.clientY]);
      if (ptrs.size === 2) {
        const [a, b] = [...ptrs.values()];
        pinchD = Math.hypot(a[0] - b[0], a[1] - b[1]);
      }
      panned = false; dragDist = 0;
    });
    svg.addEventListener("pointermove", (e) => {
      if (!ptrs.has(e.pointerId)) return;
      const prev = ptrs.get(e.pointerId);
      ptrs.set(e.pointerId, [e.clientX, e.clientY]);
      const r = svg.getBoundingClientRect();
      if (ptrs.size === 1) {
        dragDist += Math.abs(e.clientX - prev[0]) + Math.abs(e.clientY - prev[1]);
        if (!panned && dragDist > 6) { panned = true; captureAll(); }
        if (!panned) return; // a click's jitter must not wobble the view
        const dx = (e.clientX - prev[0]) / r.width * view.w;
        const dy = (e.clientY - prev[1]) / r.height * (W.h * view.w / W.w);
        view.x -= dx; view.y -= dy;
        applyViewNoRedraw();
      } else if (ptrs.size === 2) {
        const [a, b] = [...ptrs.values()];
        const d2 = Math.hypot(a[0] - b[0], a[1] - b[1]);
        if (pinchD > 0 && Math.abs(d2 - pinchD) > 2) {
          if (!panned) { panned = true; captureAll(); }
          const cx = (a[0] + b[0]) / 2, cy = (a[1] + b[1]) / 2;
          const px = view.x + ((cx - r.left) / r.width) * view.w;
          const py = view.y + ((cy - r.top) / r.height) * (W.h * view.w / W.w);
          zoomAt(d2 / pinchD, px, py);
          pinchD = d2;
        }
      }
    });
    const endPtr = (e) => {
      ptrs.delete(e.pointerId);
      if (ptrs.size < 2) pinchD = 0;
      if (!ptrs.size && panned) redraw(); // resize dots once the gesture ends
    };
    /* Listening at the window (not the svg) catches every release: bubbled
       on-svg ones, captured mid-drag ones, and — the case that matters —
       releases outside the svg from an uncaptured not-yet-a-drag press,
       whose stale entry would make the next one-finger drag look like a
       pinch. Removes itself once this map instance is gone. */
    const winEnd = (e) => {
      if (!svg.isConnected) {
        window.removeEventListener("pointerup", winEnd);
        window.removeEventListener("pointercancel", winEnd);
        return;
      }
      endPtr(e);
    };
    window.addEventListener("pointerup", winEnd);
    window.addEventListener("pointercancel", winEnd);
    /* Cheap pan while the finger is down: move the viewBox but keep dot sizes;
       redraw (which resizes dots) happens once at gesture end. */
    function applyViewNoRedraw() {
      const h = W.h * (view.w / W.w);
      view.x = Math.max(0, Math.min(W.w - view.w, view.x));
      view.y = Math.max(0, Math.min(W.h - h, view.y));
      svg.setAttribute("viewBox", `${view.x} ${view.y} ${view.w} ${h}`);
    }

    /* Filters re-count and re-color without touching the viewport. */
    controls.addEventListener("click", (e) => {
      const chip = e.target.closest(".chip[data-bit]");
      if (!chip) return;
      const bit = Number(chip.dataset.bit);
      const next = mask ^ bit;
      if (!next) return; // at least one cabin stays on
      mask = next;
      chip.setAttribute("aria-pressed", String(!!(mask & bit)));
      setFilter(mask);
      const u = new URL(location.href);
      if (mask === allMask) u.searchParams.delete("cabins");
      else u.searchParams.set("cabins", cabinLetters(mask));
      const q = u.searchParams.toString();
      history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
      redraw();
    });
    $(".map-month", controls).addEventListener("change", (e) => {
      monthIdx = Number(e.target.value);
      const u = new URL(location.href);
      if (monthIdx < 0) u.searchParams.delete("month");
      else u.searchParams.set("month", monthISO(next12Months()[monthIdx]));
      const q = u.searchParams.toString();
      history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
      redraw();
    });
    controls.querySelectorAll(".stops-ctl .np").forEach((b) => b.addEventListener("click", () => {
      stops = Number(b.dataset.stops);
      controls.querySelectorAll(".stops-ctl .np").forEach((x) =>
        x.setAttribute("aria-pressed", String(Number(x.dataset.stops) === stops)));
      const u = new URL(location.href);
      if (stops === 1) u.searchParams.delete("stops"); else u.searchParams.set("stops", "0");
      const q = u.searchParams.toString();
      history.replaceState(null, "", u.pathname + (q ? `?${q}` : ""));
      redraw();
    }));
    onNightsChange = redraw;
    onPaxChange = redraw;

    applyView();
  }
}

/* ---------------- pages: my alerts ---------------- */

/* The header link doubles as the discovery path for alerts, so it carries the
   count. Kept in sync by refreshAlertCount() after any change. */
function refreshAlertCount() {
  const link = $("#alerts-link");
  if (!link) return;
  const apply = (n) => {
    link.hidden = false;
    $(".al-count", link).textContent = n ? String(n) : "";
    link.classList.toggle("on", n > 0);
    link.title = n ? `${n} route${n > 1 ? "s" : ""} you're watching` : "Alerts";
  };
  // Instant from the last-known-good copy; the server truth reconciles.
  const cached = Notification?.permission === "granted" ? loadWatchesCache() : null;
  if (cached) apply(cached.length);
  currentAlerts().then((data) => apply(data ? data.watches.length : 0));
}

function renderAlerts() {
  current.page = "alerts"; current.params = null;
  setTitle("My alerts");
  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="section-pad">
    <p class="crumbs"><a href="/">Search</a></p>
    <h1 class="page-title">My alerts</h1>
    <p class="page-sub">We'll notify you the moment award space opens on a route you're watching —
      free, instantly, in whichever cabins you pick.</p>
  </div>`));

  const body = el(`<div id="alerts-page-body"><div class="sk-line" style="height:80px;margin-top:22px"></div></div>`);
  mainEl.append(body);
  drawAlertsPage(body);
}

async function drawAlertsPage(body) {
  body.innerHTML = "";

  // Cases where the browser can't do alerts at all — explain, don't pretend.
  if (!pushSupported()) {
    body.append(el(`<div class="empty-state">
      <div class="big">${isIOS() && !isStandalone() ? "Add Reward Flights to your Home Screen" : "This browser can't do alerts"}</div>
      <p>${isIOS() && !isStandalone()
        ? `iPhone only allows notifications for installed web apps. Tap <b>Share</b>, then
           <b>Add to Home Screen</b>, open Reward Flights from there, and your alerts will work.`
        : `It doesn't support push notifications. Chrome, Firefox, Edge and Safari all do.`}</p>
    </div>`));
    return;
  }
  if (Notification.permission === "denied") {
    body.append(el(`<div class="empty-state"><div class="big">Notifications are blocked</div>
      <p>${permissionHelpHTML("denied")}</p></div>`));
    return;
  }

  const data = await currentAlerts();
  const watches = data ? data.watches : [];

  if (!watches.length) {
    body.append(el(`<div class="empty-state">
      <div class="big">You're not watching anything yet</div>
      <p>Open any route and hit <b>🔔 Alert me</b> to be told the moment award space opens —
         pick the cabins you care about and when you can actually travel, and we'll only
         bother you when a trip you could really take becomes bookable.</p>
      <p><a class="btn" href="/">Find a route</a></p>
    </div>`));
    body.append(alertsFooterHTML());
    return;
  }

  // The device may be silently unreachable — we've pushed alerts it never
  // acknowledged. Say so, because nothing else will.
  if (data.device && data.device.reachable === false && data.device.pushCount > 0) {
    body.append(el(`<div class="reach-warn">
      <b>This device may not be receiving your alerts.</b>
      We've sent ${data.device.pushCount} notification${data.device.pushCount > 1 ? "s" : ""} that it
      never confirmed — usually your operating system is blocking this browser's notifications.
      Hit <b>Send a test notification</b> below; if it doesn't appear, check
      ${/Macintosh|Mac OS X/.test(navigator.userAgent)
        ? "System Settings → Notifications for this browser"
        : "your device's notification settings for this browser"}.
    </div>`));
  }

  const head = el(`<div class="alerts-head">
    <p class="alerts-count">Watching <b>${watches.length}</b> route${watches.length > 1 ? "s" : ""}</p>
    <div class="alerts-actions">
      <button type="button" class="btn" id="alerts-test">Send a test notification</button>
      <button type="button" class="alerts-off-all" id="alerts-all-off">Turn all off</button>
    </div>
  </div>`);
  body.append(head);

  const list = el(`<div class="card-list alerts-list"></div>`);
  const rerender = () => drawAlertsPage(body);
  const seen = loadSeen();
  for (const w of watches) list.append(alertRow(w, data, rerender, { editable: true, seen }));
  body.append(list, el(`<p class="alerts-status" role="status"></p>`), troubleshootHTML(), alertsFooterHTML());
  // Now that they've seen it, this becomes the new baseline. Deferred so the
  // "new since you last looked" line is what they actually read on this visit.
  markSeen(watches);

  $("#alerts-all-off", head).addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true; btn.textContent = "…";
    try {
      await saveWatches(data.sub, []);
      announce("All alerts turned off.");
      refreshAlertCount();
      rerender();
    } catch (err) {
      if (err.queued) {
        announce("You're offline — all alerts will switch off when you're back online.");
        data.watches = [];
        rerender();
        return;
      }
      btn.disabled = false; btn.textContent = "Turn all off";
      status(body, String(err.message || err));
    }
  });

  // Proof it works, without waiting for real availability to open.
  $("#alerts-test", head).addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true; btn.textContent = "Sending…";
    try {
      // retries: 0 — a retried /test could fire a duplicate notification.
      const res = await apiFetch("/test", { method: "POST", body: { endpoint: data.sub.endpoint }, retries: 0 });
      if (res.status === 410) {
        // The push service retired this device's endpoint; the server has
        // already dropped it. Re-subscribe transparently and keep the alerts.
        status(body, "Renewing this device's notifications…");
        await renewSubscription(data.watches);
        // Re-render FIRST (it rebuilds the status line from scratch), then
        // report — otherwise the message is wiped the moment it's set.
        await drawAlertsPage(body);
        status(body, "Renewed this device — press the test button once more.");
        return;
      }
      if (res.status === 429) throw new Error("That's enough tests for now — try again later.");
      if (!res.ok) throw new Error(`Couldn't send a test (${res.status}).`);

      // Fire a LOCAL notification too. It never touches the network, so the two
      // together tell the user which half is broken:
      //   neither appears  → the OS is blocking this browser (macOS does this
      //                      silently; the site permission still reads "granted")
      //   only this one    → we sent it but it didn't reach the device
      // Without this, "nothing happened" is indistinguishable from "we failed".
      try {
        const reg = await ensureSW();
        await reg.showNotification("Notifications are working here", {
          body: "This one came from your browser. The other came from our server — if you only see this one, tell us.",
          tag: "rf_local_check",
          icon: "/assets/icon-192.png",
        });
      } catch { /* local check is a diagnostic, never a failure */ }
      status(body, "Sent two notifications — one from your browser, one from our server.");
      // Whatever the outcome, say where to look. On macOS a blocked browser is
      // the usual culprit and nothing in the browser hints at it.
      const help = $(".alerts-help", body);
      if (help) help.hidden = false;
    } catch (err) {
      status(body, String(err.message || err));
    } finally {
      btn.disabled = false; btn.textContent = "Send a test notification";
    }
  });
}

function status(body, msg) {
  const el2 = $(".alerts-status", body);
  if (el2) { el2.textContent = msg; announce(msg); }
}

/* Shown after a test: the browser can say "granted" while the OS silently drops
   every notification, and nothing in the browser hints at it. */
function troubleshootHTML() {
  const mac = /Macintosh|Mac OS X/.test(navigator.userAgent);
  const chromey = /Chrome|Chromium|Edg/.test(navigator.userAgent) && !/Firefox/.test(navigator.userAgent);
  const browser = /Edg/.test(navigator.userAgent) ? "Microsoft Edge"
    : /Firefox/.test(navigator.userAgent) ? "Firefox"
      : /Chrome|Chromium/.test(navigator.userAgent) ? "Google Chrome" : "your browser";
  return el(`<div class="alerts-help" hidden>
    <h2>Didn't see them?</h2>
    <ul>
      ${mac ? `<li><b>macOS blocks notifications per app</b>, separately from the website's
        permission — this is the usual culprit, and nothing in the browser tells you.
        Open <b>System Settings → Notifications → ${esc(browser)}</b> and turn
        <b>Allow notifications</b> on. Check <b>Do Not Disturb / Focus</b> is off too.</li>` : ""}
      ${isIOS() ? `<li><b>On iPhone</b>, alerts only work if you opened Reward Flights from your
        <b>Home Screen</b> (Share → Add to Home Screen), not from a Safari tab.</li>` : ""}
      <li>If you saw the <b>browser</b> notification but not the <b>server</b> one, delivery is the
        problem, not your settings — turn an alert off and on again to renew this device's
        subscription, then test once more.</li>
      ${chromey ? `<li>Notifications can also be muted for the site itself: click the icon at the
        left of the address bar → <b>Notifications</b>.</li>` : ""}
    </ul>
  </div>`);
}

function alertsFooterHTML() {
  return el(`<div class="alerts-explainer">
    <h2>How alerts work</h2>
    <ul>
      <li><b>Round trips are checked as a whole.</b> We only tell you a round trip has opened when
        <i>both</i> legs have award space in the same cabin, within your trip-length window — not
        when "something changed" on the route.</li>
      <li><b>No spam.</b> Award space flickers; we hold a cooldown per route and cabin, and batch
        changes, so a churny route can't flood you.</li>
      <li><b>Party-size alerts never guess.</b> A "2+ seats" watch fires only when the data shows
        that many seats together, in one cabin, on one flight, on both legs. Where we can't see
        seat counts for a route we say so and wait for real numbers — we don't alert on hope.</li>
      <li><b>Nothing about you is stored.</b> No account, no email — just an anonymous
        notification handle from your browser, which you can revoke here at any time.</li>
    </ul>
  </div>`);
}

function renderNotFound() {
  current.page = "404"; current.params = null;
  setTitle("Not found");
  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="empty-state section-pad">
    <div class="big">That page doesn't exist.</div>
    <p><a href="/">Back to search</a></p></div>`));
}

/* ---------------- go ---------------- */

boot();
