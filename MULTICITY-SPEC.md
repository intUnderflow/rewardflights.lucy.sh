# Multi-city reward planning — scope + design spec

Status: SCOPED, engine prototyped dormant (2026-07-21). The chain engine ships
in `site/app.js` (`resolveRoutings`, `chainBits`) with a full test suite, but no
view calls it yet. This document records the feasibility findings so the UI
phase starts from evidence, not memory.

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
  // Tokyo, cabin filter on the long-haul (leg 1) — the real itinerary is ONE
  // call:
  chainBits(["BLL","LON","TYO","LON","BLL"], 4, [[0,1],[7,14],[0,1]], pax, 1)
  ```

  **`focus` is the load-bearing design decision** (found by adversarial
  review). Chain legs are separate redemptions, so the cabin filter must not
  couple them the way `roundTripBits` couples a single-ticket round trip.
  Measured on the fixture: BLL-LON has **0** premium-economy days while
  LON-TYO has **112** — a same-cabin W search from Billund would be empty
  forever, while focus-on-long-haul finds 128 one-way / 81 full-round-trip
  days on the same data. So `mask` applies only to the `focus` leg (callers
  default it to the longest leg by great-circle distance — every place ships
  `g` coords); other legs need award space in ANY cabin for the same party.
  The result's bits are the focus leg's cabins, keyed by first-leg departure
  day. `focus = null` keeps the coupled single-ticket semantics, and with
  path `[A,B,A]`, gaps `[[m,M]]` is *provably* `roundTripBits(A-B, B-A, mask,
  m, M)` — the cross-check that anchors the engine to shipped code.

  Backward pass: `reach[i][d]` = what legs *i..end* permit from day *d*
  (feasibility sentinel after the focus leg, focus cabins at and before it).
  O(legs × days × window). pax>1 thresholds every leg with seats data and
  falls back to presence bits elsewhere (same honest-note contract as the
  round-trip engine).

Uncached for now; when a view adopts it, add a cache alongside `rtCache`
(reset in `adoptBundle`, pax + gaps + focus in the key).

Tests: `mctest.js` (harness dir) — 27 checks in the real minified build against
the `data-out-test` fixture: routing resolution (hub, direct-first, no
self-trips), every chain shape cross-checked against an independent per-bit
brute-force reference (coupled AND focus modes, pax 1 and 4, one-way and
4-leg), equality with `roundTripBits`, zero-gap connection ≡ AND of legs, the
coupled-W-empty vs focus-W-nonzero asymmetry above, monotonicity (coupled ⊆
focus, full trip ⊆ its opener, pax 4 ⊆ pax 1), and all-zeros on missing legs /
malformed gaps / out-of-range focus. Fixture yields 147 one-way and 98
full-round-trip coupled matching days — real data, not vacuous passes.

## Honesty caveat (must surface in the UI)

The data is day-granular. "Same-day connection" (gap 0) means both legs have
award space on the same calendar day — NOT that the BLL arrival beats the TYO
departure, and long-haul overnight arrivals shift a day. So the default
connection window should be **[0, 1]** and the UI must say "award space on both
legs, timings not checked — verify when booking". This is the same
point-in-time honesty stance the footer already takes.

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

1. **Smart search (recommended first, the user's own framing).** When a
   searched pair has no direct route but `resolveRoutings` finds a one-stop,
   render the trip view over `chainBits` instead of an empty state, with a
   "via London" badge, a connection-window control (default [0,1] days), and
   per-leg booking links. Cabin filter targets the longest leg (`focus`); the
   UI states that plainly ("Club on the London–Tokyo leg; the Billund hop just
   needs award space"). No new page, no new URL shape beyond a `via` param.

   *Level 1.5 (cheap follow-on):* when a direct route exists but the filtered
   view is empty, also offer the via-hub alternative rather than a dead end —
   `resolveRoutings` already returns both, direct first.
2. **Explicit multi-city builder.** User-composed N-leg itineraries with
   per-junction stopover windows and a chosen focus leg (the engine already
   takes both). Only worth building if Level 1 sees use.
3. **Full graph routing (NOT recommended).** Dijkstra-style multi-hub search.
   The star topology makes it pointless today: every pair is already reachable
   in ≤1 stop through LON.

Alerts on chains are a later phase: the server detection engine evaluates the
same chain (its leg-gains theorem generalizes — a chain is newly bookable iff
bookable now AND ≥1 leg gained), watch model grows an optional `path`/`gaps`.
Not started.
