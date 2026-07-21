# Multi-city reward planning — scope + design spec

Status: LEVEL 1 SHIPPED (2026-07-21). The chain engine (`resolveRoutings`,
`chainBits`, `focusLegs`) and the Level 1 smart-search UI are live: searching
a pair with no direct route (BLL→TYO) resolves through the hub and renders a
via-trip calendar with per-leg booking. This document records the feasibility
findings and the design decisions the adversarial review forced.

Motivating user story: living in Denmark, an award trip to Tokyo is really
Billund→London→Tokyo — two separate redemptions. Today the site treats
BLL→TYO as "no route". The goal: searching BLL→TYO should be smart enough to
work out the routing itself.

## Feasibility verdict: YES, at zero bundle cost

Multi-city is a **client-side query over data we already ship**. Measured on the
live bundle:

- The route graph is a **star centered on London**: 173 places, 356 routes,
  average out-degree 2.1. LON's out-degree is 172 (universal hub); BLL's is 1
  (LON only). Every spoke→spoke journey is spoke→LON→spoke.
- `store.destsByOrigin` (built at adoption) IS the adjacency graph.
- `roundTripBits` was already a 2-leg chain evaluator; the generalization adds
  ~50 lines of dormant JS and **zero bytes of data**. No processor change, no
  format change, no new fetches.

## Engine (shipped, dormant)

Two primitives in `site/app.js`, directly after `roundTripBits`:

- `resolveRoutings(o, d, {maxStops})` — concrete routings that exist in the
  data: the direct route first, then any one-stop o→H→d. On the star graph this
  is O(out-degree); for a spoke city it returns exactly `[[o,"LON",d]]`.
  Deliberately data-driven, not hardcoded to LON — the fixture's ANU→SKB edge
  already exercises a non-London hub, and future non-star data needs no change.
