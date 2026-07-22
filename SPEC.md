# rewardflights.lucy.sh — architecture spec

Status: FINAL v1 (synthesized from design panel + measured snappiness study, 2026-07-12)

## System overview

```
github.com/intUnderflow/rewardflights          (source: scraper output)
        │  watched live by a long-running processor on the source host
        ▼
processor/ (Go, stdlib-only)
        │  deterministic transform; commit only on change
        ▼
github.com/intUnderflow/rewardflights.lucy.sh-data   (derived, no license; provenance-tagged)
        │  fetched client-side from raw.githubusercontent.com
        │  (verified: ACAO:*, gzip, max-age=300; ETag NOT CORS-readable)
        ▼
site/ (static, no build step) → Cloudflare Pages → rewardflights.lucy.sh
```

Architecture headline (empirically measured): today's entire calendar dataset
gzips to ~39KB with nibble-per-day encoding, so the v1 client loads ONE bundle
and every interaction — autocomplete, calendar, cabin filter, explore — is
in-memory, zero-network. Per-origin shards are emitted from day one so scaling
to 10x is a client flag, not a format migration.

## Derived data format (rewardflights.lucy.sh-data)

### Canonical serialization (all files)

- Go `json.MarshalIndent`-style: sorted object keys, 1-space indent, LF,
  single trailing newline, UTF-8. STOCK encoder output — no hand-rolled
  serializer (judge-flagged as the top determinism risk). Values that change
  land on their own line → meaningful git diffs (one route's day-change = a
  one-line diff); gzip eats the indentation.
- Byte-identical output for identical inputs: NO wall clock anywhere.
- Golden-file tests pin exact bytes; CI runs a determinism canary (processor
  runs twice; second run must report written=0 deleted=0).

### Provenance / freshness (every fetchable file)

- `"v"`: SOURCE repo commit SHA the generation was built from.
- `"t"`: SOURCE commit's committer timestamp (unix seconds). Semantically
  "data as of"; keeps regeneration byte-pure.
- `"schema"`: integer, currently 1. Bumped ONLY on breaking change; clients
  ignore unknown keys/fields (additive evolution is free). Unknown cabin bits
  render as "Other"; unknown place codes render as the raw code.
- Embedded in-band because CORS blocks reading ETag from raw.githubusercontent.

### Day encoding (measured: 39KB gz full dataset; 11% off binary floor)

Availability for route+airline = **nibble-per-day uppercase-hex string**:

- Global `epoch` (YYYY-MM-DD) = Jan 1 of the year of the earliest available
  date. Calendar-anchored → day-to-day regenerations only diff where data
  changed; one epoch shift per year rollover.
- Char i = day `epoch + i`. Bitmask: M(Economy)=1, W(PremiumEconomy)=2,
  C(Business)=4, F(First)=8; `0` = none. All strings uniform length =
  `days` (epoch → last available date inclusive).
- Bits are per-airline-defined via legends (below). Future airline with >4
  award buckets: its legend defines bits ≥16 and its strings use 2 hex chars
  per day via an additive per-airline `width` field (default 1). Existing
  airlines' strings never rewrite when another airline's legend grows.
- Defensive filter: dates earlier than the source commit date are dropped
  with a single aggregated WARN (protects against source pruning failures).

### Seat thresholds — optional `s` route-entry key (additive, schema stays 1)

Where the source's optional `flights[]` arrays carry per-cabin seat counts,
a route entry gains `"s"`: airline id → **2 uppercase hex chars (one byte)
per day**, same epoch/window as `a` (length exactly `2 × days`; day d =
`parseInt(s.slice(2d, 2d+2), 16)`). 2 bits per cabin: M=bits 0–1, W=2–3,
C=4–5, F=6–7. Monotone threshold code per cabin:
`0` = no sign of ≥2 seats (count unknown OR only 1 seen — deliberately
collapsed; copy must say "no sign of N seats", never "0 seats"), `1` = ≥2,
`2` = ≥3, `3` = ≥4. A party of N fits iff code ≥ N−1; N=1 uses `a` only.

