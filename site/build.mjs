// The Cloudflare Pages build: minify (exactly what the old one-liner did),
// then prerender one indexable HTML page per route, plus sitemap.xml.
//
// Run from site/: `node build.mjs`. Everything is written in place — Pages
// serves the mutated build directory, so the repo itself stays build-free
// and `mktestsite.sh` exercises the same output.
//
// Why prerender at all: the SPA serves one identical shell for every URL, so
// the site is invisible to search engines. Neither competitor exposes
// route-level pages a crawler can reach (one renders results with
// client-side JS, the other sits behind a login), so ~600 long-tail queries
// like "avios availability london tokyo" are ownable for the cost of this
// script. Each page is the REAL shell (index.html is the template) with a
// unique head and a server-rendered summary inside <main>; the SPA boots on
// top and replaces the summary with the live calendar. No JavaScript, no
// problem: the summary stands alone, stamped with its snapshot date.
//
// The data baked here goes stale between deploys by design — the head/copy
// are stable, and a daily deploy-hook rebuild (pinged by the watcher)
// bounds the staleness of the numbers. A data-fetch failure FAILS the build:
// Pages then keeps the previous deploy, which still has every route page —
// deploying without them would silently de-index the site.

import { execSync } from "node:child_process";
import { readFileSync, writeFileSync, mkdirSync } from "node:fs";

const DATA = process.env.DATA_BASE || "https://raw.githubusercontent.com/intUnderflow/rewardflights.lucy.sh-data/main";
const ORIGIN = "https://rewardflights.lucy.sh";
const CABINS = [[1, "M", "Economy"], [2, "W", "Premium Economy"], [4, "C", "Business"], [8, "F", "First"]];

// --- 1. minify, exactly as the previous build command did -----------------
for (const f of ["app.js", "style.css", "sw.js", "assets/world-1.js"]) {
  execSync(`npx esbuild ${f} --minify --charset=utf8 --allow-overwrite --outfile=${f}`, { stdio: "inherit" });
}

// --- 2. data ---------------------------------------------------------------
async function getJSON(path, { optional = false } = {}) {
  const res = await fetch(`${DATA}/${path}`);
  if (!res.ok) {
    if (optional) return null;
    throw new Error(`${path}: HTTP ${res.status}`);
  }
  return res.json();
}
const bundle = await getJSON("availability.json");
const stats = await getJSON("stats.json", { optional: true });
const asOf = new Date(bundle.t * 1000).toISOString().slice(0, 10);
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

// Merged presence bits per day for one route (width-1 airlines only —
// mirrors the site's decoder).
function mergedBits(route) {
  const out = new Uint8Array(longest(route));
  for (const [al, str] of Object.entries(route.a)) {
    if ((bundle.airlines[al]?.width ?? 1) !== 1) continue;
    for (let i = 0; i < str.length; i++) {
      const v = parseInt(str[i], 16);
      if (v) out[i] |= v;
    }
  }
  return out;
}
function longest(route) {
  return Math.max(0, ...Object.values(route.a).map((s) => s.length));
}
const epochMs = Date.UTC(...bundle.epoch.split("-").map((n, i) => (i === 1 ? n - 1 : +n)));
const todayIdx = Math.floor((Date.now() - epochMs) / 86400000);
const isoOf = (i) => new Date(epochMs + i * 86400000).toISOString().slice(0, 10);
const MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"];

// One honest climate sentence per cabin, or nothing (absence of history is
// not evidence of absence of seats — say nothing rather than guess).
function climateLine(key, letter, label) {
  const g = stats?.routes?.[key]?.[letter];
  if (!g || !g.w) return "";
  const rate = g.w >= 1 ? `about ${Math.round(g.w)} time${Math.round(g.w) === 1 ? "" : "s"} a week`
    : `about ${Math.max(1, Math.round(g.w * 4))} time${Math.round(g.w * 4) === 1 ? "" : "s"} a month`;
  let life = "";
  if (g.d >= 5 && Array.isArray(g.s)) {
    const total = g.s.reduce((a, b) => a + b, 0);
    let acc = 0, mi = 0;
    for (; mi < g.s.length; mi++) { acc += g.s[mi]; if (acc * 2 >= total) break; }
    life = [", and seats are usually snapped up within an hour",
      ", and seats usually last 1–6 hours",
      ", and seats usually last under a day",
      ", and seats usually last 1–3 days",
      ""][mi] || "";
  }
  return `<li>${esc(label)} award seats have opened ${rate} on this route${life}.</li>`;
}
function bestMonths(key) {
  const byMonth = new Array(12).fill(0);
  for (const g of Object.values(stats?.routes?.[key] || {})) {
    (g.m || []).forEach((n, i) => { byMonth[i] += n; });
  }
  const total = byMonth.reduce((a, b) => a + b, 0);
  if (total < 20) return "";
  const top = byMonth.map((n, i) => [n, i]).sort((a, b) => b[0] - a[0]).slice(0, 2)
    .filter(([n]) => n > total / 12).map(([, i]) => MONTHS[i]);
  return top.length ? `<p>Most award-seat openings so far have been for travel in ${top.join(" and ")}.</p>` : "";
}

// --- 3. the template -------------------------------------------------------
const template = readFileSync("index.html", "utf8");
if (!template.includes('<main id="main"')) throw new Error("template drift: no <main id=\"main\">");

