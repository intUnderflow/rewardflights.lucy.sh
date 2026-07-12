# push-worker — alert subscription store

A deliberately dumb Cloudflare Worker: it holds Web Push subscriptions and
hands them to the alert sender. It **never sends notifications** (the watcher
does the encryption and sending — bulk RFC 8291 encryption would blow a
Worker's CPU budget) and it **stores no personal data**: a subscription is an
opaque push endpoint plus its two public key blobs. No account, no email, no
IP, nothing to leak or to subject-access.

## What a subscription looks like

```
t:rf_LON-TYO_rt_C:<sha256(endpoint)>  ->  {endpoint, p256dh, auth}
e:<sha256(endpoint)>                  ->  {endpoint, topics: [...]}
```

A topic is one thing a person wants: `rf_LON-TYO_rt_C` = "Business **round
trips** on LON⇄TYO". `rt` = round trip, `ow` = one way.

## Deploy (once)

```sh
cd push-worker
npx wrangler kv namespace create SUBS      # paste the id into wrangler.toml
npx wrangler secret put PULL_SECRET        # long random string; the sender uses it
npx wrangler deploy
```

Then either use the printed `*.workers.dev` URL, or add a custom domain route
(`push.rewardflights.lucy.sh/*`) in the Cloudflare dashboard. Whichever you
pick must match `PUSH_API` in `site/app.js` and the `connect-src` entry in
`site/_headers`.

## Routes

| Method | Path | Who | Purpose |
|---|---|---|---|
| POST | `/subscribe` | browser | store/replace a subscription's topic set |
| POST | `/unsubscribe` | browser | forget a subscription entirely |
| GET | `/topics?endpoint=` | browser | which topics is this device watching? |
| GET | `/subs?topic=` | sender (Bearer) | subscribers for one topic |
| GET | `/active-topics` | sender (Bearer) | topics with ≥1 subscriber |
| DELETE | `/subs` | sender (Bearer) | prune an endpoint the push service reported dead (404/410) |

Subscribe only accepts endpoints on real push services (Apple/Google/Mozilla/
Microsoft), so the store can't be abused as free key-value storage.

## Free-tier notes

KV's **1,000 writes/day** is the binding limit, not requests: one subscription
change costs (number of topics + 1) writes. That's roughly 250 subscription
edits a day — plenty at hobby scale, and reads (what the sender does
constantly) are far more generous.