- Per-cabin value = **MAX of seats across that day's flights** of the airline
  (a party must fit on ONE flight — never SUM); merge airlines client-side
  with per-cabin MAX, never OR/SUM.
- `a` is the presence authority: a cabin's code is nonzero only when its bit
  is set in `a`; contradictions resolve to the `a` bit with a WARN.
- Emitted per route+airline ONLY when at least one day has flight detail, so
  0% flights coverage produces byte-identical output (the dormant phase).
  Absent key = counts unknown → surfaces fall back to `a` presence with an
  honest note, never a silently empty view.
- Unrelated to the reserved per-airline `width` mechanism (cabin-legend
  growth of `a` only); never widen `a` — decoders skip width ≠ 1 airlines.
- changes/recent.json does NOT report seat-threshold transitions (v1); the
  previous bundle's `s` keys are tolerated and ignored by the differ.

### Files

```
manifest.json                        ~200 B stable entrypoint / version poll
availability.json                    whole-dataset bundle (v1 client's ONE fetch)
origins/<ORIGIN>.json                per-origin shards (same shape, filtered)
flights/<ORIG>/<DEST>/<YYYY-MM>.json per-flight detail, all airlines merged
                                     (origin-nested: GitHub UI truncates
                                     1000-entry dir listings at 10x scale)
changes/recent.json                  rolling opened/closed feed (see below)
places.json                          standalone copy of the places table
README.md, FORMAT.md
```

`manifest.json`:
```json
{"bundle":"availability.json","changes":"changes/recent.json",
 "counts":{"airlines":1,"places":173,"routeDates":72116,"routes":345},
 "epoch":"2026-01-01","source":{…},"mode":"bundle","schema":1,
 "t":1770000000,"v":"<source sha>"}
```

`availability.json`:
```json
{"airlines":{"BA":{"cabins":{"1":"Economy","2":"Premium Economy",
                             "4":"Business","8":"First"},
                   "name":"British Airways","slug":"british-airways"}},
 "days":517,"epoch":"2026-01-01",
 "source":{"repo":"https://github.com/intUnderflow/rewardflights",
           "note":"Derived from …; provided as-is, no warranty."},
 "places":{"LON":{"country":"United Kingdom","name":"London",
                  "search":["Heathrow","LHR","Gatwick","LGW","City","LCY","Stansted","STN","Luton","LTN"]},…},
 "routes":{"LON-TYO":{"a":{"BA":"000…680C…"},"fm":["2026-10"]}},
 "schema":1,"t":1770000000,"v":"…"}
```
- Route entry: lowercase keys = metadata, uppercase = (never used; airline ids
  live under `"a"`). `"a"` maps airline id → nibble string. `"fm"` lists
  route-months that have flight-detail files — clients never probe/404
  (raw.githubusercontent caches 404s for 5 min). `"fm"` omitted when empty.
  `"s"` (optional) maps airline id → seat-threshold string (see above);
  omitted when the route+airline has no flight detail.
- Airline ids come from an APPEND-ONLY registry in processor assets
  (slug → {id, name, cabins}); once assigned an id is frozen forever;
  a later colliding airline gets its slug as id. (Judge-flagged: retroactive
  id renames would break every path/key.)
- `places` embedded (cold load = ONE fetch) — only codes present in current
  data; curated table lives in processor assets; unmapped code → emit
  `{"name":"<CODE>"}` + greppable WARN (route churn must not need redeploys).
  Multi-airport metros carry `search` aliases (airport names + IATA codes)
  for autocomplete.
- `origins/LON.json`: identical shape, `routes` filtered to origin LON,
  `places` filtered to codes referenced by those routes.

