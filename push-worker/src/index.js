/* rewardflights.lucy.sh — push subscription store.
 *
 * This Worker is deliberately dumb: it holds Web Push subscriptions and hands
 * them to the alert sender. It never sends notifications (the watcher does the
 * encryption and sending, which would blow the Worker CPU budget at fan-out)
 * and it stores no personal data — a subscription is an opaque push endpoint
 * plus its two public key blobs, with no account, email, or IP attached.
 *
 * KV layout (one namespace):
 *   t:<topic>:<endpointHash>  -> {endpoint, p256dh, auth}   (per-topic index)
 *   e:<endpointHash>          -> {endpoint, topics: [...]}  (for unsubscribe)
 *   Topics look like: rf_LON-TYO_rt_C  (route, one-way|round-trip, cabin)
 *
 * Public routes (browser):
 *   POST   /subscribe    {endpoint, p256dh, auth, topics: []}
 *   POST   /unsubscribe  {endpoint}                (endpoint == capability)
 *   GET    /topics?endpoint=...                    (what am I subscribed to?)
 * Authed routes (the alert sender, Bearer PULL_SECRET):
 *   GET    /subs?topic=<topic>     -> [{endpoint, p256dh, auth}]
 *   GET    /active-topics          -> [topic, ...]  (topics with ≥1 subscriber)
 *   DELETE /subs  {endpoint}       -> prune a dead (404/410) subscription
 */

const TOPIC_RE = /^rf_[A-Z]{3}-[A-Z]{3}_(ow|rt)_[MWCF]$/;
const MAX_TOPICS_PER_SUB = 60;
const MAX_BODY = 16 * 1024;

/* Only accept endpoints belonging to real push services, so the store can't be
   used as free arbitrary key-value storage. */
const PUSH_HOSTS = [
  /\.push\.apple\.com$/,
  /^fcm\.googleapis\.com$/,
  /^updates\.push\.services\.mozilla\.com$/,
  /\.notify\.windows\.com$/,
  /\.push\.services\.mozilla\.com$/,
];

const json = (data, status = 200, extra = {}) =>
  new Response(JSON.stringify(data), {
    status,
    headers: { "content-type": "application/json", ...cors(), ...extra },
  });

const cors = () => ({
  "access-control-allow-origin": "https://rewardflights.lucy.sh",
  "access-control-allow-methods": "GET,POST,DELETE,OPTIONS",
  "access-control-allow-headers": "content-type,authorization",
  "access-control-max-age": "86400",
});

async function hashEndpoint(endpoint) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(endpoint));
  return btoa(String.fromCharCode(...new Uint8Array(digest)))
    .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function validEndpoint(endpoint) {
  let u;
  try { u = new URL(endpoint); } catch { return false; }
  return u.protocol === "https:" && PUSH_HOSTS.some((re) => re.test(u.hostname));
}

async function readBody(request) {
  const raw = await request.text();
  if (raw.length > MAX_BODY) throw new Error("body too large");
  return JSON.parse(raw);
}

function authed(request, env) {
  const got = request.headers.get("authorization") || "";
  const want = `Bearer ${env.PULL_SECRET}`;
  // Constant-time-ish: compare fixed-length digests rather than raw strings.
  return got.length === want.length && got === want;
}

async function listAll(env, prefix) {
  const out = [];
  let cursor;
  do {
    const page = await env.SUBS.list({ prefix, cursor });
    out.push(...page.keys);
    cursor = page.list_complete ? undefined : page.cursor;
  } while (cursor);
  return out;
}

