# rewardflights.lucy.sh-data — file formats (schema 1)

This repository is **machine-generated** award ("Avios") seat-availability
data, derived from
[intUnderflow/rewardflights](https://github.com/intUnderflow/rewardflights)
by the processor in
[intUnderflow/rewardflights.lucy.sh](https://github.com/intUnderflow/rewardflights.lucy.sh).
Every file documented here is owned and rewritten by that processor — do not
edit them by hand. `README.md` is maintained by humans and is outside this
document's scope.

## Serialization (all JSON files)

- Stock Go `json.MarshalIndent`-style output: object keys sorted bytewise,
  **1-space indent**, UTF-8, LF line endings, exactly one trailing newline.
- Byte-identical regeneration: rebuilding from the same source commit produces
  the exact same bytes (no wall clock anywhere), so unchanged data creates no
  git churn and every diff line is a real data change.

## Provenance fields

| Field | Type | Meaning |
|---|---|---|
| `v` | string | Commit SHA of the **source** repo this generation was built from. Provenance, and the in-band change detector — CORS blocks reading `ETag` from raw.githubusercontent.com, so compare `v` instead. |
| `t` | number | The source commit's committer timestamp, unix seconds. This is "data as of" — **not** the processing wall clock. |
| `schema` | number | Format version, currently `1`. Bumped **only** on breaking changes; check `schema === 1` and prompt for an app update otherwise. New fields, airlines, routes and place codes are data, not schema — ignore anything unknown. |

`schema` appears on every file. `t` and `v` appear **only** on the files a
consumer polls or fetches for version — `manifest.json`, `availability.json`,
`changes/recent.json`, and the `flights/` detail files. The **origin shards**
and **`places.json`** deliberately omit `t`/`v`: they are versioned by the
manifest, and embedding the per-commit sha/time would rewrite every shard on
every commit (even unchanged ones), flooding the data repo with no-op diffs. A
shard-mode client cache-busts a shard with `?v=<manifest.v>`.

## Keys and codes

- **Route** keys are `ORIGIN-DEST`, where each side is an IATA
  metropolitan/city code (`LON` = all London airports, `TYO` = Haneda +
  Narita). Routes are **directional**: `LON-TYO` and `TYO-LON` are distinct.
- **Airline** ids are IATA-style codes (`BA`) assigned by an append-only
  registry; the `airlines` table in each availability file maps id →
  display name, source slug and cabin legend.
- **Cabin bitmask** (per the airline's `cabins` legend; for BA):
  Economy `M` = 1, Premium Economy `W` = 2, Business `C` = 4, First `F` = 8.

## File inventory

| Path | Purpose |
|---|---|
| `manifest.json` | ~200 B stable entrypoint: generation id, counts, epoch. Poll this. |
| `availability.json` | Whole-dataset bundle: airlines, places, per-route day strings. |
| `origins/<ORIGIN>.json` | Per-origin shard: the bundle's shape (minus `t`/`v`), routes filtered to that origin. |
| `places.json` | Standalone copy of the place table (no `t`/`v`). |
| `flights/<ORIG>/<DEST>/<YYYY-MM>.json` | Per-flight detail for one route-month (only where the source provides it). |
| `changes/recent.json` | Rolling feed of recently opened/closed/changed availability. |
| `FORMAT.md` | This document. |

## Day encoding — the nibble string

Availability for one route + airline is a hex string with **one uppercase hex
digit (nibble) per day**:

- `epoch` (`YYYY-MM-DD`, in manifest/bundle/shards) is **January 1 of the year
  containing the earliest available date** in the dataset. It is
  calendar-anchored so day-to-day regenerations only diff where data actually
  changed; it shifts at most once per year rollover.
- Character `i` (0-based) describes the day `epoch + i days`.
- Each digit is the bitwise OR of the cabins with award availability that day
  (`M=1, W=2, C=4, F=8`); `'0'` means no availability.
- Every string in a file has the same length, `days` = number of days from
  `epoch` through the latest available date in the dataset, inclusive.
  Leading/trailing runs of `0` cost almost nothing after gzip.
- Day lookup is O(1); cabin filtering is a bitmask test.
- Reserved for the future: an additive per-airline `width` field (default 1)
  lets an airline with more than four award buckets use 2 hex chars per day.
  Existing airlines' strings never rewrite when another airline's legend
  grows.
- Dates earlier than the source commit's UTC date are dropped at generation
  time, so strings never claim availability in the past.

### Worked example

Given `"epoch":"2026-01-01"` and a route entry

```json
"LON-TYO": {"a": {"BA": "0000000000…5…"}}
```

to look up 2026-07-12:

```
index = days from 2026-01-01 to 2026-07-12
      = 31 (Jan) + 28 (Feb) + 31 (Mar) + 30 (Apr) + 31 (May) + 30 (Jun) + 11
      = 192
```

(11, not 12, because the 12th is 11 days after July 1, and the index is
0-based from the epoch.) Then `s[192]`, say `'5'`: `0x5 = 1 + 4` → Economy
**and** Business have award availability; Premium Economy and First do not.
`'F'` = all four cabins; `'0'` = nothing that day.

## Seat thresholds — the seats string (optional `s` key)

Where the source provides per-flight seat counts, a route entry additionally
carries `s`: airline id → a string of **two uppercase hex characters (one
byte) per day**, over the same `epoch`/`days` window as `a`, so its length is
exactly `2 × days`. Day `d` is the byte `parseInt(s.slice(2*d, 2*d + 2), 16)`,
which packs one 2-bit threshold code per cabin:

| Bits | Cabin |
|---|---|
| 0–1 | Economy `M` |
| 2–3 | Premium Economy `W` |
| 4–5 | Business `C` |
| 6–7 | First `F` |

| Code | Meaning |
|---|---|
| 0 | no sign of ≥2 seats — count unknown **or** only 1 seat seen (deliberately collapsed; never means "0 seats") |
| 1 | ≥2 award seats on a single flight |
| 2 | ≥3 award seats on a single flight |
| 3 | ≥4 award seats on a single flight |

Semantics:

- The value is the **maximum across that day's flights** of the airline — a
  party must fit on **one** flight, so counts are never summed across flights.
  Merge airlines client-side with a per-cabin **MAX** (never OR or SUM): a
  party rides one airline's one flight.
- A party of N fits in a cabin iff the cabin's code ≥ N−1 (N=2 → code ≥ 1,
  N=3 → code ≥ 2, N=4 → code ≥ 3). N=1 needs only `a` — the seats layer never
  changes single-passenger semantics.
- `a` is the sole presence authority: a cabin's code is nonzero **only if**
  that cabin's bit is set in `a` for the same day. Contradictory source data
  resolves to the `a` bit (code forced to 0) with a processor warning.
- The key is present for a route+airline **only when at least one of its days
  carries flight detail**. An absent key means seat counts are unknown for
  the whole route+airline — fall back to `a` presence and say so; never treat
  absence (or code 0) as "0 seats".
- `schema` stays `1`: the key is additive, and clients that don't know it
  ignore it. It is unrelated to the reserved per-airline `width` mechanism,
  which concerns cabin-legend growth of `a` only and is untouched.

Worked example: `"s"` char pair `"40"` = byte `0x40` → First code
`(0x40 >> 6) & 3 = 1` (≥2 First seats on one flight); Economy, Premium
Economy and Business codes 0 (no sign of ≥2 seats).

## `manifest.json`

```json
{
 "bundle": "availability.json",
 "changes": "changes/recent.json",
 "counts": {"airlines": 1, "places": 173, "routeDates": 72116, "routes": 345},
 "epoch": "2026-01-01",
 "source": {…},
 "mode": "bundle",
 "schema": 1,
 "t": 1770000000,
 "v": "<source sha>"
}
```

- `bundle` / `changes`: repo-relative paths of the bundle and the changes feed.
- `counts`: dataset totals — `routeDates` is the number of route+date pairs
  with availability. Informational; the bundle is authoritative.
- `mode`: currently always `"bundle"` (fetch `availability.json`). A future
  `"shard"` mode will direct clients to `origins/` shards if the dataset
  outgrows the bundle budget; the format already supports it.

## `availability.json` (and `origins/<ORIGIN>.json`)

```json
{
 "airlines": {
  "BA": {
   "cabins": {"1": "Economy", "2": "Premium Economy", "4": "Business", "8": "First"},
   "name": "British Airways",
   "slug": "british-airways"
  }
 },
 "days": 517,
 "epoch": "2026-01-01",
 "source": {…},
 "places": {
  "LON": {
   "country": "United Kingdom",
   "g": [51.507, -0.128],
   "name": "London",
   "search": ["Heathrow", "LHR", "Gatwick", "LGW"]
  }
 },
 "routes": {
  "LON-TYO": {"a": {"BA": "000…680C…"}, "fm": ["2026-10"]}
 },
 "schema": 1,
 "t": 1770000000,
 "v": "…"
}
```

- `airlines`: airline id → `{cabins, name, slug}`. `cabins` maps each bitmask
  value (as a decimal string) to a display label. Render unknown bits as
  "Other".
- `places`: place code → `{name, country?, search?, g?}` for **every code
  appearing in `routes`**. `search` (optional) lists extra autocomplete
  aliases — airport names and airport IATA codes for multi-airport metros.
  `g` (optional) is `[latitude, longitude]` — the city itself for metro
  codes, else the airport (airport coordinates from OurAirports, public
  domain). A code the processor doesn't know yet appears as
  `{"name": "<CODE>"}`; render the raw code and skip it on maps.
- `routes`: route → entry. In a route entry, **lowercase keys are metadata**:
  - `a`: airline id → nibble string (see day encoding). A route served by
    several airlines has several keys; merge availability client-side with a
    per-day bitwise OR.
  - `fm` (optional, omitted when empty): sorted list of `YYYY-MM` route-months
    that have a `flights/<ORIG>/<DEST>/<YYYY-MM>.json` detail file. Only fetch
    detail for listed months — never probe (raw.githubusercontent.com caches
    404s). A listed month that still 404s is "detail pending", not an error.
  - `s` (optional, omitted when the route has no flight detail): airline id →
    seat-threshold string, 2 hex chars per day (see the seat-thresholds
    section). Absent = seat counts unknown; fall back to `a`.
- `origins/<ORIGIN>.json` is byte-compatible in shape: `routes` is filtered to
  routes starting at that origin, and `places` to the codes those routes
  reference. Everything else (airlines, epoch, days, source) is identical.

## `places.json`

```json
{"places": {…}, "schema": 1, "t": 1770000000, "v": "…"}
```

The same `places` table as the bundle, standalone — for shard-mode clients
and tooling. Only codes present in the current data are included.

## `flights/<ORIG>/<DEST>/<YYYY-MM>.json`

Per-flight (airport-level) detail for one route-month, all airlines merged.
Emitted only where the source data carries flight detail; the calendar-layer
files above never grow when detail ships.

```json
{
 "days": {
  "15": {
   "BA": [
    {
     "arr": "08:55",
     "car": ["BA"],
     "dep": "13:00",
     "fn": ["BA0007"],
     "peak": "off-peak",
     "rfs": true,
     "seats": {"C": 1, "W": 6},
     "via": []
    }
   ]
  }
 },
 "route": "LON-TYO",
 "schema": 1,
 "t": 1770000000,
 "v": "…"
}
```

- `days` keys are **zero-padded 2-digit days of the month** (lexical order =
  chronological order); each maps airline id → array of flight objects.
- Flight object fields:

| Field | Meaning |
|---|---|
| `fn` | Marketing flight number(s); more than one = a connecting itinerary, in order. |
| `car` | Operating carrier IATA code(s), deduplicated. |
| `via` | Intermediate airport(s) for connections; `[]` = non-stop. |
| `dep` / `arr` | Scheduled local times, 24h `HH:MM` — first-segment departure, last-segment arrival. |
| `peak` | Airline pricing band for the date: `"peak"`, `"off-peak"`, or `null` (unknown). |
| `rfs` | `true` if the flight offers Reward Flight Saver (reduced cash surcharge). |
| `seats` | Cabin code → award seats available. Cabins with zero seats are omitted. |

## `changes/recent.json`

```json
{
 "entries": [
  {"al": "BA", "c": "C", "d": "2026-10-15", "k": "opened", "r": "LON-TYO", "t": 1770000000}
 ],
 "schema": 1,
 "t": 1770000000,
 "v": "…"
}
```

A rolling feed of availability changes at route + airline + date granularity,
newest first, trimmed to 1000 entries. This is the **only state-dependent
file**: each generation diffs the previous `availability.json` against the
new one and prepends what changed. Entry fields:

- `r` route, `al` airline id, `d` the departure date (`YYYY-MM-DD`).
- `k`: `"opened"` (date had no availability, now has), `"closed"` (had, now
  none), `"changed"` (cabin set differs).
- `c`: the cabin letters concerned, in `MWCF` order — the **new** cabin set
  for `opened`/`changed`, the **last-seen** cabin set for `closed`.
- `t`: the source timestamp of the generation that observed the change.

Dates that merely rolled into the past are **not** reported as closed.

Seat-threshold (`s`) transitions are **not** reported in this feed: entries
reflect `a` cabin-bit changes only, so a day going from 2 to 4 seats (bit
unchanged) emits nothing. A future format revision may add a seat-transition
entry kind; consumers must ignore unknown `k` values.

## Size budgets

Enforced by the processor at generation time: `availability.json` ≤ 300 KiB
gzipped; every other file ≤ 50 KiB gzipped.

## Provenance

This dataset is derived from
[github.com/intUnderflow/rewardflights](https://github.com/intUnderflow/rewardflights).
It carries no license of its own. Every file embeds a machine-readable
`source` block naming that repo, with a no-warranty note: availability facts
are provided as-is, with no guarantee of accuracy or bookability.