`flights/<ORIG>/<DEST>/<YYYY-MM>.json` (when source grows `flights` arrays):
```json
{"days":{"15":{"BA":[{"arr":"08:55","car":["BA"],"dep":"13:00",
   "fn":["BA0007"],"peak":"off-peak","rfs":true,
   "seats":{"C":1,"W":6},"via":[]}]}},
 "route":"LON-TYO","schema":1,"t":…,"v":"…"}
```
- Day keys zero-padded 2-digit (lexical = chronological). All airlines merged
  per route-month (one fetch per date-click even on multi-airline routes).
- Calendar-layer files never grow when detail ships. Target ≤15KB gz.

`changes/recent.json` — the ONLY state-dependent output (documented as such):
```json
{"entries":[{"c":"C","d":"2026-10-15","k":"opened","r":"LON-TYO",
             "al":"BA","t":1770000000}],"schema":1,"t":…,"v":"…"}
```
- Processor diffs previous availability.json (already in -out) against new:
  route+airline+date granularity, kinds opened/closed/changed (cabin set).
  Roll-off exclusions: dates that merely passed are NOT "closed".
- Append newest-first, trim to 1000 entries. Deterministic given
  (source, previous derived state); canary-safe (re-run diffs nothing).
- Powers the "recently opened" home module + future alerts.

### Size budgets (processor-enforced hard failure)
- availability.json ≤ 300KB gz (beyond → future flip to `mode:"shard"`)
- every other file ≤ 50KB gz

### Safety rails
- 50%-shrink guardrail: if new total routeDates < 50% of previous manifest's,
  hard-fail before any write/delete (`-force` overrides). Protects against a
  truncated source checkout mass-deleting the derived tree.