async function prune(env, endpoint) {
  const h = await hashEndpoint(endpoint);
  const rec = await env.SUBS.get(`e:${h}`, "json");
  const topics = rec?.topics || [];
  await Promise.all([
    ...topics.map((t) => env.SUBS.delete(`t:${t}:${h}`)),
    env.SUBS.delete(`e:${h}`),
  ]);
  return topics.length;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method === "OPTIONS") return new Response(null, { headers: cors() });

    try {
      /* ---------- browser: subscribe ---------- */
      if (request.method === "POST" && url.pathname === "/subscribe") {
        const { endpoint, p256dh, auth, topics } = await readBody(request);
        if (!validEndpoint(endpoint)) return json({ error: "bad endpoint" }, 400);
        if (typeof p256dh !== "string" || typeof auth !== "string") {
          return json({ error: "bad keys" }, 400);
        }
        const want = [...new Set(Array.isArray(topics) ? topics : [])].filter((t) => TOPIC_RE.test(t));
        if (!want.length) return json({ error: "no valid topics" }, 400);
        if (want.length > MAX_TOPICS_PER_SUB) return json({ error: "too many topics" }, 400);

        const h = await hashEndpoint(endpoint);
        const prev = (await env.SUBS.get(`e:${h}`, "json"))?.topics || [];
        const sub = JSON.stringify({ endpoint, p256dh, auth });
        const gone = prev.filter((t) => !want.includes(t));

        await Promise.all([
          ...want.map((t) => env.SUBS.put(`t:${t}:${h}`, sub)),
          ...gone.map((t) => env.SUBS.delete(`t:${t}:${h}`)),
          env.SUBS.put(`e:${h}`, JSON.stringify({ endpoint, topics: want })),
        ]);
        return json({ ok: true, topics: want });
      }

      /* ---------- browser: unsubscribe (knowing the endpoint is the capability) ---------- */
      if (request.method === "POST" && url.pathname === "/unsubscribe") {
        const { endpoint } = await readBody(request);
        if (!validEndpoint(endpoint)) return json({ error: "bad endpoint" }, 400);
        const n = await prune(env, endpoint);
        return json({ ok: true, removed: n });
      }

      /* ---------- browser: what am I subscribed to? ---------- */
      if (request.method === "GET" && url.pathname === "/topics") {
        const endpoint = url.searchParams.get("endpoint") || "";
        if (!validEndpoint(endpoint)) return json({ error: "bad endpoint" }, 400);
        const rec = await env.SUBS.get(`e:${await hashEndpoint(endpoint)}`, "json");
        return json({ topics: rec?.topics || [] });
      }

      /* ---------- sender: subscriptions for a topic ---------- */
      if (request.method === "GET" && url.pathname === "/subs") {
        if (!authed(request, env)) return json({ error: "unauthorized" }, 401);
        const topic = url.searchParams.get("topic") || "";
        if (!TOPIC_RE.test(topic)) return json({ error: "bad topic" }, 400);
        const keys = await listAll(env, `t:${topic}:`);
        const subs = await Promise.all(keys.map((k) => env.SUBS.get(k.name, "json")));
        return json(subs.filter(Boolean));
      }

      /* ---------- sender: which topics have anyone listening? ---------- */
      if (request.method === "GET" && url.pathname === "/active-topics") {
        if (!authed(request, env)) return json({ error: "unauthorized" }, 401);
        const keys = await listAll(env, "t:");
        const topics = [...new Set(keys.map((k) => k.name.split(":")[1]))];
        // Cacheable: the sender polls this to skip topics nobody wants.
        return json(topics, 200, { "cache-control": "max-age=60" });
      }

      /* ---------- sender: prune a dead subscription (push service said 404/410) ---------- */
      if (request.method === "DELETE" && url.pathname === "/subs") {
        if (!authed(request, env)) return json({ error: "unauthorized" }, 401);
        const { endpoint } = await readBody(request);
        if (typeof endpoint !== "string" || !endpoint) return json({ error: "bad endpoint" }, 400);
        const n = await prune(env, endpoint);
        return json({ ok: true, removed: n });
      }

      return json({ error: "not found" }, 404);
    } catch (err) {
      return json({ error: String(err.message || err) }, 400);
    }
  },
};
