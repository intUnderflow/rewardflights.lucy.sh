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
const MANIFEST_POLL_MS = 5 * 60 * 1000;
const DAY_MS = 86400000;
const NEW_BADGE_MS = 48 * 3600 * 1000;

/* Cabin bit → color class. Position in the seat-stack is ascending bit
   value (Economy at the bottom, First on top); unknown bits render gray. */
const BIT_CLASS = { 1: "cab-m", 2: "cab-w", 4: "cab-c", 8: "cab-f" };
const bitClass = (bit) => BIT_CLASS[bit] || "cab-x";

/* Cabin bit → BA redemption CabinCode (M/W/C/F). */
const CABIN_CODE = { 1: "M", 2: "W", 4: "C", 8: "F" };

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
   granularity of our data. A return date is optional. */
function baBookingURL(origin, dest, outIso, bit, returnIso) {
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
    ["NumberOfAdults", "1"],
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

async function fetchWatches(sub) {
  const res = await fetch(`${PUSH_API}/watches?endpoint=${encodeURIComponent(sub.endpoint)}`);
  if (!res.ok) throw new Error(`alert service unavailable (${res.status})`);
  return (await res.json()).watches || [];
}

async function saveWatches(sub, watches) {
  if (!watches.length) {
    // Nothing left to watch: drop the subscription rather than keep a dangling
    // endpoint on file.
    await fetch(`${PUSH_API}/unsubscribe`, {
      method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ endpoint: sub.endpoint }),
    });
    await sub.unsubscribe().catch(() => {});
    return;
  }
  const res = await fetch(`${PUSH_API}/subscribe`, {
    method: "POST", headers: { "content-type": "application/json" },
    body: JSON.stringify({ ...subPayload(sub), watches }),
  });
  if (!res.ok) {
    const detail = await res.json().catch(() => ({}));
    throw new Error(detail.error || `couldn't save alerts (${res.status})`);
  }
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
  if (!store.bundle || !store.bundle.routes[w.route]) return empty;
  const mask = watchMask(w);
  if (!mask) return empty;

  const t0 = Math.max(0, todayIndex());
  const last = store.bundle.days - 1;
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
  const oFrom = w.out?.from ? clamp(isoToIdx(w.out.from), t0, last) : t0;
  const oTo = w.out?.to ? clamp(isoToIdx(w.out.to), t0, last) : last;

  const out = routeBits(w.route);
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
  const ret = routeBits(rev);
  const [minN, maxN] = w.nights ? [w.nights.min, w.nights.max] : NIGHTS_ANY;
  // No return range given → it's implied by the outbound window + nights.
  const rFrom = w.ret?.from ? clamp(isoToIdx(w.ret.from), t0, last) : t0;
  const rTo = w.ret?.to ? clamp(isoToIdx(w.ret.to), t0, last) : last;

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
  if (w.kind === "rt" && !store.bundle?.routes?.[reverseRoute(w.route)]) {
    return "There's no return route in the data, so a round trip can't be found.";
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
function watchSummary(w) {
  const bits = [];
  bits.push(w.out ? `Out ${fmtRange(w.out.from, w.out.to)}` : "Any date");
  if (w.kind === "rt") {
    if (w.ret) bits.push(`back ${fmtRange(w.ret.from, w.ret.to)}`);
    if (w.nights) bits.push(`${w.nights.min}–${w.nights.max} nights`);
  }
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
    return { sub, watches: await fetchWatches(sub) };
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
      if (!res.ok) throw new Error(`HTTP ${res.status} for ${url}`);
      return await res.json();
    } catch (err) {
      if (left > 0) {
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
  monthCache: new Map(),
  rtCache: new Map(),
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
  for (const key of Object.keys(bundle.routes)) {
    const [o, d] = key.split("-");
    store.origins.add(o);
    if (!store.destsByOrigin.has(o)) store.destsByOrigin.set(o, []);
    store.destsByOrigin.get(o).push(d);
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
   from the current month). Cached. */
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

function routeTotals(routeKey) {
  const bits = routeBits(routeKey);
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
function roundTripBits(outKey, retKey, mask, minNights, maxNights) {
  const t0 = Math.max(0, todayIndex());
  const ck = `${outKey}|${retKey}|${mask}|${minNights}|${maxNights}|${t0}`;
  if (store.rtCache.has(ck)) return store.rtCache.get(ck);
  const out = routeBits(outKey);
  const ret = store.bundle.routes[retKey] ? routeBits(retKey) : null;
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
  elx.textContent = `data as of ${freshLabel()}`;
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
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") checkForUpdate();
  });
}

async function checkForUpdate() {
  if (document.visibilityState !== "visible" || !store.bundle) return;
  try {
    // Cache-bust the tiny manifest so we see new versions promptly.
    const manifest = await getJSON(`${dataBase}/manifest.json?ts=${Math.floor(Date.now() / 60000)}`, { retries: 0 });
    if (manifest.v && manifest.v !== store.bundle.v) {
      // New generation: bypass the 5-minute CDN cache via a versioned URL.
      const fresh = await getJSON(`${dataBase}/availability.json?v=${encodeURIComponent(manifest.v)}`);
      // Guard against a stale CDN replica: only adopt when the bundle we got
      // back actually IS a new version. Otherwise we'd re-adopt the same data
      // and fire a false "updated" pulse + destructive re-render every poll.
      if (fresh.v && fresh.v !== store.bundle.v) {
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
      }
    }
    hideBanner();
  } catch { /* silent — next poll retries */ }
}

function isTypingInSearch() {
  const a = document.activeElement;
  return !!(a && a.tagName === "INPUT" && a.closest(".search-card"));
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
      const label = counts
        ? `${p.name}, ${p.country}. ${total} days with seats in the next year.`
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
  }));

  // Both ends picked → go. Unconditionally: the pair may exist in either
  // direction, only the reverse, or not at all — the target page's empty
  // state explains and links onward (/from/, the reverse trip), which beats
  // silently doing nothing here.
  function maybeGo() {
    if (!homeSel.origin || !homeSel.dest) { updateHint(); return; }
    const key = `${homeSel.origin}-${homeSel.dest}`;
    if (homeTripMode === "trip") {
      const [lo, hi] = homeNights();
      navigate(`/trip/${key}?nights=${lo}-${hi}`);
    } else navigate(`/route/${key}`);
  }

  function updateHint() {
    const hint = $("#home-hint");
    if (homeSel.origin && !homeSel.dest) {
      const n = (store.destsByOrigin.get(homeSel.origin) || []).length;
      hint.innerHTML = `<a href="/from/${homeSel.origin}">See all ${n} destinations from ${esc(placeName(homeSel.origin))} →</a>`;
    } else hint.textContent = "";
  }

  attachAutocomplete(originIn, {
    getRestrict: () => originsWithRoutes,
    sparkFor: (code) => monthCounts("origin", code),
    onPick: (p) => { homeSel.origin = p.code; originIn.value = p.name; updateHint(); destIn.focus(); maybeGo(); },
  });
  attachAutocomplete(destIn, {
    getRestrict: () => homeSel.origin
      ? new Set(store.destsByOrigin.get(homeSel.origin) || []) : allDests,
    sparkFor: (code) => homeSel.origin
      ? monthCounts("route", `${homeSel.origin}-${code}`) : null,
    onPick: (p) => { homeSel.dest = p.code; destIn.value = p.name; maybeGo(); },
  });
  originIn.addEventListener("input", () => { homeSel.origin = null; updateHint(); });
  destIn.addEventListener("input", () => { homeSel.dest = null; });
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
  const data = await currentAlerts();
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
    <span class="alert-when">${esc(watchSummary(w))}</span>
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
      await saveWatches(data.sub, data.watches.filter((x) => x.id !== w.id));
      announce(`Alerts off for ${o} to ${d}.`);
      refreshAlertCount();
      rerender();
    } catch (err) {
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
  const modules = el(`<div class="modules"></div>`);
  const opened = recentlyOpened();
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
  const top = topRoutes(6);
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
  if (modules.children.length) mount.append(modules);

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

function recentlyOpened() {
  if (!store.changes?.entries) return [];
  const byRoute = new Map();
  for (const e of store.changes.entries) {
    if (e.k !== "opened") continue;
    const g = byRoute.get(e.r) || { route: e.r, count: 0, t: 0 };
    g.count++; g.t = Math.max(g.t, e.t);
    byRoute.set(e.r, g);
  }
  return [...byRoute.values()]
    .filter((g) => store.bundle.routes[g.route])
    .sort((a, b) => b.t - a.t || b.count - a.count).slice(0, 6);
}

function topRoutes(n) {
  return Object.keys(store.bundle.routes)
    .map((key) => ({ key, total: routeTotals(key).total }))
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
  ["Weekend", 2, 4],
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

/* The current trip-length window as a query string for swap/cross links —
   the window is direction-agnostic, so it survives an origin/dest swap
   (picked dates do not: they belong to the old outbound direction). */
function nightsQS() {
  const w = parseNights(new URLSearchParams(location.search).get("nights")) || getNightsPref();
  return w ? `?nights=${w[0]}-${w[1]}` : "";
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

    if (!pushSupported()) {
      pop.innerHTML = isIOS() && !isStandalone()
        ? `<p class="bell-note"><b>Add Reward Flights to your Home Screen</b> to get alerts on iPhone —
           tap Share, then “Add to Home Screen”, and open it from there.</p>`
        : `<p class="bell-note">This browser doesn't support push notifications.</p>`;
      return;
    }
    if (Notification.permission === "denied") {
      pop.innerHTML = `<p class="bell-note">${permissionHelpHTML("denied")}</p>`;
      return;
    }

    pop.innerHTML = `<div class="sk-line" style="height:150px"></div>`;
    let all = [], mine = null;
    try {
      const sub = await getSubscription();               // no permission prompt yet
      if (sub) all = await fetchWatches(sub);
      mine = all.find((w) => w.route === routeKey && w.kind === kind) || null;
    } catch { /* store unreachable → offer a fresh panel anyway */ }

    renderPanel(mine, all);
  }

  function renderPanel(mine, all) {
    // Seed from the existing watch if there is one; otherwise from what the
    // user has already told this page (cabin filter + trip length + picked day).
    const seedMask = mine ? watchMask(mine) : (defaultMask || 15);
    const nights = mine?.nights
      ? [mine.nights.min, mine.nights.max]
      : (kind === "rt" ? (ctx.nights || NIGHTS_ANY) : null);
    let mode = mine?.out ? "exact" : (ctx.pickedOut && !mine ? "around" : "any");
    if (mine?.out && ctx.pickedOut && !mine.ret) mode = "exact";
    let flex = 7;
    let outFrom = mine?.out?.from || "", outTo = mine?.out?.to || "";
    let retFrom = mine?.ret?.from || "", retTo = mine?.ret?.to || "";

    pop.innerHTML = "";
    pop.append(el(`<p class="bell-title">${mine ? "Alerting you when a" : "Alert me when a"}
      ${kind === "rt" ? "round trip" : "one-way seat"} opens on ${o} ${arrow} ${d}</p>`));

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
    const save = el(`<button type="button" class="btn bell-save">${mine ? "Update alerts" : "Save alerts"}</button>`);
    pop.append(countLine, save);

    if (mine) {
      const off = el(`<button type="button" class="bell-offbtn">Turn off alerts for this route</button>`);
      off.addEventListener("click", () => commit(null, off));
      pop.append(off);
    }
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
        whenBody.append(el(`<p class="bell-hint">We'll alert you whenever space opens, on any date${
          kind === "rt" && nights ? ` (${nights[0]}–${nights[1]} nights)` : ""}.</p>`));
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
      const minIso = idxToIso(t0), maxIso = idxToIso(store.bundle.days - 1);
      whenBody.append(el(`<div class="bell-dates">
        <label>Leave between
          <span><input type="date" class="bd-of" min="${minIso}" max="${maxIso}" value="${esc(outFrom || defaultOut()[0])}">
          <input type="date" class="bd-ot" min="${minIso}" max="${maxIso}" value="${esc(outTo || defaultOut()[1])}"></span>
        </label>
        ${kind === "rt" ? `<label>Come back between
          <span><input type="date" class="bd-rf" min="${minIso}" max="${maxIso}" value="${esc(retFrom || "")}">
          <input type="date" class="bd-rt" min="${minIso}" max="${maxIso}" value="${esc(retTo || "")}"></span>
        </label>
        <p class="bell-hint">Leave the return blank and we'll work it out from your trip length${
          nights ? ` (${nights[0]}–${nights[1]} nights)` : ""}.</p>` : ""}
      </div>`));
      for (const i of whenBody.querySelectorAll("input")) {
        i.addEventListener("input", () => {
          outFrom = $(".bd-of", whenBody).value; outTo = $(".bd-ot", whenBody).value;
          if (kind === "rt") { retFrom = $(".bd-rf", whenBody)?.value || ""; retTo = $(".bd-rt", whenBody)?.value || ""; }
          recount();
        });
      }
    }

    function defaultOut() {
      const t0 = Math.max(0, todayIndex());
      const anchor = ctx.pickedOut ? dayIndexOf(ctx.pickedOut) : t0;
      return [idxToIso(Math.max(t0, anchor)), idxToIso(Math.min(store.bundle.days - 1, anchor + 30))];
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
      if (kind === "rt" && nights) w.nights = { min: nights[0], max: nights[1] };

      if (mode === "around" && ctx.pickedOut) {
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
        if (kind === "rt" && (retFrom || retTo)) {
          const last = store.bundle.days - 1;
          w.ret = {
            from: retFrom || idxToIso(Math.min(last, isoToIdx(w.out.from) + w.nights.min)),
            to: retTo || idxToIso(Math.min(last, isoToIdx(w.out.to) + w.nights.max)),
          };
        }
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
      countLine.className = pairs ? "bell-count ok" : "bell-count none";
      countLine.textContent = pairs
        ? `${pairs} ${what}${pairs > 1 ? "s" : ""} match right now`
        : `Nothing matches right now — we'll tell you the moment something opens.`;
    }

    async function commit(w, btnEl) {
      btnEl.disabled = true;
      const was = btnEl.textContent;
      btnEl.textContent = w ? "Saving…" : "Turning off…";
      note.textContent = "";
      try {
        const sub = await getSubscription({ create: true });
        const list = (await fetchWatches(sub).catch(() => []))
          .filter((x) => !(x.route === routeKey && x.kind === kind));
        if (w) list.push(w);
        await saveWatches(sub, list);
        setLabel(!!w);
        refreshAlertCount();
        note.textContent = w
          ? "Armed. We'll tell you the moment new space opens in your dates."
          : "Alerts off for this route.";
        announce(note.textContent);
        setTimeout(close, 1500);
      } catch (err) {
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

  const bits = routeBits(key);
  const totals = routeTotals(key);
  const container = el(`<div></div>`);
  const toolbar = el(`<div class="route-toolbar"></div>`);
  const body = el(`<div></div>`);
  mainEl.append(toolbar, container, body);

  let mask = 0, firstDraw = true;
  toolbar.append(cabinChips(totals.perCabin, (m) => { mask = m; drawCalendars(); }));
  if (routeHasNew(key)) {
    toolbar.append(el(`<span class="new-legend"><span class="new-dot" aria-hidden="true"></span>opened in the last 48h</span>`));
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

    // Year strip (in-page scroll shortcuts, not tabs → role=group)
    const strip = el(`<div class="year-strip" role="group" aria-label="Jump to month"></div>`);
    months.forEach((mo, mi) => {
      const c = countIn(mo);
      const now = new Date();
      const isCur = mo.y === now.getFullYear() && mo.m === now.getMonth();
      const btn = el(`<button type="button" class="ys-month${isCur ? " current" : ""}" aria-label="${fmtMonth.format(utcDate(mo.y, mo.m, 1))}: ${c} days with seats">
        <span class="ys-label">${mo.label}</span>
        <span class="ys-bars" aria-hidden="true"></span>
        <span class="ys-count">${c || "·"}</span>
      </button>`);
      const bars = $(".ys-bars", btn);
      // one bar per cabin present that month, height by its day count
      for (const [bit] of cabinLegend()) {
        if (!(bit & mask)) continue;
        let n = 0;
        for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, bits.length); i++)
          if (bits[i] & bit) n++;
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
      grid.append(monthCal(key, bits, mo, mask, t0));
    }
    body.append(grid);

    function countIn(mo) {
      let n = 0;
      for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, bits.length); i++)
        if (bits[i] & mask) n++;
      return n;
    }
  }
}

function daysInMonth(mo) { return mo.end - mo.start; }

function monthCal(routeKey, bits, mo, mask, t0) {
  const first = utcDate(mo.y, mo.m, 1);

  // Months with nothing to show collapse to a compact card — a full grid of
  // empty cells is noise when the answer is simply "no".
  let any = false, anyShown = false;
  for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, bits.length); i++) {
    if (bits[i]) { any = true; if (bits[i] & mask) { anyShown = true; break; } }
  }
  if (!anyShown) {
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
    const v = (idx >= 0 && idx < bits.length) ? bits[idx] : 0;
    const shown = v & mask;
    const isPast = idx < t0;
    if (!v || isPast) {
      grid.append(el(`<span class="day${isPast ? " past" : ""}"><span class="num">${dnum}</span><span class="stack"></span></span>`));
      continue;
    }
    monthDays += shown ? 1 : 0;
    const iso = isoOf(dayDate(idx));
    const fresh = isNew(routeKey, iso);
    // Lit day: lanes show the cabins passing the filter. Dim day (space only
    // in filtered-out cabins): lanes show what's there, grayed by CSS.
    const cell = el(`<button type="button" class="day has${shown ? "" : " dim"}${fresh ? " new" : ""}"
        aria-label="${esc(fmtDate.format(dayDate(idx)))}: ${esc(cabNames(v))} available${fresh ? ", newly opened" : ""}">
      <span class="num">${dnum}</span>
      ${stackHTML(shown || v)}
    </button>`);
    cell.addEventListener("click", () => openDayPanel(routeKey, idx));
    cell.addEventListener("mouseenter", (e) => showTip(e.currentTarget, idx, bits[idx]));
    cell.addEventListener("mouseleave", hideTip);
    cell.addEventListener("focus", (e) => showTip(e.currentTarget, idx, bits[idx]));
    cell.addEventListener("blur", hideTip);
    grid.append(cell);
  }
  $(".mc", box).textContent = `${monthDays}d`;
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

  const outBits = routeBits(key);
  const t0 = todayIndex();
  const legend = cabinLegend();
  const allMask = legend.reduce((m, [bit]) => m | bit, 0);

  const toolbar = el(`<div class="route-toolbar"></div>`);
  const container = el(`<div></div>`);
  const body = el(`<div></div>`);
  mainEl.append(toolbar, container, body);

  let mask = 0, firstDraw = true, chipsEl = null;

  /* Chip counts are ROUND-TRIPPABLE outbound days (recounted from the
     engine), not raw one-way availability. */
  function perCabinRT() {
    const rt = roundTripBits(key, revKey, allMask, nights[0], nights[1]);
    const per = new Map();
    for (let i = Math.max(0, t0); i < rt.length; i++) {
      const v = rt[i];
      if (!v) continue;
      for (const [bit] of legend) if (v & bit) per.set(bit, (per.get(bit) || 0) + 1);
    }
    return per;
  }

  function rebuildChips() {
    const fresh = cabinChips(perCabinRT(), (m) => { mask = m; drawCalendars(); },
      (count, label) => count
        ? `${count} outbound days with a same-cabin ${label} return`
        : `No ${label} round trips within ${nights[0]}–${nights[1]} nights`);
    if (chipsEl) chipsEl.replaceWith(fresh); else toolbar.prepend(fresh);
    chipsEl = fresh;
  }

  function buildNightsControl() {
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
      for (const b of wrap.querySelectorAll(".np")) {
        b.setAttribute("aria-pressed",
          Number(b.dataset.lo) === nights[0] && Number(b.dataset.hi) === nights[1] ? "true" : "false");
      }
      minIn.value = nights[0]; maxIn.value = nights[1];
    }
    function apply(lo, hi) {
      nights = [lo, hi];
      setNightsPref(lo, hi);
      const u = new URL(location.href);
      u.searchParams.set("nights", `${lo}-${hi}`);
      history.replaceState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
      sync();
      rebuildChips(); // recounts chips; chips' onChange redraws the calendars
    }
    const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));
    function fromInputs(which) {
      let lo = clamp(Math.round(Number(minIn.value)) || nights[0], 1, 60);
      let hi = clamp(Math.round(Number(maxIn.value)) || nights[1], 1, 60);
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

  toolbar.append(buildNightsControl());
  rebuildChips(); // synchronously sets mask + first drawCalendars()
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
    const rb = roundTripBits(key, revKey, mask, nights[0], nights[1]);
    // All-cabin recount alongside the masked one, so dim cells can honestly
    // distinguish "no same-cabin return in the window" from "a round trip
    // exists — your cabin filter is hiding it".
    const rbAll = mask === allMask ? rb : roundTripBits(key, revKey, allMask, nights[0], nights[1]);
    const months = next12Months();

    // Year strip: recounted from roundBits, like every number on this page.
    const strip = el(`<div class="year-strip" role="group" aria-label="Jump to month"></div>`);
    months.forEach((mo, mi) => {
      const c = countIn(mo);
      const now = new Date();
      const isCur = mo.y === now.getFullYear() && mo.m === now.getMonth();
      const btn = el(`<button type="button" class="ys-month${isCur ? " current" : ""}" aria-label="${fmtMonth.format(utcDate(mo.y, mo.m, 1))}: ${c} days with round trips">
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
    for (const mo of months) grid.append(monthCalTrip(mo, rb, rbAll));
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
     in filtered-out cabins says so — never "no return" when one is there. */
  function monthCalTrip(mo, rb, rbAll) {
    const first = utcDate(mo.y, mo.m, 1);
    let anyOut = false;
    for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, outBits.length); i++) {
      if (outBits[i]) { anyOut = true; break; }
    }
    if (!anyOut) {
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
      const aria = v
        ? `${dateLabel}: round trip available in ${esc(cabNames(v))}`
        : hiddenBits
          ? `${dateLabel}: round trip available in ${esc(cabNames(hiddenBits))} — hidden by your cabin filter`
          : `${dateLabel}: outbound only — no return within ${nights[0]}–${nights[1]} nights`;
      const cell = el(`<button type="button" class="day has${v ? "" : " dim"}" aria-label="${aria}">
        <span class="num">${dnum}</span>
        ${stackHTML(v || vOut)}
      </button>`);
      const tipNote = v ? null : hiddenBits
        ? `round trip open in ${cabNames(hiddenBits)} — hidden by your cabin filter`
        : "outbound only — no same-cabin return in window";
      cell.addEventListener("click", () => pickOutbound(idx));
      cell.addEventListener("mouseenter", (e) => showTip(e.currentTarget, idx, v, tipNote));
      cell.addEventListener("mouseleave", hideTip);
      cell.addEventListener("focus", (e) => showTip(e.currentTarget, idx, v, tipNote));
      cell.addEventListener("blur", hideTip);
      grid.append(cell);
    }
    $(".mc", box).textContent = `${monthDays}d`;
    return box;
  }

  /* Picking an outbound day is a history entry (Back = undo the pick). */
  function pickOutbound(idx) {
    const iso = isoOf(dayDate(idx));
    const u = new URL(location.href);
    u.searchParams.set("out", iso);
    u.searchParams.delete("ret");
    history.pushState(null, "", `${u.pathname}?${u.searchParams.toString()}`);
    openPairPanel(o, d, idx, nights, mask);
  }

  // A pinned outbound in the URL (?out=…) restores the pair panel; an
  // invalid or past date, or a day with no outbound space, degrades silently.
  const outIdx = tripDayIndex(params.get("out") || "");
  if (outIdx >= 0 && outBits[outIdx]) {
    const retP = params.get("ret") || "";
    openPairPanel(o, d, outIdx, nights, mask, tripDayIndex(retP) >= 0 ? retP : null);
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

function openDayPanel(routeKey, idx) {
  const [o, d] = routeKey.split("-");
  const iso = isoOf(dayDate(idx));
  const bits = routeBits(routeKey);
  panelReturnFocus = document.activeElement;
  panelCloseHook = null;

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
    <div class="dp-cabs">${legend.map(([bit, label]) => `
      <a class="dp-cab" data-bit="${bit}" target="_blank" rel="noopener noreferrer"
         href="${baBookingURL(o, d, iso, bit, null)}">
        <span class="swatch ${bitClass(bit)}" aria-hidden="true"></span>
        <span class="dp-cab-label">${esc(label)}</span>
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
function openPairPanel(o, d, idx, nights, mask, retIso = null) {
  const key = `${o}-${d}`, revKey = `${d}-${o}`;
  const outBitsAll = routeBits(key);
  const vOut = outBitsAll[idx];
  const [lo, hi] = nights;
  const outIso = isoOf(dayDate(idx));
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
      <p class="dp-lead">Search the outbound one way:</p>
      <div class="dp-cabs">${legend.map(([bit, label]) => `
        <a class="dp-cab" target="_blank" rel="noopener noreferrer"
           href="${baBookingURL(o, d, outIso, bit, null)}">
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
    rowsWrap.append(el(`<button type="button" class="pp-row" role="radio" aria-checked="false"
        tabindex="-1" data-iso="${r.iso}"
        aria-label="Return ${esc(fmtRet.format(dayDate(r.idx)))}, ${r.n} night${r.n === 1 ? "" : "s"}, ${esc(cabNames(r.bits))} open">
      <span class="pp-radio" aria-hidden="true"></span>
      <span class="pp-row-date">${esc(fmtRet.format(dayDate(r.idx)))}</span>
      <span class="pp-row-n">${r.n} night${r.n === 1 ? "" : "s"}</span>
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
        ctas.append(el(`<a class="pp-cta" target="_blank" rel="noopener noreferrer"
            href="${baBookingURL(o, d, outIso, bit, r.iso)}">
          <span class="swatch ${bitClass(bit)}" aria-hidden="true"></span>
          Search ${esc(label)} round trip
          <span class="pp-cta-go" aria-hidden="true">↗</span></a>`));
      }
    } else {
      ctas.append(el(`<p class="pp-none">No single cabin is open both ways on these dates — book two one-ways:</p>`));
    }
    $("#pp-oneways", panelEl).innerHTML =
      `${shared ? "or " : ""}book each leg one-way:
       <a href="${baBookingURL(o, d, outIso, 0, null)}" target="_blank" rel="noopener noreferrer">${o}→${d} ${esc(fmtRet.format(dayDate(idx)))} ↗</a> ·
       <a href="${baBookingURL(d, o, r.iso, 0, null)}" target="_blank" rel="noopener noreferrer">${d}→${o} ${esc(fmtRet.format(dayDate(r.idx)))} ↗</a>`;
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
  mainEl.append(el(`<div class="section-pad">
    <p class="crumbs"><a href="/">Search</a></p>
    <h1 class="page-title">Everywhere from ${esc(placeName(o))}</h1>
    <p class="page-sub">${dests.length} destinations with award seats in the next year.
      Bars show days with round trips per month (any cabin, ${NIGHTS_DEFAULT[0]}–${NIGHTS_DEFAULT[1]} nights).</p>
  </div>`));

  if (!dests.length) {
    mainEl.append(el(`<div class="empty-state">
      <div class="big">No destinations right now.</div>
      <p>No award seats from ${esc(placeName(o))} in the current data.</p></div>`));
    return;
  }

  // Counts are ROUND-TRIPPABLE days over the default window (any cabin): a day
  // counts only when its outbound has a valid same-cabin return. Destinations
  // with outbound space but zero such days keep their card, grayed and badged.
  const t0 = Math.max(0, todayIndex());
  const allMask = cabinLegend().reduce((m, [bit]) => m | bit, 0);
  const months = next12Months();
  const cards = dests.map((d) => {
    const key = `${o}-${d}`;
    const rt = roundTripBits(key, `${d}-${o}`, allMask, NIGHTS_DEFAULT[0], NIGHTS_DEFAULT[1]);
    let total = 0, union = 0;
    for (let i = t0; i < rt.length; i++) if (rt[i]) { total++; union |= rt[i]; }
    const counts = months.map((mo) => {
      let n = 0;
      for (let i = Math.max(mo.start, t0); i < Math.min(mo.end, rt.length); i++) if (rt[i]) n++;
      return n;
    });
    return { d, key, total, union, counts, out: routeTotals(key) };
  }).sort((a, b) => b.total - a.total || b.out.total - a.out.total);

  const grid = el(`<div class="dest-grid"></div>`);
  const max = Math.max(1, ...cards.flatMap((c) => c.counts));
  for (const c of cards) {
    const ow = c.total === 0; // outbound space only — nothing round-trippable
    const cabs = cabinLegend().filter(([bit]) => (ow ? c.out.union : c.union) & bit)
      .map(([bit, label]) => `<span class="swatch ${bitClass(bit)}" title="${esc(label)}"></span>`).join("");
    grid.append(el(`<a class="dest-card${ow ? " dc-ow" : ""}" href="/trip/${c.key}">
      <span class="dc-head"><span class="dc-code">${c.d}</span>
        <span class="dc-name">${esc(placeName(c.d))}</span>
        <span class="dc-country">${esc(placeCountry(c.d))}</span></span>
      <span class="dc-spark" aria-hidden="true">${c.counts.map((n) =>
        `<i style="height:${Math.max(2, Math.round((n / max) * 30))}px"></i>`).join("")}</span>
      <span class="dc-meta">${ow
        ? `<span class="dc-badge">outbound only — no return space seen</span>`
        : `<span>${c.total} days with round trips</span>`}<span class="dc-cabs">${cabs}</span></span>
    </a>`));
  }
  mainEl.append(grid);
}

/* ---------------- pages: my alerts ---------------- */

/* The header link doubles as the discovery path for alerts, so it carries the
   count. Kept in sync by refreshAlertCount() after any change. */
function refreshAlertCount() {
  const link = $("#alerts-link");
  if (!link) return;
  currentAlerts().then((data) => {
    const n = data ? data.watches.length : 0;
    link.hidden = false;
    $(".al-count", link).textContent = n ? String(n) : "";
    link.classList.toggle("on", n > 0);
    link.title = n ? `${n} route${n > 1 ? "s" : ""} you're watching` : "Alerts";
  });
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
      btn.disabled = false; btn.textContent = "Turn all off";
      status(body, String(err.message || err));
    }
  });

  // Proof it works, without waiting for real availability to open.
  $("#alerts-test", head).addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    btn.disabled = true; btn.textContent = "Sending…";
    try {
      const res = await fetch(`${PUSH_API}/test`, {
        method: "POST", headers: { "content-type": "application/json" },
        body: JSON.stringify({ endpoint: data.sub.endpoint }),
      });
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
