<!-- Design spec for date-constrained alert watches. Authoritative for the
implementation in processor/internal/{alertstore,alertapi,alerts} and the alert
UI in site/app.js. Owner decisions (§"Owner decisions flagged") are resolved as:
  1. EC-3 horizon rule  -> ADOPT the spec's pick (bounded watches get frontier
     alerts; unbounded keep today's suppression). This is what makes "tell me
     when BA loads next October" work at all.
  2. Notification tag   -> ADOPT stacking (unique tag per send). With the
     1-push/hour/device cap, collapsing would silently destroy unread dates.
  3. Cooldown 3h / Batch 1h, batched PER DEVICE (not per topic).
  4. The ledger redesign (state sized by events-in-3h, not by the dataset) ships
     with this work; it fixes an existing unbounded-growth bug.
-->

# DESIGN SPEC — Date-constrained alert watches

**Status:** proposed, v1. Replaces the `topics` model in `alertstore` / `alerts` / `alertapi` and the bell + `/alerts` UI in `site/app.js`. `webpush` and `sw.js` are untouched.

**Files read:** `SPEC.md`, `processor/internal/alerts/{alerts,bundle}.go`, `processor/internal/alertstore/alertstore.go`, `processor/internal/alertapi/alertapi.go`, `processor/watch.go`, `site/{app.js,sw.js}`, live `availability.json` (345 routes, 518 days, epoch 2026-01-01, uniform nibble length, 344/345 routes have a reverse).

---

## 0. The three ideas the whole design rests on

1. **A round-trip alert is about PAIRS, not days.** The unit of news is "(outbound D, return R) is now bookable in cabin c". Diffing days misses the crux case; diffing pairs is exact.

2. **Pair-newness reduces to leg-gains.** A pair `(D,R,c)` is newly satisfiable iff it is satisfiable now AND at least one of its two legs *gained* cabin `c` this cycle. Proof: `newSat ∧ ¬prevSat = (outNew[D]&c ∧ retNew[R]&c) ∧ (¬prevOut[D]&c ∨ ¬prevRet[R]&c)`. So we never enumerate pairs — we enumerate the handful of changed leg-days and expand each into its window. This is what makes 1k×20 watches cheap.

3. **The availability transition is global; the notification decision is per-subscriber — and so is the cooldown, but it can be *computed* from global state.** Flap suppression is a property of the data (a leg blinking), not of the person. I keep **zero per-subscriber cooldown state**. The global state is two maps sized by *events in the last 3 hours*, not by the dataset — which makes today's state file ~50× **smaller**, not bigger (see §4).

---

## 1. Data model

### 1.1 The Watch

A subscription is `{endpoint, p256dh, auth, watches: [Watch]}`. A Watch is one thing a person wants.

```jsonc
{
  "endpoint": "https://web.push.apple.com/QF…",
  "p256dh": "BF…",
  "auth": "k3…",
  "watches": [
    {
      "id": "a91c4f0e",                              // server-derived, content hash
      "route": "LON-TYO",
      "kind": "rt",                                  // "rt" | "ow"
      "cabins": ["C", "F"],                          // non-empty subset of M W C F
      "out":    {"from": "2026-10-01", "to": "2026-10-20"},   // optional
      "ret":    {"from": "2026-10-10", "to": "2026-10-31"},   // optional, rt only
      "nights": {"min": 7, "max": 14},               // optional, rt only
      "createdAt": 1770000000,                       // server-set
      "lastFiredAt": 1770461234                      // server-set; 0 = never fired
    },
    {
      "id": "3d70b2aa",
      "route": "NYC-LON",
      "kind": "ow",
      "cabins": ["M"],
      "createdAt": 1769900000
    }
  ]
}
```

**Optionality and defaults — chosen so the common user never touches a date field:**

| field | required | default when omitted | meaning of the default |
|---|---|---|---|
| `route` | yes | — | `^[A-Z]{3}-[A-Z]{3}$` |
| `kind` | yes | — | `rt` \| `ow` |
| `cabins` | yes | — | non-empty, deduped, sorted `M<W<C<F` |
| `out` | **no** | unbounded → `[today, +∞)` | "any time" |
| `ret` | **no** (rt only) | derived: `[out.from + nights.min, out.to + nights.max]`, unbounded if `out` is | "wherever the nights window lands me" |
| `nights` | **no** (rt only) | `{min: 1, max: 30}` | matches `NIGHTS_DEFAULT` in app.js and `Window=30` today |
| `minSeats` | **no** | `0` (≡ 1 passenger) | "we travel as a party of N": fire only on evidence of ≥ N seats together, same cabin, ONE flight, BOTH legs. Valid stored values: `0` or `2..4` (`4` means "4+", the top code the seats layer encodes). An explicit `1` → 400 — the zero value is the only spelling of "no constraint", so ids stay content-stable |

`ret`/`nights` on an `ow` watch → 400. `out.to` unbounded is expressed by omitting the key entirely (not by a sentinel date).