function page({ path, title, desc, body }) {
  let html = template;
  html = html.replace(/<title>[^<]*<\/title>/, `<title>${esc(title)}</title>`);
  html = html.replace(/(<meta name="description" content=")[^"]*(">)/, `$1${esc(desc)}$2`);
  // Per-page canonical + social card; the template's own og: block (home's)
  // is replaced wholesale so no page carries two.
  html = html.replace(/<link rel="canonical"[^>]*>\n?/, "");
  html = html.replace(/<meta (?:property="og:|name="twitter:)[^>]*>\n?/g, "");
  const head = [
    `<link rel="canonical" href="${ORIGIN}${path}">`,
    `<meta property="og:type" content="website">`,
    `<meta property="og:site_name" content="Reward Flights">`,
    `<meta property="og:title" content="${esc(title)}">`,
    `<meta property="og:description" content="${esc(desc)}">`,
    `<meta property="og:url" content="${ORIGIN}${path}">`,
    `<meta property="og:image" content="${ORIGIN}/assets/og.png">`,
    `<meta property="og:image:width" content="1200">`,
    `<meta property="og:image:height" content="630">`,
    `<meta name="twitter:card" content="summary_large_image">`,
  ].join("\n");
  html = html.replace("</head>", head + "\n</head>");
  html = html.replace(/(<main id="main"[^>]*>)/, `$1\n${body}`);
  return html;
}

// --- 4. route pages --------------------------------------------------------
const placeName = (c) => bundle.places?.[c]?.name || c;
mkdirSync("route", { recursive: true });
const keys = Object.keys(bundle.routes).sort();
let built = 0;
for (const key of keys) {
  const [o, d] = key.split("-");
  const route = bundle.routes[key];
  const bits = mergedBits(route);
  const alNames = Object.keys(route.a).filter((al) => (bundle.airlines[al]?.width ?? 1) === 1)
    .map((al) => bundle.airlines[al]?.name || al).sort();
  const rows = [];
  let totalDays = 0;
  for (const [bit, letter, label] of CABINS) {
    let days = 0, next = -1;
    for (let i = Math.max(0, todayIdx); i < bits.length; i++) {
      if (bits[i] & bit) { days++; if (next < 0) next = i; }
    }
    totalDays = Math.max(totalDays, days);
    if (days) rows.push(`<tr><td>${esc(label)}</td><td>${days} day${days === 1 ? "" : "s"}</td><td>${isoOf(next)}</td></tr>`);
  }
  let anyDays = 0;
  for (let i = Math.max(0, todayIdx); i < bits.length; i++) if (bits[i]) anyDays++;
  const cityO = placeName(o), cityD = placeName(d);
  const airlines = alNames.join(" and ");
  const climate = CABINS.map(([, l, n]) => climateLine(key, l, n)).filter(Boolean).join("\n");
  const hasReverse = !!bundle.routes[`${d}-${o}`];

  const body = `<section class="prerender section-pad">
  <p class="crumbs"><a href="/">Search</a> · <a href="/from/${o}">Everywhere from ${esc(cityO)}</a></p>
  <h1>${esc(cityO)} → ${esc(cityD)} award seat availability</h1>
  <p>A free, live calendar of ${esc(airlines)} reward-flight (Avios) seat availability from
  ${esc(cityO)} (${o}) to ${esc(cityD)} (${d}) — every bookable date in the next year, updated
  within seconds of the airline's own data, with free instant alerts when new seats open.</p>
  ${anyDays
    ? `<p><strong>${anyDays} date${anyDays === 1 ? " has" : "s have"} award seats right now</strong> (snapshot from ${asOf}; the live calendar below updates continuously).</p>
  <table><thead><tr><th>Cabin</th><th>Dates with seats</th><th>Next available</th></tr></thead>
  <tbody>${rows.join("")}</tbody></table>`
    : `<p><strong>No award seats are open on this route right now</strong> (snapshot from ${asOf}). That changes without notice — set a free alert and we'll tell you the moment seats appear.</p>`}
  ${climate ? `<ul>${climate}</ul>` : ""}
  ${bestMonths(key)}
  <p>${hasReverse ? `<a href="/trip/${key}">Plan a ${esc(cityO)} ⇄ ${esc(cityD)} round trip</a> · ` : ""}${hasReverse ? `<a href="/route/${d}-${o}">${esc(cityD)} → ${esc(cityO)} (return direction)</a> · ` : ""}<a href="/from/${o}">everywhere from ${esc(cityO)}</a></p>
  <noscript><p>This summary works without JavaScript; the interactive calendar and alerts need it.</p></noscript>
</section>
`;
  const title = `${cityO} to ${cityD} Avios award seats (${key}) — Reward Flights`;
  const desc = anyDays
    ? `Free live calendar of ${airlines} award (Avios) seat availability from ${cityO} to ${cityD}. ${anyDays} dates bookable right now. Free instant alerts when seats open.`
    : `Free live calendar of ${airlines} award (Avios) seat availability from ${cityO} to ${cityD}, with free instant alerts the moment seats open.`;
  writeFileSync(`route/${key}.html`, page({ path: `/route/${key}`, title, desc, body }));
  built++;
}

// --- 5. sitemap ------------------------------------------------------------
const urls = [`${ORIGIN}/`, ...keys.map((k) => `${ORIGIN}/route/${k}`)];
writeFileSync("sitemap.xml",
  `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n` +
  urls.map((u) => `<url><loc>${u}</loc><lastmod>${asOf}</lastmod></url>`).join("\n") +
  `\n</urlset>\n`);

console.log(`prerendered ${built} route pages + sitemap (data as of ${asOf})`);