- Managed-path ownership: processor owns manifest.json, availability.json,
  places.json, FORMAT.md, origins/**, flights/**, changes/**; write-if-
  different, delete stale, NEVER touches README/.git.
- Malformed source entries: WARN + skip, never abort the run; hard fail only
  on structural problems (unreadable roots, zero data, budget breach,
  guardrail).

## Processor CLI

`processor -src <rewardflights checkout> -out <data repo checkout>
 [-source-sha SHA] [-source-time UNIX] [-force]`

Missing sha/time → read from `git -C src log -1`. Machine-parseable summary:
`SUMMARY routes=… routeDates=… origins=… places=… warnings=… written=… deleted=… unchanged=… bundleGzBytes=…`

## Website (./site, no build step)

- SPA: index.html + one JS + one CSS (critical CSS inlined), History-API
  routing (`/`, `/route/LON-TYO`, `/from/LON`) with Cloudflare Pages SPA
  fallback via `_redirects` (`/* /index.html 200`).
- Cold path: preconnect + `preload as=fetch` availability.json (branch-head
  URL); skeleton calendar from pure date math (fixed geometry, zero CLS).
- Repeat path: localStorage snapshot (`avail:v1` key incl. schema) renders
  REAL data first frame; network refresh in parallel; embedded `v` compare;
  subtle "updated" pulse + changed-cell highlight. Snapshot read/write fully
  try/catch'd (private mode, quota, corruption → cold path).
- Freshness: manifest.json (~200B) polled every 5 min while visible + on
  visibilitychange. New `v` → refetch bundle with `?v=<new>` (query-string
  cache-bust — raw.githubusercontent caches per-full-URL). Always render
  "availability as of <t> — verify with the airline before booking".
  Optional liveness enhancement: GitHub commits API (CORS-enabled) for source
  repo last-commit time; hide gracefully on rate limit. (Judge-flagged: a
  dead scraper must not masquerade as a quiet market.)
- Client contract: bundle is authoritative over manifest counts; unknown
  airline id → render raw id; unknown cabin bit → "Other"; unknown place →
  raw code; `fm`-listed month that 404s → "detail pending", retry, never an
  error state.
- All data access through ONE promise-map-deduped fetch layer; speculative
  fetches `priority:'low'`, capped 4–6, disabled on saveData/2g; failed
  speculation evicted from the map (never poisons user path).
- In-memory features: autocomplete (names + search aliases), route calendar
  w/ cabin filters, direction swap, explore ("where from LON in F in
  October"), "recently opened" module from changes feed.
- Failure: 8s timeout, ×2 retry w/ jittered backoff, amber stale banner when
  serving snapshot only; never white-screen.
- Provenance/attribution to the source repo shown in the footer.

## Automation (constantly-running watcher)

`processor -watch -push -src <...> -out <...> -token-cmd '<...>'`

- Polls the local source checkout's HEAD (`-interval`, default 2s). Because the
  source is produced on the same host, watching locally is instant — no webhook.
- On a new source commit: fetch+reset the out repo to origin/main (guarantees a
  fast-forward push), regenerate, and if anything changed, commit
  `data: source <short-sha>` and push using a token from `-token-cmd`.
- Never exits on a transient error (logs and retries next tick); the service
  supervisor restarts the process itself if it dies.
- Idempotent: regenerating unchanged data stages nothing, so no empty commits.
- Host-specific service config (launchd/systemd, token command) lives on the
  host, not in the repo.

## Round trips (/trip/ — the default surface, added 2026-07-12)

Round trip is the primary user goal; the directional data supports it entirely
client-side. `/trip/ORIG-DEST` (segmented control navigates to/from the one-way
`/route/` view):

- Engine: `roundTripBits(outKey, retKey, mask, minNights, maxNights)` — per
  outbound day D, OR of `ret[R] & (out[D] & mask)` over R in
  [D+minNights, D+maxNights] (same-cabin-both-ways by construction; minNights
  ≥ 1; missing reverse route → zeros). Cached per (route, mask, window).
- Calendar: day lit iff a round trip is completable (stack = round-trippable
  cabins); outbound-only days render dim but clickable with an honest
  explanation; chips / year strip / month counts all recount from roundBits.
- Trip length: presets Weekend 2–4 · 1 week 5–9 · 2 weeks 10–16 · Flexible
  1–30 (default) + custom min/max; pref in sessionStorage, URL wins.
- URL state: `?nights=MIN-MAX&out=YYYY-MM-DD&ret=YYYY-MM-DD`; pushState for
  picks (Back = undo), replaceState for filter tweaks; invalid params degrade.
- Pair-picker panel: radio-group of valid returns (nights + 4-lane stack),
  sorted shares-a-selected-cabin first; sticky summary; ONE CTA per cabin in
  `outBits & retBits & mask` (an empty-BA-result link is impossible by
  construction); per-leg one-way fallbacks; filter-hidden round trips get an
  honest "hidden by your cabin filter" note + "Show all cabins". Mobile:
  full-screen 1 Out · 2 Back · 3 Book stepper.
- Seat-stack is a fixed 4-lane gauge everywhere (absent cabin = faint track),
  restoring position-encoding for CVD and mobile legibility. First has its own
  hue (`--cab-f` copper-rose, CVD-validated); `--gold` is brand-only.
- Home search is round-trip-first (segmented + nights presets); explore
  (`/from/`) counts round-trippable days (1–30 window) with "outbound only"
  badges.

## Seat alerts

Free, instant Web Push when award space opens on a route someone is watching.
Full design: [ALERTS-SPEC.md](ALERTS-SPEC.md). In brief:

- An alert is a **watch**: route + direction (round trip / one way) + cabins +
  *when the person can actually travel* (outbound window, return window, trip
  length). Unbounded windows mean "any time" — the common case, and the default.
- A round trip is only reported when **both legs** have space in the **same
  cabin** inside the trip-length window. The unit of news is a (outbound,
  return) PAIR, not a day.
- **Pair-newness reduces to leg-gains**: a pair is newly bookable iff it is
  bookable now AND at least one of its two legs gained that cabin this cycle.
  So detection enumerates the handful of changed leg-days and expands each into
  its window — never the pair space. This is what makes per-subscriber date
  constraints affordable (~30µs for 1k subscribers × 20 watches).
  *The pre-2026-07 engine only looked at outbound gains and therefore silently
  dropped every round trip created by a **return**-leg opening — measured at
  41% of all newly-bookable round trips over an 18.8h replay of real data.*
- Anti-spam state is **global, not per-subscriber**: flapping is a property of
  the data, not the person. Cooldown is derived from an openedAt/closedAt ledger
  sized by events in the last 3h. Batching is per device (≤1 push/hour, ≤24/day).
- The site shows a **live match count** for a watch (computed in-browser from the
  in-memory dataset, zero network), because a well-set date window can be
  legitimately silent for weeks and would otherwise look broken.
- Delivery: RFC 8291 + VAPID, sent from the watcher. Subscriptions live on the
  same host (no third-party store); we keep no account, email, or IP — only a
  revocable push endpoint.

## Licensing
Code (processor + site) is CC BY-NC-SA 4.0. The derived data carries no license
of its own; each file embeds a `source` provenance block naming the source repo,
with a no-warranty note. Bundled fonts remain under the SIL OFL.

## Deliberately deferred (format already accommodates)
- `mode:"shard"` client path + SHA-pinned shard URLs (bundle > 300KB gz)
- Month×cabin summary bundle for shard-era autocomplete/explore
- Service Worker (localStorage SWR delivers ~90% of the value now)
- Fallback data origin (second Pages project serving the data repo)

## Freshness: 30-second bucket tags ("design-time agreement")

raw.githubusercontent's CDN caches every URL — 200s AND 404s — for ~300s, so
any mutable URL is up to 5 minutes stale and cache-busting query strings are
ignored at the edge (verified empirically; so is the GitHub API's
"304s-don't-count" rate-limit exemption, which no longer holds, and jsDelivr's
purge, which refills stale from its own branch-resolution cache). The floor
for free-at-scale polling of a mutable name is therefore ~5 minutes.

The protocol removes mutable names from the hot path. Time is chopped into
30s buckets (bucket = unix/30); the processor tags the data repo
`t-<bucket>` at each boundary whose HEAD moved (processor/tagger.go), and
clients poll the JUST-CLOSED bucket's tag URL ~10s after its close:

- A 200 is fresh by construction — nobody could request the URL before the
  tag existed — and immutable, so the CDN caches it correctly forever.
- A 404 is permanently true — the bucket is over — so a cached 404 is just
  the right answer served cheaply.

Client and publisher never coordinate at runtime; they agree at design time
on names computed from the clock. Typical commit-to-painted: ~30s (worst
~60s), vs ~5 minutes for the fallback manifest poll, which remains
underneath as the floor and the cold-start path.

Soundness invariants (site/app.js freshness section + tagger.go):
1. Clients never trust the device clock: bucket arithmetic runs on server
   time from Date headers (synced on every getJSON response), advanced by
   performance.now(), with a hard fire-time guard — polling a bucket EARLY
   would poison edges with cached 404s right as the tag lands.
2. The tagger is PROMPT OR NEVER: a tag is pushed within 5s of the boundary
   or not at all (a late tag would race the pollers).
3. Tags point at HEAD, so a missed/poisoned/skipped bucket is fully
   recovered by the next successful one; adoption is MONOTONIC on source
   time t (the stale fallback can never downgrade a bucket-fresh client).
4. Tags are rendezvous points, not history: pruned after ~10 minutes.
5. Catch-up probes backward over closed buckets (newest-first, first 200
   wins) at boot and on tab return — closed buckets are frozen, so these
   can never poison anything.

Adversarial note: the naming scheme is public, so anyone can pre-fetch
future bucket URLs to poison edges deliberately. The blast radius of a
perfect sustained attack is degradation to the fallback's ~5-minute
latency — never wrongness, never loss.
