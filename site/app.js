/* build 6 */
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
        const wasRoute = current.page === "route";
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
  else if ((m = path.match(/^\/from\/([A-Z]{3})\/?$/i)))
    renderFrom(m[1].toUpperCase());
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
    else if (e.key === "Enter") { e.preventDefault(); if (items[active]) { close(); onPick(items[active]); } }
    else if (e.key === "Escape") close();
  });
}

/* ---------------- pages: home ---------------- */

let homeSel = { origin: null, dest: null };

function renderHome() {
  current.page = "home"; current.params = null;
  setTitle(null);
  if (!store.bundle) return; // static skeleton stays until data lands

  mainEl.innerHTML = "";
  const hero = el(`<section class="hero">
    <h1 class="hero-title">Find award seats<br>before they're gone.</h1>
    <p class="hero-sub">A live calendar of British Airways reward-flight availability on every route — built on free, open data.</p>
    <div class="search-card">
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

  function maybeGo() {
    if (homeSel.origin && homeSel.dest) {
      const key = `${homeSel.origin}-${homeSel.dest}`;
      navigate(store.bundle.routes[key] ? `/route/${key}` : `/route/${key}`);
    } else updateHint();
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

function buildHomeModules(mount) {
  mount.innerHTML = "";
  const modules = el(`<div class="modules"></div>`);
  const opened = recentlyOpened();
  if (opened.length) {
    const mod = el(`<section class="module"><h2><span class="dot" aria-hidden="true"></span>Recently opened</h2><div class="card-list"></div></section>`);
    const listEl = $(".card-list", mod);
    for (const g of opened) {
      const [o, d] = g.route.split("-");
      listEl.append(el(`<a class="route-card" href="/route/${g.route}">
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
      listEl.append(el(`<a class="route-card" href="/route/${key}">
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

function cabinChips(perCabin, onChange) {
  const legend = cabinLegend();
  const allMask = legend.reduce((m, [bit]) => m | bit, 0);
  let mask = getFilter() ?? allMask;
  mask &= allMask; if (!mask) mask = allMask;

  const wrap = el(`<div class="cabin-chips" role="group" aria-label="Filter by cabin"></div>`);
  for (const [bit, label] of legend) {
    const count = perCabin?.get(bit) || 0;
    const chip = el(`<button type="button" class="chip${count ? "" : " none"}" aria-pressed="${!!(mask & bit)}"
      title="${count ? `${count} days with ${esc(label)} seats` : `No ${esc(label)} seats on this route right now`}">
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

/* ---------------- pages: route ---------------- */

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
      <h1 class="route-title" aria-label="${o} to ${d}">${o} <span class="arrow" aria-hidden="true">→</span> ${d}</h1>
      <div class="head-actions">
        <a class="btn" href="/route/${d}-${o}" title="See the return direction">⇄ Return</a>
      </div>
    </div>
    <p class="route-cities">${esc(placeName(o))}${placeCountry(o) ? `, ${esc(placeCountry(o))}` : ""}
      <span class="via">to</span>
      ${esc(placeName(d))}${placeCountry(d) ? `, ${esc(placeCountry(d))}` : ""}</p>
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
  const legend = cabinLegend();
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
    const stack = legend
      .filter(([bit]) => v & bit)
      .map(([bit]) => `<i class="${(bit & mask) ? bitClass(bit) : ""} ${bit & mask ? "" : "off"}"></i>`)
      .join("");
    const fresh = isNew(routeKey, iso);
    const cell = el(`<button type="button" class="day has${shown ? "" : " dim"}${fresh ? " new" : ""}"
        aria-label="${esc(fmtDate.format(dayDate(idx)))}: ${esc(cabNames(v))} available${fresh ? ", newly opened" : ""}">
      <span class="num">${dnum}</span>
      <span class="stack" aria-hidden="true">${stack}</span>
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

function renderRouteSkeleton(o, d) {
  mainEl.innerHTML = "";
  mainEl.append(el(`<div class="route-head">
    <div class="route-title-row"><h1 class="route-title">${esc(o)} <span class="arrow">→</span> ${esc(d)}</h1></div>
    <div class="months" style="margin-top:34px">${Array.from({ length: 3 }, () => `
      <section class="month"><div class="sk-line" style="height:18px;width:60%;margin-bottom:12px"></div>
      <div class="grid">${Array.from({ length: 35 }, () => `<span class="day sk-cell"></span>`).join("")}</div></section>`).join("")}
    </div>
  </div>`));
}

/* ---------------- tooltip ---------------- */

let tipEl = null;
function showTip(anchor, idx, v) {
  hideTip();
  const legend = cabinLegend().filter(([bit]) => v & bit);
  tipEl = el(`<div class="tip" role="tooltip">
    <div class="t-date">${fmtDate.format(dayDate(idx))}</div>
    ${legend.map(([bit, label]) =>
      `<div class="t-cab"><span class="swatch ${bitClass(bit)}"></span>${esc(label)}</div>`).join("")}
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

async function openDayPanel(routeKey, idx) {
  const [o, d] = routeKey.split("-");
  const iso = isoOf(dayDate(idx));
  const route = store.bundle.routes[routeKey];
  const bits = routeBits(routeKey);
  panelReturnFocus = document.activeElement;

  const legend = cabinLegend().filter(([bit]) => bits[idx] & bit);
  panelEl.setAttribute("role", "dialog");
  panelEl.setAttribute("aria-modal", "true");
  panelEl.setAttribute("aria-labelledby", "dp-title");
  panelEl.innerHTML = "";
  // Return-leg candidates: dates on the reverse route (dest→origin) on or
  // after this outbound date. Used to optionally deep-link a round trip.
  const revKey = `${d}-${o}`;
  const revBits = store.bundle.routes[revKey] ? routeBits(revKey) : null;
  const returns = [];
  if (revBits) {
    for (let i = idx; i < revBits.length; i++) {
      if (revBits[i]) returns.push({ iso: isoOf(dayDate(i)), bits: revBits[i] });
    }
  }
  const returnOptions = returns.map((r) =>
    `<option value="${r.iso}">${esc(fmtRet.format(dayDate(dayIndexOf(r.iso))))} — ${esc(cabNames(r.bits))}</option>`
  ).join("");

  panelEl.append(el(`<div>
    <button class="dp-close" type="button" aria-label="Close">×</button>
    <p class="dp-date">${esc(fmtDate.format(dayDate(idx)))}</p>
    <p class="dp-route" id="dp-title">${o} <span style="color:var(--gold)" aria-hidden="true">→</span> ${d}</p>
    <p class="dp-lead">Award seats available — search British Airways to book:</p>
    <div class="dp-cabs">${legend.map(([bit, label]) => `
      <a class="dp-cab" data-bit="${bit}" target="_blank" rel="noopener noreferrer"
         href="${baBookingURL(o, d, iso, bit, null)}">
        <span class="swatch ${bitClass(bit)}" aria-hidden="true"></span>
        <span class="dp-cab-label">${esc(label)}</span>
        <span class="dp-cab-go">Search one way ↗</span>
      </a>`).join("")}
    </div>
    ${returns.length ? `<div class="dp-return">
      <label for="dp-return-date">Return leg (${d} → ${o})</label>
      <select id="dp-return-date">
        <option value="">One way only</option>
        ${returnOptions}
      </select>
    </div>` : ""}
    <div class="dp-flights" id="dp-flights"></div>
    <p class="dp-note">Links open BA's Avios redemption search for the whole city pair.
      Seen in data as of ${esc(freshLabel())}. Availability moves fast — verify before planning.</p>
  </div>`));
  panelEl.hidden = false; scrimEl.hidden = false;
  document.body.classList.add("modal-open");
  setInert(true);
  $(".dp-close", panelEl).addEventListener("click", closeDayPanel);
  panelEl.addEventListener("keydown", trapTab);

  // Wire the return picker: rewrite each cabin link between one-way and round
  // trip as the user picks a return date.
  const returnSelect = $("#dp-return-date", panelEl);
  if (returnSelect) {
    returnSelect.addEventListener("change", () => {
      const ret = returnSelect.value || null;
      for (const link of panelEl.querySelectorAll(".dp-cab")) {
        const bit = Number(link.dataset.bit);
        link.href = baBookingURL(o, d, iso, bit, ret);
        $(".dp-cab-go", link).textContent = ret ? "Search return ↗" : "Search one way ↗";
      }
    });
  }
  $(".dp-close", panelEl).focus();

  // Flight-level detail, only where the data says it exists (no 404 probing —
  // raw.githubusercontent caches 404s for five minutes).
  const month = iso.slice(0, 7);
  if (route.fm?.includes(month)) {
    const holder = $("#dp-flights", panelEl);
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
}

function closeDayPanel() {
  if (panelEl.hidden) return;
  panelEl.hidden = true; scrimEl.hidden = true;
  panelEl.removeEventListener("keydown", trapTab);
  document.body.classList.remove("modal-open");
  setInert(false);
  panelReturnFocus?.focus?.();
}

/* Make the page behind the modal inert (unfocusable + hidden from AT). */
function setInert(on) {
  for (const sel of [".topbar", "#banner", "#main", ".footer"]) {
    const node = $(sel);
    if (node) node.inert = on;
  }
}

/* Cycle Tab within the open panel so focus can't wander onto the scrim-covered
   page behind it. */
function trapTab(e) {
  if (e.key !== "Tab") return;
  const focusable = panelEl.querySelectorAll(
    'a[href], button:not([disabled]), input, [tabindex]:not([tabindex="-1"])');
  if (!focusable.length) return;
  const first = focusable[0], last = focusable[focusable.length - 1];
  if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
}

scrimEl.addEventListener("click", closeDayPanel);
document.addEventListener("keydown", (e) => { if (e.key === "Escape") { closeDayPanel(); hideTip(); } });

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
    <p class="page-sub">${dests.length} destinations with award seats in the next year. Bars show days with seats per month.</p>
  </div>`));

  if (!dests.length) {
    mainEl.append(el(`<div class="empty-state">
      <div class="big">No destinations right now.</div>
      <p>No award seats from ${esc(placeName(o))} in the current data.</p></div>`));
    return;
  }

  const cards = dests.map((d) => {
    const key = `${o}-${d}`;
    const totals = routeTotals(key);
    const counts = monthCounts("route", key);
    return { d, key, totals, counts };
  }).sort((a, b) => b.totals.total - a.totals.total);

  const grid = el(`<div class="dest-grid"></div>`);
  const max = Math.max(1, ...cards.flatMap((c) => c.counts));
  for (const c of cards) {
    const cabs = cabinLegend().filter(([bit]) => c.totals.union & bit)
      .map(([bit, label]) => `<span class="swatch ${bitClass(bit)}" title="${esc(label)}"></span>`).join("");
    grid.append(el(`<a class="dest-card" href="/route/${c.key}">
      <span class="dc-head"><span class="dc-code">${c.d}</span>
        <span class="dc-name">${esc(placeName(c.d))}</span>
        <span class="dc-country">${esc(placeCountry(c.d))}</span></span>
      <span class="dc-spark" aria-hidden="true">${c.counts.map((n) =>
        `<i style="height:${Math.max(2, Math.round((n / max) * 30))}px"></i>`).join("")}</span>
      <span class="dc-meta"><span>${c.totals.total} days</span><span class="dc-cabs">${cabs}</span></span>
    </a>`));
  }
  mainEl.append(grid);
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