- `chainBits(path, mask, gaps, pax, focus)` — per-departure-day cabin mask for
  completing the whole itinerary. `gaps` is one `[min,max]` day-window per
  junction, so connection-vs-stopover is expressed per stop:

  ```js
  // Billund→Tokyo return in Club: ≤1-day connections at LON, 7–14 nights in
  // Tokyo, Club coupled across BOTH long-haul legs (legs 1 and 2), hops free:
  chainBits(["BLL","LON","TYO","LON","BLL"], 4, [[0,1],[7,14],[0,1]], pax, [1,2])
  ```

  **`focus` is the load-bearing design decision** (forced by two rounds of
  adversarial review). Chain legs are separate redemptions, so the cabin
  filter must not couple every leg the way `roundTripBits` couples a
  single-ticket round trip: measured, BLL-LON has **0** premium-economy days
  while LON-TYO has **112** — an all-coupled W search from Billund is empty
  forever, while the shipped semantics find 73 full-round-trip W days on the
  same data. Round two caught the overshoot: a single focus leg would light
  "Club" days whose return long-haul was Economy-only. So `focus` is an
  ARRAY of leg indices: those legs couple to one shared cabin (a "Club" trip
  means Club on both long legs), every other leg needs award space in ANY
  cabin for the same party. `focusLegs(path)` picks them: every leg at least
  half as long (great-circle, from the places' `g` coords) as the longest.
  The result's bits are the focus legs' shared cabins, keyed by first-leg
  departure day. `focus = null` couples ALL legs — with path `[A,B,A]`, gaps
  `[[m,M]]` that is *provably* `roundTripBits(A-B, B-A, mask, m, M)`, the
  cross-check anchoring the engine to shipped code.

  Backward pass, one rule for every leg: a focus leg ANDs its masked cabins
  into the payload; a non-focus leg gates on any-space and passes the payload
  through (seeded at "every cabin" when no focus leg follows). O(legs × days
  × window). pax>1 thresholds every leg with seats data and falls back to
  presence elsewhere; the VIEWS apply pax only when every leg has the seats
  layer (`chainSeatsKnown`), same one-consistent-threshold stance as the
  direct pages.

Tests: `mctest.js` (harness dir) — 34 engine checks in the real minified build
against the `data-out-test` fixture: routing resolution, every chain shape
cross-checked against an independent per-bit brute-force reference (coupled,
single-focus, dual-focus; pax 1 and 4; one-way and 4-leg), equality with
`roundTripBits`, zero-gap connection ≡ AND of legs, the coupled-W-empty vs
focus-W-nonzero asymmetry, monotonicity (coupled ⊆ focus, more focus legs ⊆
fewer, full trip ⊆ opener, pax 4 ⊆ pax 1), `focusLegs` picks, and all-zeros on
every malformed input. `viatest.js` — 30 UI checks (below).

## Honesty caveat (must surface in the UI)

The data is day-granular: it can say two legs have award space on given days,
never that an arrival beats a departure. **The overnight-stop rule (owner
decision, 2026-07-21) resolves this:** every hub junction has a hard 1-night
floor (`conn` = 1..3 nights, default 1; no same-day option anywhere — site,
engine, or server), because any arrival today makes any departure tomorrow
bookable. Residual caveat the via note still carries: a long-haul FIRST leg
can itself land the next calendar day. Same point-in-time honesty stance as
the footer.

## Booking reality (investigated 2026-07-21)

BA does **not** support multi-city award booking online at all — it is
phone-only (£15 offline fee, waived for Gold), per ThriftyPoints and
AwardWallet. Consequently there is no multi-city `_gf` deep-link to build, and
none is needed: each leg is its own redemption, so the UI links each leg
through the existing `baBookingURL` (point-to-point `_gf` deep-link) — "Book
BLL→LON on 3 Oct", "Book LON→TYO on 4 Oct". Separate tickets also sidestep
the missed-connection/protection question by making it explicit: these are
independent bookings, and the UI should say so.

## Feature ladder

1. **Smart search — SHIPPED.** The user's own framing: "searching BLL→TYO
   should be smart enough to work out how to search for this properly."
   - Home search: destinations reachable in one stop are pickable
     (`reachableDests`); via pairs get no sparkline (a wrong shape is worse
     than none). Picking lands on the normal `/trip/O-D` URL.
   - `/trip/O-D` and `/route/O-D` with no direct route resolve a hub that
     works in the needed direction(s) (`viaHub`) and render the via calendar:
     "via London" badge, honesty note (separate bookings; dates matched on
     award space, not flight times), stop-length control (1 / ≤2 / ≤3
     nights at the hub — `?conn=`, default 1 night; overnight floor by design), trip-length +
     party controls as on direct pages, cabin chips recounted from the chain
     with the filter on the long-haul legs, unreleased-horizon cards.
   - Day panel: outbound legs as separate rows with per-leg BA deep links
     (BA can't book multi-city awards online), a date chooser when the
     connection window allows two long-haul days, return days as radio rows
     (nights measured from the long-haul departure), hop-home dates included,
     and a cabin line naming what's open on both long legs. Picks pin to
     `?out=`/`?ret=` and restore on load, as on direct trips.
   - Alert bell on via pages saves chain watches (`via` + `conn` on the
     wire; no party row — hop routes carry no seat data). Direct routes are
     untouched — the via path only activates where the empty state used to
     be.
   - "Where can you go" (map + `/from/` list) shows the whole reachable
     world (`viaDestsFrom`): via dots look like every other dot — hovering
     (or focusing) any dot draws its ROUTE as projected great-circle arcs
     (`gcRuns`, antimeridian-safe), one arc nonstop or two with a gold ring
     at the overnight connection (owner call: a permanent translucent
     treatment read as noise; the routing appears when you ask). List cards
     badge "via LON". All counts come from the chain engine with the same
     overnight stops and click through to the via calendar. chainBits
     gained the rtCache-style cache this made necessary (the map recounts
     on every pan frame).

   *Level 1.5 (cheap follow-on, not built):* when a direct route exists but
   the filtered view is empty, also offer the via-hub alternative —
   `resolveRoutings` already returns both, direct first.
2. **Explicit multi-city builder.** User-composed N-leg itineraries with
   per-junction stopover windows and a chosen focus leg (the engine already
   takes both). Only worth building if Level 1 sees use.
3. **Full graph routing (NOT recommended).** Dijkstra-style multi-hub search.
   The star topology makes it pointless today: every pair is already reachable
   in ≤1 stop through LON.

**Alerts on chains — SHIPPED (2026-07-21).** The watch model grew `via` +
`conn`, the server detection engine evaluates whole chains (the leg-gains
theorem generalizes: a chain is newly bookable iff bookable now AND ≥1 leg
gained — where a hop leg "gains" only on no-space → some-space), and the bell
is live on via pages. See ALERTS-SPEC "Via (chain) watches".