**`id` is `sha256(route|kind|cabins|out|ret|nights[|L<leadDays>][|S<minSeats>])[:8]` (hex).** Content-derived, so re-saving the same watch is idempotent, duplicates collapse for free, and the client can compute it for optimistic UI. `leadDays` and `minSeats` fold in **conditionally** (only when `> 0` / `≥ 2`, separator included in the fold) so a watch without them hashes byte-identically to the original formula — every pre-feature stored id, and everything keyed off ids (`carryHistory`, `MarkFired`, pending items, the client's `rf:seen:v1` baselines), survives each field's introduction. Pinned by `TestMinSeatsIDStability`. Editing a watch (including its party size) changes its id — fine, because `Upsert` replaces the whole list anyway; `createdAt`/`lastFiredAt` are carried over for ids that already exist. *Runner-up: client-supplied random ids — allows meaningless duplicates and needs a separate dedupe pass.*

**Why structured objects, not extended topic strings** (`rf_LON-TYO_rt_CF_o20261001-20261020_n7-14` would have kept the store schema unchanged): the grammar becomes unvalidatable, unextendable, and the on-disk store stops being readable by a human at 3am. *Runner-up in one line: extended topic strings — trivially compatible, permanently unpleasant.*

### 1.2 On-disk (alertstore, schema 2)

```json
{"schema": 2,
 "subs": [{"endpoint":"…","p256dh":"…","auth":"…","watches":[ … ]}]}
```

`topics` disappears from disk. It survives only as an **API projection** (§2, §6). One-time: on first load of a schema-1 file, write `subscriptions.json.v1.bak` before rewriting.

### 1.3 Caps

`MaxWatchesPerSub = 20` (the stated target). `maxBody` 16KB → 32KB (20 watches ≈ 4KB, leave headroom). Legacy `MaxTopicsPerSub = 60` retained for the legacy path.

---

## 2. Migration

**Existing subscriptions keep working with zero user action, and the first cycle after deploy produces byte-identical notifications for them.**

### 2.1 Store: topics → watches (lossless, semantics-preserving)

On load of a `schema: 1` file, for each subscription: parse each topic `rf_{ROUTE}_{ow|rt}_{c}`, **group by (route, kind), union the cabins**, and emit one Watch per group with `out`/`ret`/`nights` omitted.

This is an exact identity, not an approximation:

* old `ow` topic ≡ `{kind:"ow", cabins:[c], out:unbounded}` — "day D has cabin c".
* old `rt` topic ≡ `{kind:"rt", cabins:[c], out:unbounded, ret:unbounded, nights:{1,30}}` — the old joint condition is *literally* "some return in `[D+1, D+30]`", and `nights:{min:1,max:30}` expands to exactly `[D+1, D+30]`. Reverse-route-must-exist, horizon clipping, and today-clipping are unchanged.

Grouping cabins is also what the UI already renders (one `/alerts` row per route+kind with several swatches), so nothing looks different.

### 2.2 API: the stale-client hazard, and the merge rule

The SPA is cached; a stale `app.js` will keep calling `POST /subscribe {topics:[…]}` with **the whole set** and `GET /topics`. If `GET /topics` hides a date-constrained watch (it cannot be expressed as a topic) and the stale client then saves, it would **delete** that watch. That must not happen.

**Rule:**

* `GET /topics` returns the legacy projection: one topic per cabin, **only for watches that are topic-representable** (unbounded `out` and `ret`, `nights == {1,30}`, `minSeats < 2`). Constrained watches — including party watches, which a stale client cannot express — are invisible to it.
* `POST /subscribe` with `topics` (legacy write) **replaces only the topic-representable watches**; constrained watches are preserved untouched.
* `POST /subscribe` with `watches` (v2 write) **replaces the entire list**.
* Both keys present → `400`.
* `POST /unsubscribe` still removes everything (an explicit user action).

Net effect: a stale client can read and edit exactly what it understands, and cannot destroy what it does not.

### 2.3 Alert state file

`schema: 1 → 2`. Schema mismatch → start fresh (the existing `loadState` behaviour). Cost of starting fresh, once, at deploy: cooldown history is lost (a leg that happens to be mid-flap could re-alert once) and any pending batch is dropped. Both are bounded and one-off. Do **not** attempt to translate topic-keyed `LastPub` into subscription-keyed — not worth the code.

---

## 3. Detection algorithm

### 3.1 State the watcher holds

```go
type Watcher struct {
    prev  *bundleState              // as today: route -> []byte cabin bits per day (abs days)
    state *stateData                // persisted, §4
    idx   *watchIndex               // rebuilt only when store.Version() changes
    ver   uint64
}

// watchIndex: which watches can be affected by a change on route R?
//   ow watch on A-B  -> indexed under A-B
//   rt watch on A-B  -> indexed under A-B  AND under B-A   (a return-leg gain makes pairs)
type watchIndex struct {
    byRoute map[string][]watchRef    // watchRef = {subKey, endpoint, *Watch}
}
```

### 3.2 Per cycle

```
Cycle(rawBundle):
    b = parseBundle(raw)                       // unchanged
    if prev == nil            -> baseline(b); return          // never alerts
    if b.t < prev.t           -> WARN "source time went backwards"; baseline(b); return
    now   = b.t                                // NEVER wall clock
    today = unixDay(now)

    # ---- GLOBAL LAYER: what changed in the world (once per cycle) ----
    # Compare only days that existed in BOTH bundles and are not in the past.
    lo = max(today, b.epochDay, prev.epochDay)
    hi = min(prev.endDay, b.endDay)            # prev-horizon clip == today's rule

    gains  = {}                                # route -> [(day, gainedBits)]
    losses = {}                                # route -> [(day, lostBits)]
    for route, newBits in b.merged:
        oldBits = prev.merged[route]           # absent -> brand-new route: skip (see EC-11)
        if bytes.Equal(newBits, oldBits) { continue }        # 345 memcmps, ~free
        for d in [lo, hi):
            o = oldBits[d-prev.epochDay]; n = newBits[d-b.epochDay]
            if g := n &^ o; g != 0 { gains[route]  += (d, g) }
            if l := o &^ n; l != 0 { losses[route] += (d, l) }

    # circuit breaker: a source-repo rebuild / backfill must not page 1000 people
    if totalDays(gains) > maxGainDays (2000):
        WARN "bulk-change cycle, alerting skipped"; applyTransitions(gains, losses, now)
        prev = b; save(); return

    # ---- update the global transition ledger (BEFORE evaluating: order matters) ----
    # NOTE: isFlap() below reads closedAt/openedAt as of the START of this cycle for the
    # *partner* leg and the *just-closed* history, so apply losses first, then evaluate,
    # then apply gains. See §3.4.
    applyLosses(losses, now)                   # closedAt[k]=now; delete openedAt[k]

    # ---- PER-SUBSCRIBER LAYER: only watches on routes that actually moved ----
    hits = map[subKey][]item                   # item = {watchId, kind, route, D, R, cabin}
    for route in keys(gains):
        for w in idx.byRoute[route]:
            if w.expired(today) or w.impossible() { continue }
            evaluate(w, route, gains, b, now, today) -> items
            hits[w.subKey] += items

    applyGains(gains, now)                     # openedAt[k]=now  (after evaluation)

    for subKey, items in hits: addPending(subKey, items)
    prune(now, today); flush(now, today); prev = b; save()
```

### 3.3 `evaluate` — the heart

```
# Round trip. route A-B watched; `dirty` is the route that gained (A-B or B-A).
evaluate_rt(w, dirty, gains, b, now, today):
    out = b.merged[w.route]                    # A-B  (outbound leg)
    ret = b.merged[reverse(w.route)]           # B-A  (return leg); absent -> return nothing
    m   = w.cabinMask                          # e.g. C|F
    O   = clamp(w.out, today, b.endDay-1)      # absolute-day range, unbounded -> [today, end)
    R   = clamp(w.ret, today, b.endDay-1)      # unbounded -> derived from O + nights
    items = []

    if dirty == w.route:                       # (a) the OUTBOUND leg gained
        for (D, g) in gains[dirty]:
            if D not in O { continue }
            g &= m; if g == 0 { continue }
            for r in [max(D+w.nights.min, R.from) .. min(D+w.nights.max, R.to, len(ret)-1)]:
                c := g & ret[r]                # gained outbound cabins the return ALSO has now
                for bit in c:
                    if !isFlap(w.route,D, reverse,r, bit, now) { items += (D,r,bit) }

    if dirty == reverse(w.route):              # (b) the RETURN leg gained  <-- crux case #3
        for (r, g) in gains[dirty]:
            if r not in R { continue }
            g &= m; if g == 0 { continue }
            for D in [max(r-w.nights.max, O.from, today) .. min(r-w.nights.min, O.to, len(out)-1)]:
                c := g & out[D]                # the outbound may be WEEKS-OLD availability
                for bit in c:
                    if !isFlap(w.route,D, reverse,r, bit, now) { items += (D,r,bit) }

    return dedupe(items)                       # a pair whose BOTH legs gained appears in (a) and (b)
```

One-way is loop (a) with no partner:

```
evaluate_ow(w, dirty, gains, b, now, today):
    for (D, g) in gains[w.route]:
        if D in clamp(w.out, today, …) and (g & w.cabinMask) != 0:
            for bit in g & w.cabinMask:
                if !isFlap(w.route, D, nil, 0, bit, now) { items += (D, bit) }
```

**This is exactly the case in the brief's #3.** User wants out 1–20 Oct / back 10–31 Oct, Business. A `TYO-LON` Business return opens on 14 Oct. `dirty == reverse(w.route)` → loop (b) fires → `D` ranges over `[14 Oct − maxNights … 14 Oct − minNights] ∩ [1–20 Oct]` → `D = 3 Oct`, whose Business bit has been set for weeks → `c = C` → **alerted**, with the pair `3–14 Oct`. Nothing "opened" on the outbound and 3 Oct is old data; the pair is what is new.

Correspondingly: a watch with `nights ≤ 5` gets **nothing** (11 nights doesn't fit); a watch with `ret ≤ 13 Oct` gets **nothing**; a watch already holding pair (3,14) as satisfiable gets **nothing** (the gain would be zero).

### 3.4 `isFlap` — the cooldown, computed from global state only

Global ledger (both maps keyed `ROUTE|c|YYYY-MM-DD`, `ROUTE` directional so `TYO-LON` is its own leg):

* `openedAt[k]` — set on a gain. **Pruned once older than `Cooldown`.** So *absent + currently open ⇒ "open since before the cooldown window"*, i.e. −∞.
* `closedAt[k]` — set on a loss. **Pruned once older than `Cooldown`.** Absent ⇒ "no recent close".

```
isFlap(outRoute,D, retRoute,R, cabin, now):
    L1 = key(outRoute,D,cabin); L2 = key(retRoute,R,cabin)   # L2 nil for one-way
    # When did the PREVIOUS joint run of this pair end?  (-inf if there wasn't one)
    E = -inf
    for (L, other) in [(L1,L2), (L2,L1)]:
        cl := closedAt[L];  if absent { continue }           # this leg didn't close recently
        if other == nil || openedAt[other] absent || openedAt[other] <= cl:
            E = max(E, cl)          # `other` was already open at cl ⇒ a joint run ended at cl
    return E != -inf && (now - E) < Cooldown
```

Walk the four cases:

| scenario | result |
|---|---|
| Leg blinks off/on within 3h while the partner stays open (the classic flap) | `closedAt[L1]=t1`, partner `openedAt` absent ⇒ `E=t1`, gap<3h → **FLAP, suppressed** ✓ |
| **The trap:** outbound blinks off; *while it is off*, the return opens; outbound returns within 3h | for L1: `closedAt=t1`, but `openedAt[L2]=t1.5 > t1` ⇒ the partner was **not** open at t1 ⇒ no prior joint run ⇒ `E=-inf` → **ALERT** ✓ |
| Both legs blink off and on together (scrape artifact) | both qualify ⇒ `E=t1`, gap<3h → **FLAP, suppressed** ✓ |
| Leg genuinely closed 4h ago and reopens | `closedAt` was pruned ⇒ absent ⇒ `E=-inf` → **ALERT** ✓ |

The trap row is precisely the design a "cooldown on gains" implementation gets wrong (it suppresses the reopening gain and the user *never* hears about a pair that has never been announced). This is worth a named test.

Two map lookups per candidate pair-cabin. No per-subscriber cooldown state exists.

**Party thresholds get their own ledger plane.** A seat count sagging `3 → 1 → 3` never touches the presence bit, so the bit ledger cannot see the blink and a `minSeats ≥ 2` watch would re-alert on count churn. Threshold transitions therefore ledger under `ROUTE|c|S<N>|YYYY-MM-DD` (one plane per encoded threshold, maintained from the per-threshold gain/loss planes); bit keys keep the exact legacy 3-segment spelling so persisted state carries across the deploy, and every key still ends in the date, which is what `prune`'s date parsing reads (`TestThresholdLedgerPrunes`).

**Seats-layer detection semantics** (the `"s"` route key — 2 hex chars/day, 2-bit monotone code per cabin: 0 = no evidence of ≥2 seats [unknown OR known-1, deliberately collapsed], 1 = ≥2, 2 = ≥3, 3 = ≥4; MAX-merged across airlines, masked by `"a"`, which stays the sole presence authority):

* `minSeats ≤ 1` reads exactly the pre-seats bytes — byte-identical behaviour whether or not the bundle carries `"s"` (`TestSeatChurnInvisibleToOnePax`).
* `minSeats ≥ 2` gain = the per-cabin predicate "code ≥ minSeats − 1 AND presence bit set" **newly satisfied** this cycle; the partner leg must satisfy the same predicate. Threshold transitions are diffed only when BOTH cycles carry the seats layer for the route: the layer's FIRST appearance baselines silently (`TestSeatsLayerOnboardingBaselines`, mirroring EC-11) so a network-wide flight-detail backfill cannot flood gainDays past the EC-13 breaker — which would advance the ledger while publishing nothing, permanently silencing genuine same-cycle 1-pax openings. Party watches fire from the next transition after onboarding. EC-13's gain-day count includes threshold-plane days.
* A leg with no `"s"` (or code below the need) satisfies **nothing** at `minSeats ≥ 2`: unknown never fires — a push is a promise (`TestUnknownCountsNeverFireParty`). The read-time `no-seat-data` honesty therefore lives in the client (bell + `/alerts` row), which sees the same bundle.

### 3.5 Complexity at real numbers

Measured inputs: 345 routes × 518 days; ~182 change events/hour network-wide; `Cycle` only runs on a new *source commit* (every ~2s–3min).

| step | cost | at 1k subs × 20 watches |
|---|---|---|
| parse bundle | 345 × 518 nibble decodes | ~0.5 ms (already paid today) |
| diff | 345 × `bytes.Equal(518B)`, then a 518-byte scan for the ≤ few routes that moved | < 100 µs |
| gains | typically **1 route, 1–5 days** | — |
| watch eval | `Σ_dirty (watches on route) × (gain-days) × (nights window ≤ 60)` | avg: 58 watches/route × 5 × 60 = **17k byte-ANDs ≈ 30 µs**. Adversarial worst case (all 20k watches on the one route that moved): 20k × 5 × 60 = 6M byte-ops ≈ **6–10 ms** |
| ledger update + prune | O(events in a 3h window) ≈ 2–5k entries | < 1 ms |
| state write | ~100 KB JSON | ~2 ms |

Total worst case **~15 ms** inside a 2s loop, and it runs *after* `gitCommitPush` (unchanged in `watch.go`), so it cannot delay the data push. Add a WARN if a cycle's alert work exceeds 500 ms, with counts, so a pathology is visible.

**Caching / indexing decisions:**

* **`watchIndex` rebuilt only when `store.Version()` changes** (add a `Version() uint64` bumped on every mutation; snapshot under the store's existing `RWMutex`). 20k entries, rebuilt on subscribe, not per cycle.
* **The route `[]byte` IS the bitset.** One byte per day holds all four cabins, so the inner loop is a single `ret[r] & g`. Do *not* build per-cabin bitsets; it would be slower.
* **Dirty-route driving** is the whole optimisation: a watch is only touched when its route or its reverse moved.
* If watches-per-route ever gets extreme, the one further step is a per-route `uint64[9]` "any cabin open" bitmap so the window scan skips empty days with `popcount` instead of 60 byte loads. **Not needed at 1k subs — do not build it.**

---

## 4. Anti-spam state

`stateData` schema 2. Everything below persists across restarts (same atomic tmp+rename as today).

| map | key | value | size bound | pruned |
|---|---|---|---|---|
| `openedAt` | `ROUTE\|cabin\|YYYY-MM-DD` | unix t of the start of the current open run | **O(gains in the last `Cooldown`)** ≈ 182 events/h × 3h × ~2 day-cabins ≈ **~1k** | age > Cooldown; date < today |
| `closedAt` | same | unix t of the end of the last open run | **O(losses in the last `Cooldown`)** ≈ **~1k** | age > Cooldown; date < today |
| `pending` | `subKey` (sha256 of endpoint) | up to 64 items + an `overflow` count | **≤ #subs (1k)**, transient | drained on publish; past dates dropped; unknown subKey dropped |
| `lastPub` | `subKey` | unix t of last successful push | **≤ #subs that were pushed in the last `Batch`** | age > Batch; unknown subKey |

**Total ≈ 2–3k entries, ~100 KB.** This is a **~50× reduction** on today's `LastOn`, which is keyed `(typ, route, cabin, date)` over the whole dataset — at 72k non-zero route-dates that is on the order of 200k+ entries and a multi-MB file rewritten every cycle. The redesign both fixes the per-subscriber explosion *and* fixes an existing one, because the ledger is now sized by **events in a 3-hour window**, not by the dataset. Flag this to the owner as a free win.

**Cooldown semantics (per pair, per cabin, global):** a pair is not re-announced unless its joint availability was genuinely off for ≥ `Cooldown` (default 3h). Not keyed by subscriber, because *flapping is a property of the data*.

**Batch semantics (per subscriber):** the batch key is the **subscription**, not the topic and not the watch. At most **one push per device per `Batch` (default 1h)**; the first alert after a quiet hour goes out **immediately** (`lastPub` absent or older than `Batch`), so latency is unchanged for the common case. Everything that fires inside the hour is merged into one digest. Hard cap: **24 pushes/device/day**, regardless of how many watches they hold.

*Runner-up: batch per `(subscription, watch)` — more granular, but 20 watches → up to 20 pushes/hour, which is exactly the complaint the feature is meant to fix.*

**`lastFiredAt`** (per watch, for the UI — §7) lives on the **Watch in the store**, not in the alert state: the watcher calls `store.MarkFired(endpoint, watchIDs, t)` after a successful publish, and the store's existing debounced writer absorbs it. Bounded by #watches; carried across `Upsert` for unchanged ids.

**Pruning runs every cycle** (both ledgers are small enough to iterate) and drops: entries older than `Cooldown`, entries whose date is in the past, and any `pending`/`lastPub` whose `subKey` is no longer in the store (covers 404/410 removals and unsubscribes).

---

## 5. Notification copy

`maxListedPairs = 4`, `maxListedDates = 6`, `maxDigestWatches = 3`. Payload shape unchanged (`{title, body, url, tag}`) — `sw.js` needs no edit.

**Round trip, one watch, one cabin**
```
Title:  Business round trips open: LON ⇄ TYO
Body:   3 new: 12–19 Oct, 14–21 Oct, 16–23 Oct
URL:    /trip/LON-TYO?nights=7-14&out=2026-10-12&ret=2026-10-19
```
The URL deep-links the **first pair** — `/trip/` already parses `?nights=MIN-MAX&out=…&ret=…`, so one tap lands on the pair-picker with that trip selected. That is the whole point of the message.

**Round trip, one watch, several cabins** — group, cap at 4 pairs total:
```
Title:  Award round trips open: LON ⇄ TYO
Body:   Business: 12–19 Oct, 14–21 Oct · First: 16–23 Oct
```

**More pairs than the cap**
```
Body:   9 new: 12–19 Oct, 14–21 Oct, 16–23 Oct, 18–25 Oct, +5 more
```

**One way** (unchanged in spirit from today)
```
Title:  Business seats open: LON → TYO
Body:   3 new dates: Sun 12 Oct, Tue 14 Oct, Thu 16 Oct
URL:    /route/LON-TYO
```

**Party watch (`minSeats ≥ 2`)** — the threshold enters the title only when *every* item in the group is party news (render's dedupe keeps the lowest threshold per pair, so mixed news stays unqualified and true for everyone):
```
Title:  Business (3+ seats) round trips open: LON ⇄ TYO     (rt)
Title:  Business (2+ seats) open: LON → TYO                  (ow)
```

**Digest (several watches in one batch)**
```
Title:  Award space on 3 of your routes
Body:   LON ⇄ TYO: 3 new Business · NYC ⇄ MAD: 1 new First · SFO → LON: 2 new Business
URL:    /alerts
```
(`+N more` past 3 watches.)

**Date rendering:** `12–19 Oct`; crossing a month `28 Sep–5 Oct`; year appended to the end date only when it differs from the year of "today" (`28 Dec–4 Jan 2027`). Weekdays are dropped for round trips (they double the length); they stay for one-way, where the list is a single column of dates. Rendering is pure functions of absolute day numbers — no locale, no timezone.

**`tag`:** unique per send (`rf_<subKey[:8]>_<t>`), so notifications **stack rather than replace**. Today's `tag = topic` supersedes, which with digest batching would silently destroy dates the user hadn't acted on. The 1-push/hour/device cap makes stacking safe. *Owner decision: if you'd rather they collapse in the tray, set `tag = "rf"` — you lose unread detail.*

---

## 6. API changes

```
POST /subscribe      {endpoint, p256dh, auth, watches:[Watch]}      → v2, full replace
POST /subscribe      {endpoint, p256dh, auth, topics:[string]}      → legacy, merge (§2.2)
                     both keys → 400
     response        {ok:true, watches:[Watch + {status}]}          (normalized, ids assigned)

GET  /watches?endpoint=…   → {watches:[Watch + {status}]}           NEW
GET  /topics?endpoint=…    → {topics:[…]}                           legacy projection, deprecated
POST /unsubscribe    unchanged
POST /test           unchanged
GET  /healthz        + "watches": <total watch count>
```

`status` ∈ `active` | `expired` (all ranges in the past) | `impossible` (§8) | `no-return` (rt on a route with no reverse) — **computed at read time from the current date and the bundle's route set**; nothing is stored. The watcher injects `HorizonFunc() (todayDay, endDay int, routes map[string]bool)` into `alertapi.Config` (nil ⇒ skip the horizon/route checks, so tests and a cold start still work).

**Validation (all → 400 with a human-readable `error`):**

| rule | why |
|---|---|
| `route` matches `^[A-Z]{3}-[A-Z]{3}$` | grammar only — an **unknown** route is accepted (routes churn; the store must not depend on data). Watcher skips it, `status:"unknown-route"` |
| `kind ∈ {rt, ow}`; `cabins` ⊆ `{M,W,C,F}`, non-empty | |
| `ret`/`nights` absent unless `kind == "rt"` | |
| dates are `YYYY-MM-DD` and round-trip through `time.Parse` | rejects `2026-02-31` |
| `from ≤ to` on each range | |
| `to ≥ today − 1` (UTC) | a range wholly in the past is dead on arrival. **The 1-day grace is deliberate**: `todayIndex()` in app.js is the user's *local* calendar day, which can be one ahead of the watcher's UTC day |
| `to − from ≤ 180 days` for a **bounded** range | (a) abuse bound; (b) load-bearing for the horizon rule in §8. "I'm flexible" is the unbounded default, not a 2-year range |
| `from ≤ today + 800` | absurdity guard |
| `1 ≤ nights.min ≤ nights.max ≤ 60` | matches the UI clamp |
| `minSeats ∈ {0, 2, 3, 4}` (reject negatives, an explicit `1`, and `> 4`) | `0` is the only spelling of "one passenger" (id stability); `4` is the top threshold the seats layer encodes ("4+") |
| **feasibility**: reject if `ret.to < out.from + nights.min` **or** `ret.from > out.to + nights.max` | the watch could never fire. Catch it at save, not with eternal silence |
| `len(watches) ≤ 20` | |
| body ≤ 32 KB | |

**Wire tolerance for the server's own echoes:** the client's save flow is read-modify-write — `GET /watches`, edit, `POST /subscribe` the whole list — so a POSTed watch may legitimately carry the read-only fields the server itself emits (`status`; `id`/`createdAt`/`lastFiredAt` are Watch-schema fields already). The decoder keeps `DisallowUnknownFields` but whitelists exactly those echoes (tolerated and discarded, never stored or trusted); any other unknown field still 400s, so a misnamed `minSeats` fails loudly instead of silently dropping the user's constraint. The site independently strips watches to the wire schema before POSTing (one sanitize function on every save path), so new-site → old-server also works.

Rate limits, CORS allowlist, endpoint allowlist, and the endpoint-as-capability model are all unchanged.

---

## 7. UI

### 7.1 The two principles

1. **Inherit, don't ask.** By the time the bell is clicked, the page already knows the cabins (filter), the nights window (`buildNightsControl`), and often a picked outbound date. The bell reads all three.
2. **Show the answer while they choose.** The entire dataset is in memory. `roundTripBits()` already answers "how many trips match?" in microseconds. Every date control shows a **live match count** as it changes. This is what makes a silent alert legible (see §7.4) and it costs one function.

Add one client helper:

```js
// Trips currently matching a watch's constraints. Zero network. Reuses the rtCache.
function matchesNow(w) {                     // -> {pairs, cabins:Map<bit,count>}
  const rt = roundTripBits(w.route, reverse(w.route), w.mask, w.nights[0], w.nights[1]);
  // count D in outRange where rt[D] != 0 AND some R in [D+min,D+max] ∩ retRange has the bit
}
```

### 7.2 Bell panel — desktop

Default state, nothing picked on the calendar (**the path most users take — no date field is touched**):

```
┌──────────────────────────────────────────────────────┐
│  Alert me when a round trip opens                    │
│  LON ⇄ TYO                                           │
│                                                      │
│  CABINS                                              │
│   [✓] ▮ Business      [ ] ▮ First                    │
│   [ ] ▮ Prem. Econ.   [ ] ▮ Economy                  │
│                                                      │
│  TRIP LENGTH        7–14 nights   ·   change         │
│                                     (from this page) │
│                                                      │
│  WHEN CAN YOU TRAVEL?                                │
│   (•) Any time      ( ) Only these dates…            │
│                                                      │
│   ✓ 41 Business round trips match right now          │
│                                                      │
│              [   Save alerts   ]                     │
│   Turn off alerts for this route                     │
└──────────────────────────────────────────────────────┘
```

If the user **has already clicked an outbound day** on the calendar, the date mode defaults to *around that date* — the flex chip row, not a form:

```
│  WHEN CAN YOU TRAVEL?                                │
│   ( ) Any time   (•) Around 12 Oct   ( ) Exact dates │
│                                                      │
│   Flexible by:  [ ±3 ] [ ±7 ]* [ ±14 ] days          │
│   → Out 5–19 Oct · Back 12 Oct – 2 Nov · 7–14 nights │
│                                                      │
│   ✓ 4 Business round trips match right now           │
```

`±N` sets `out = pick ± N`; the **return range is derived** (`out.from + nights.min … out.to + nights.max`) and shown read-only. The count re-renders on every chip click. "Around my date + how flexible am I" is how people actually think, and it is one tap.

"Exact dates" is progressive disclosure — the only place two ranges appear, and the only way to say "back by the 31st" (which `out + nights` cannot express):

```
│  WHEN CAN YOU TRAVEL?                                │
│   ( ) Any time  ( ) Around 12 Oct  (•) Exact dates   │
│                                                      │
│   Leave between  [2026-10-01] and [2026-10-20]       │
│   Come back between [2026-10-10] and [2026-10-31]    │
│   Trip length  [ 7 ] – [ 14 ] nights                 │
│                                                      │
│   ⚠ No round trips match right now — we'll alert you │
│     the moment one opens.  (Widen dates)             │
```

Native `<input type="date">` with `min`/`max` clamped to the data horizon. The count line updates on `input`. If the constraints are **impossible** (§8) the Save button disables with `Your return window ends before your outbound + 7 nights.`

On save: `Armed. We'll tell you the moment a new Business round trip opens in your dates.` plus, when matches already exist, `4 match right now — see them →` (links to `/trip/…?nights=…` with the ranges applied). This is the answer to "should we push already-bookable space?" — **no** (§8, EC-12); we show it instead.

### 7.3 Bell panel — mobile

Full-screen sheet (same component, same order), sticky footer:

```
┌────────────────────────────┐
│  ✕   Alert me              │
│      LON ⇄ TYO · round trip│
├────────────────────────────┤
│  CABINS                    │
│   [✓] ▮ Business           │
│   [ ] ▮ First              │
│   [ ] ▮ Premium Economy    │
│   [ ] ▮ Economy            │
│                            │
│  TRIP LENGTH               │
│   7–14 nights      change  │
│                            │
│  WHEN CAN YOU TRAVEL?      │
│   ┌────────┬─────────────┐ │
│   │Any time│ Only dates… │ │
│   └────────┴─────────────┘ │
│                            │
│   ✓ 41 trips match now     │
├────────────────────────────┤
│  [      Save alerts      ] │   ← sticky
└────────────────────────────┘
```

### 7.4 `/alerts` page rows

```
┌───────────────────────────────────────────────────────────────────┐
│  Watching 3 routes        [Send a test notification]  [Turn all off]│
└───────────────────────────────────────────────────────────────────┘

┌───────────────────────────────────────────────────────────────────┐
│  LON ⇄ TYO                       ▮▮  round trip   [Edit]  [Off]   │
│  London to Tokyo                                                  │
│  Out 1–20 Oct · Back 10–31 Oct · 7–14 nights                      │
│  ● Armed · 4 Business round trips match right now                 │
└───────────────────────────────────────────────────────────────────┘
┌───────────────────────────────────────────────────────────────────┐
│  NYC → LON                        ▮   one way     [Edit]  [Off]   │
│  New York to London                                               │
│  Any date                                                         │
│  ● Armed · nothing matches right now — we'll tell you             │
│    Last alert 3 days ago                                          │
└───────────────────────────────────────────────────────────────────┘
┌───────────────────────────────────────────────────────────────────┐
│  SFO ⇄ LON                       ▮▮  round trip   [Edit]  [Off]   │   ← dimmed
│  Out 3–10 Mar · Back 10–17 Mar                                    │
│  ⚠ These dates have passed — edit them or turn the alert off      │
└───────────────────────────────────────────────────────────────────┘
```

`[Edit]` opens the **same bell panel component**, prefilled — one implementation. The match count and "last alert" line are what convert *"is this thing broken?"* into *"it's armed and there's genuinely nothing yet."* Given the owner's replay data (a 1–21 Oct outbound constraint produced **0** events in 16.4h while unconstrained produced 5), this line is not a nicety — **it is the feature's credibility**, and it is free.

### 7.5 What I would NOT build

* **Multiple date windows per watch.** One range each. Two windows = two watches.
* **Day-of-week constraints** ("Friday departures only"). Real demand, but a fourth UI dimension and a fourth predicate; the calendar already shows it.
* **Rolling windows** ("always the next 90 days"). They silently drift and need re-anchoring; "Any time" or explicit dates.
* **A custom range-drag calendar inside the popover.** Flex chips + native date inputs cover it; a second calendar widget is a permanent maintenance tax.
* **A "your alert expired" push.** Unsolicited push about *nothing being available* is spam. The `/alerts` row says it.
* **A "your dates are already bookable" push at subscribe time.** §8 EC-12.
* **Per-watch quiet hours, snooze, email/SMS fallback, points thresholds** (the last isn't in the data). *Seat-count thresholds were on this list while counts weren't in the data; the optional `"s"` bundle layer changed that — `minSeats` is now specced in §1.1/§3.4 and ships dormant-but-honest (a party watch on a route without seat data is savable, shown as never-fires by the client, and armed the moment data arrives).*

---

## 8. Edge cases

| # | case | behaviour |
|---|---|---|
| EC-1 | **Range expires** (`out.to` — and for rt `ret.to` — before today) | Watcher skips it (one integer compare). `status:"expired"` computed at read; `/alerts` shows it dimmed with an inline edit. **No push.** A background sweep purges watches expired > 30 days; a subscription whose watches are all purged is removed. |
| EC-2 | **Range beyond the booking horizon** | Accepted. Clamped at evaluation. See EC-3 for how it eventually fires. |
| EC-3 | **Horizon growth** (a new day arrives at the end of the string, already open) | Today's code clips to `prev.endDay` so horizon growth is never "new" — correct for a flexible watcher (otherwise they'd be pinged daily forever), but it means a user watching *next October* would **never** hear the day BA loads their dates. **Rule: horizon-edge gains (`D ≥ prevEndDay`) count only for watches with an explicitly bounded `out` range** — the user's own signal that they care about specific dates. The `to − from ≤ 180 days` validation (§6) is what bounds the blast radius: worst case one push/day while the frontier crawls their window, hourly-batched. Unbounded watches keep today's behaviour exactly. *Runner-up: a one-shot "your dates are now loading" event with a per-(sub,watch) cursor — more state, delays the T-355 release moment that award hunters actually want. **Owner decision.*** |
| EC-4 | **Impossible round trip** (`ret.to < out.from + nights.min`, or `ret.from > out.to + nights.max`) | Rejected at the API (400, plain-English message); the bell disables Save with the same message live. Re-checked in the watcher as defence in depth → `status:"impossible"`, skipped. |
| EC-5 | **Epoch shift / year rollover** | All arithmetic is in **absolute day numbers** (`parseDay`/`dayDate`, days since 1970-01-01 UTC); the bundle epoch only affects `epochDay`. The diff window is `[max(today, b.epochDay, prev.epochDay), min(prev.endDay, b.endDay))`, so days that vanish off the front on a New Year epoch shift are (a) in the past anyway and (b) never seen as losses. `TestEpochShiftDoesNotRealert` extends to pairs. |
| EC-6 | **DST / timezones** | Dates are calendar dates; the day boundary is **UTC midnight**. Detection time comes from the bundle's source commit `t`, never the wall clock, so replays are deterministic. Note the asymmetry: `todayIndex()` in app.js is the user's **local** calendar day, so a user in UTC+13 can consider a day "today" that the watcher considers past. Consequences are confined to (a) the API accepting `to ≥ today − 1` (the grace in §6) and (b) a match count that may include a day the watcher won't alert on. Both harmless; never construct dates via local-time `Date()`. |
| EC-7 | **rt watch on a route with no reverse** (1 of 345 today) | Can never fire. `status:"no-return"`; the bell disables the round-trip option when `store.bundle.routes[reverse]` is missing. |
| EC-8 | **Cabin the route never sells** | Harmless (never fires); the live match count shows 0 and the bell can hint "this route has no First". |
| EC-9 | **Subscription dies** (404/410) | `store.Remove` (unchanged) + prune its `pending`/`lastPub` on the next flush. |
| EC-10 | **Source timestamp goes backwards** (a rollback in the source repo) | `b.t < prev.t` → re-baseline, no alerts, WARN. Prevents negative cooldown arithmetic. |
| EC-11 | **New route appears / route disappears** | Absent from `prev.merged` → no gains computed for it this cycle (it becomes the baseline). Disappearing routes generate no losses (their watches simply never fire; `status:"unknown-route"`). |
| EC-12 | **Space is already bookable when the watch is saved** | **We do not push.** Alerts are for *new* space; a push on save would be indistinguishable from a real opening and would train people to ignore them. Instead the bell's live count and the `/alerts` "4 match right now →" line make the existing space visible at exactly the moment they'd wonder. This is only defensible *because* §7.2/§7.4 exist — ship them together. |
| EC-13 | **Bulk source rewrite** (backfill, re-scrape, repo rebuild) | Circuit breaker: `> 2000` gain-days in one cycle → advance the ledger, publish **nothing**, WARN. Never page 1000 people because a scraper was restarted. Gain-days include threshold-plane days, so the first cycle where the scraper emits seat data dataset-wide trips the same breaker instead of paging every party watch at once. |
| EC-14 | **`minSeats ≥ 2` watch on a route with no seats layer** | Never fires (unknown counts satisfy nothing — a push is a promise). The watch is savable and survives; the client's bell + `/alerts` row explain the dormancy ("no seat-count data for this route yet") instead of dead silence. It arms automatically when `"s"` arrives for the route; the arrival cycle itself baselines silently (see §3.4), and the watch fires from the next threshold crossing. |
| EC-15 | **Seat count sags below the threshold and returns** (`3 → 1 → 3`) | Invisible to the presence bit. The threshold-qualified ledger plane (`ROUTE|c|S<N>|date`) records the close/reopen, so within the cooldown it is a flap (suppressed) and after it a genuine re-deepening (alerts). The 1-pax plane is untouched either way. |

---

## 9. Test plan

The cases that separate a right implementation from a plausible-looking wrong one.

**Pair detection (crux #3)** — all against a hand-built two-bundle pair, watch = `LON-TYO rt, C, out 1–20 Oct, ret 10–31 Oct, nights 1–30`:

1. `RetGainPairsWithOldOutbound` — prev: `LON-TYO` 3 Oct has C (and has had for weeks); `TYO-LON` 14 Oct closed. new: `TYO-LON` 14 Oct gains C. **Expect exactly one item: (3 Oct, 14 Oct, C).** *A day-diffing implementation emits nothing here.*
2. `OutGainPairsWithOldReturn` — the mirror image.
3. `NightsWindowExcludes` — same as (1) with `nights {1,5}` (11 nights doesn't fit) → **nothing**.
4. `RetRangeExcludes` — same as (1) with `ret 10–13 Oct` → **nothing**.
5. `OutRangeExcludes` — same as (1) with `out 5–20 Oct` (3 Oct outside) → **nothing**.
6. `BothLegsGainSameCycle` — pair found by loop (a) *and* loop (b) → emitted **once**.
7. `AlreadySatisfiablePairIsNotNews` — an unrelated day on the same route changes; the existing pair is not re-emitted.
8. `CabinNotWatched` — F opens, watch wants C → nothing.
9. `HorizonGrowthOnly` — extend the bundle by one already-open day: an **unbounded** watch gets nothing; a watch with a bounded `out` covering that day gets **one** item (EC-3).

**Anti-spam:**

10. `ClassicFlapSuppressed` — leg opens → alert; closes; reopens 20 min later while the partner is open → **no second alert**. Reopens 4h later → **alert**.
11. **`FlapWithNewPartnerMustAlert`** — outbound C closes; *while it is off*, the return opens; the outbound reopens within the cooldown. **Expect an alert for the new pair.** This is the single most important test in the suite: it is the exact case a "cooldown on gains" implementation silently drops, forever.
12. `BothLegsFlapTogetherSuppressed` — synchronised blink → no re-alert.
13. `BatchingIsPerSubscriberNotPerTopic` — two watches fire 5 minutes apart → **one push immediately, one at the hour boundary**, and no pair appears in both messages.
14. `PerDeviceHourlyCap` — 20 watches all fire → **exactly one push**, a digest.
15. `BulkChangeCircuitBreaker` — a bundle where every route gains → **zero publications**, ledger advanced.
16. `StateStaysBounded` — replay 1000 churny cycles; assert `len(openedAt)+len(closedAt) < 5000` and that no key's date is in the past. *This is the test that would have caught the current `LastOn` growth.*
17. `RestartNoDuplicates` — save state mid-flight, reload, re-`Baseline` on the same bundle → no publications; a pair published before the restart is not republished.
18. `PublishFailureRetains` — publish returns an error → items stay pending, ledger still advances (today's contract).

**Party size (`minSeats`, §3.4 seats semantics):**

17. `SeatThresholdCrossingFires` — a count rising `1 → 2` on an already-open day (no bit change) fires a 2-pax watch and NOT a 1-pax watch; title carries `(2+ seats)`.
18. `UnknownCountsNeverFireParty` / `SeatChurnInvisibleToOnePax` — a bit gain with no `"s"` fires only the 1-pax watch; count churn under constant bits fires nobody at 1-pax.
    `PartnerLegMustHoldSeats` — the party must fit on BOTH legs; the return catching up later fires via loop (b). `SeatsAppearingIsAGain` — the layer arriving already-deep arms and fires. `SeatFlapSuppressed` — `2 → 1 → 2` within the cooldown is one alert. `ThresholdLedgerPrunes` — 4-segment keys prune by date. `MixedPartyAndOnePaxNewsUnqualified` — dedupe keeps the lowest threshold. `MinSeatsIDStability` — a MinSeats-less watch hashes to the pinned pre-feature id. `RepostedWatchesAccepted` — the server's own `GET /watches` echo round-trips through `POST /subscribe`; a genuinely unknown field still 400s.

**Multi-subscriber:**

19. `TwoSubsSameRouteDifferentRanges` — the same return opens; sub A (`out 1–20 Oct`) is alerted, sub B (`out 1–20 Nov`) is not.
20. `SameSubTwoOverlappingWatches` — a pair matching both watches appears once in the digest.

**Migration:**

21. `LegacyTopicsProduceIdenticalOutput` — **golden**: load a schema-1 store, run the recorded real-bundle sequence from `TestDryRunRealBundles` through both the old detector and the new one; assert the publication sequence is **identical** (title, body, URL, ordering) for legacy subscriptions.
22. `StaleClientCannotDeleteConstrainedWatch` — sub has one unbounded + one date-constrained watch. `GET /topics` returns only the unbounded one. `POST /subscribe {topics:[…]}` with a modified list → the constrained watch **survives**.
23. `SchemaOneBackupWritten` — `.v1.bak` exists after the first flush.

**API validation:** each row of the §6 table gets one 400 case and one accept case, plus `ImpossibleWatchRejected` (EC-4), `PastRangeRejectedWithOneDayGrace` (EC-6), `RangeLongerThan180DaysRejected` (EC-3's bound), `TwentyOneWatchesRejected`.

**Determinism:** re-run the whole bundle sequence twice → byte-identical publication list (the existing house rule: no wall clock anywhere).

---

## Via (chain) watches — shipped 2026-07-21

Extends the model to one-stop journeys (MULTICITY-SPEC Level 1): a watch may
carry `via` (the hub, a 3-letter place ≠ either endpoint) and `conn` (stop
length at the hub in nights, 1..3, default 1 — the overnight floor is a design
rule: flight times aren't in the data, so a same-day connection can't be
promised, and any arrival today makes any departure tomorrow bookable).

- **Model.** `Watch.LegRoutes()` expands `BLL-TYO via LON` to the chain
  `BLL-LON, LON-TYO[, TYO-LON, LON-BLL]`. Cabins constrain the FOCUS legs
  (same `focusLegs` half-of-longest rule as the site, from the bundle's place
  coords), coupled to one shared cabin; other legs need any award space for
  the party. `minSeats >= 2` with `via` is rejected at Normalize (EC-4: hop
  routes carry no seat data, so the watch could never fire). The id folds
  `|V<hub>C<conn>` conditionally — via-less ids are byte-identical to the
  pre-via formula (TestViaIDStability). Via watches are never
  topic-representable. Status checks every leg route (missing outbound leg →
  unknown-route, missing return leg → no-return).
- **Detection.** The leg-gains theorem generalizes: a chain is newly bookable
  iff bookable now AND ≥1 leg gained this cycle. A focus leg "gains" when a
  watched cabin bit appears (the existing per-cabin plane); a hop gains only
  when its day goes no-space → some-space (a new cabin beside existing space
  changes nothing the hop is asked for). The index lists a via watch under
  every leg route. Enumeration pins the gained day at its leg position and
  walks the junction windows ([1,conn] at the hub each way, nights at the
  destination); news granularity matches the site's via calendar — `Out` =
  first-leg departure day, `Ret` = return long-haul day. Frontier (EC-3),
  cooldown (via `isFlapChain`, the N-leg joint-run generalization over focus
  legs only — the per-cabin ledger cannot express a hop's any-space run), the
  EC-13 breaker, batching and caps all apply unchanged.
- **Copy.** Labels append the hub ("Business round trips open: BLL ⇄ TYO via
  LON"); items carry `v`/`cn` so the deep link lands on the via calendar with
  the pair pinned (`?out=&ret=&conn=`). `Via` joins `dedupeKey` and the render
  group key — direct and chain news on the same endpoints never merge.
- **Tests.** `chain_test.go` (10 scenarios: long-haul gain, hop gain, hop
  cabin churn silent, cabin coupling, overnight floor, missing leg, baseline,
  flap, one-way, frontier bounded-vs-unbounded) and `via_test.go` (id
  stability, Normalize table, LegRoutes/Status/topics). Client: `viatest.js`
  §8–9 (bell saves via+conn, /alerts shows the chain, live count via the
  chain matcher in `matchesNow`).

## Owner decisions flagged

1. **EC-3 horizon rule** — "bounded watches get frontier alerts" (my pick, no extra state, ≤ daily during the crawl) vs. a one-shot "your dates are loading" (less noise, delays the T-355 release moment).
2. **Notification `tag`** — stack (my pick; preserves unread dates) vs. collapse (tidier tray).
3. **`Cooldown` and `Batch` defaults** — I kept 3h / 1h, but the batch key changed from *topic* to *device*, which is a real behaviour change: a 20-watch user now gets at most 24 pushes/day instead of a possible 480.
4. **The free win** — the ledger redesign shrinks the existing state file from O(dataset) to O(events in 3h). Worth deploying even if the date-range feature slipped.