/* Cvent Data service worker.
 *
 * Strategies (same-origin GET only; cross-origin requests are never touched):
 *   precached shell   cache-first, stored only on OK responses. The precache
 *                     list comes from /assets.json (content-hashed file
 *                     names, computed by the server) — no hardcoded array,
 *                     so a re-publish needs no SW source change.
 *   /api/cvent/event{,/payments}  network-first with a 5-minute runtime
 *                     cache; on network failure the cached copy is served,
 *                     and if it is >= 5 min old an offline signal is posted
 *                     on the "cvent-data" BroadcastChannel for the page UI
 *   /api/cvent/event/attendees*   always network, never cached
 *   /data/*  dump snapshots — cache-first, never expire (immutable)
 *   other static            stale-while-revalidate
 *   navigations             network-first, fall back to precached index.html
 *
 * Caches: "cvent-data-v1" (static), "cvent-data-api-v1" (API responses),
 * "cvent-data-api-meta" ({url, ts} sidecar for the 5-minute freshness check).
 * Bump "v1" to force a full re-precache on next activate.
 */

const STATIC_CACHE = "cvent-data-v13";
const API_CACHE = "cvent-data-api-v1";
const META_CACHE = "cvent-data-api-meta";
const OFFLINE_CHANNEL = "cvent-data";
const API_TTL_MS = 5 * 60 * 1000;

// Precached paths, populated from /assets.json at install. The regex is the
// install-independent fallback: a content-hashed shell asset always looks
// like <name>.<40 hex chars>.<js|css>.
const PRECACHED = new Set();
const HASHED_ASSET_RE = /^.+\.[0-9a-f]{40}\.(?:js|css)$/;

// Fallback precache list, used only when /assets.json is unreachable (e.g.
// a pre-hashing server build). The server's /assets.json is authoritative.
const PRECACHE_FALLBACK = [
  "/", "/index.html", "/styles.css", "/theme.js", "/app.js",
  "/sources/registry.js", "/sources/cvent.js", "/manifest.webmanifest",
  "/icons/icon-192.png", "/icons/icon-512.png",
  "/icons/icon-192-maskable.png", "/icons/icon-512-maskable.png",
  "/fonts/manrope-latin.woff2", "/fonts/space-grotesk-latin.woff2",
];

/* Fetch the server-computed precache list; the fallback on failure. */
async function precacheList() {
  try {
    const res = await fetch("/assets.json", { cache: "no-store" });
    if (res.ok) {
      const data = await res.json();
      if (Array.isArray(data.files) && data.files.length) return data.files;
    }
  } catch (err) {
    /* network or shape problem — fall through to the hardcoded list */
  }
  return PRECACHE_FALLBACK;
}

/* ---------- install: precache the static shell, then skipWaiting ---------- */

self.addEventListener("install", (event) => {
  event.waitUntil(
    (async () => {
      const files = await precacheList();
      files.forEach((p) => PRECACHED.add(p));
      const cache = await caches.open(STATIC_CACHE);
      try {
        await Promise.all(
          files.map((p) =>
            fetch(p).then((res) => {
              if (res && res.ok) return cache.put(p, res); // OK responses only
            })
          )
        );
      } catch (err) {
        // Failed precache: drop the partial cache, rethrow so install fails
        // and the browser keeps the previously-active worker.
        await caches.delete(STATIC_CACHE).catch(() => {});
        throw err;
      }
      await self.skipWaiting();
    })()
  );
});

/* ---------- activate: drop old cache generations, take control ---------- */

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      const keep = new Set([STATIC_CACHE, API_CACHE, META_CACHE]);
      const keys = await caches.keys();
      await Promise.all(
        keys.filter((k) => !keep.has(k)).map((k) => caches.delete(k))
      );
      await self.clients.claim();
    })()
  );
});

/* ---------- fetch routing ---------- */

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return; // cross-origin: ignore

  const path = url.pathname;

  // (a) Attendee search: ALWAYS network — stale results are worse than none.
  if (path.startsWith("/api/cvent/event/attendees")) {
    event.respondWith(fetch(req));
    return;
  }

  // (b) Core event payloads: network-first, 5-minute runtime cache,
  // stale-serve + offline signal on network failure.
  if (path === "/api/cvent/event" || path === "/api/cvent/event/payments") {
    event.respondWith(eventApi(req));
    return;
  }

  // (c) Navigations: network-first, fall back to the precached shell.
  if (req.mode === "navigate") {
    event.respondWith(
      fetch(req).catch(() => caches.match("/index.html"))
    );
    return;
  }

  // (d) Dump snapshots: cache-first, never expire (immutable; the server's
  // no-cache header is intentionally ignored for offline use).
  if (path.startsWith("/data/")) {
    event.respondWith(cacheFirst(req));
    return;
  }

  // (e) Precached shell files (the hashed list from /assets.json):
  // strict cache-first. A request matching the hashed-asset shape
  // (name.<40-hex>.js|css) is treated as precached even before install
  // populated the set.
  if (PRECACHED.has(path) || HASHED_ASSET_RE.test(path)) {
    event.respondWith(cacheFirst(req));
    return;
  }

  // (f) Everything else same-origin static: stale-while-revalidate.
  event.respondWith(staleWhileRevalidate(req));
});

/* ---------- strategy helpers ---------- */

async function cacheFirst(req) {
  const hit = await caches.match(req.url);
  if (hit) return hit;
  const res = await fetch(req);
  if (res && res.ok) {
    const cache = await caches.open(STATIC_CACHE);
    await cache.put(req.url, res.clone());
  }
  return res;
}

async function staleWhileRevalidate(req) {
  const cache = await caches.open(STATIC_CACHE);
  const hit = await cache.match(req.url);
  const network = fetch(req)
    .then((res) => {
      if (res && res.ok) {
        cache.put(req.url, res.clone()).catch(() => {});
      }
      return res;
    })
    .catch(() => hit || Response.error());
  return hit || network;
}

async function eventApi(req) {
  let res;
  try {
    res = await fetch(req);
  } catch (err) {
    // Network failure: serve cached copy; signal offline if >= TTL old.
    const cache = await caches.open(API_CACHE);
    const hit = await cache.match(req.url);
    if (!hit) return Response.error(); // never cached — let the view fail
    if ((Date.now() - await metaTs(req.url)) >= API_TTL_MS) {
      signalOffline(req.url);
    }
    return hit;
  }
  if (res && res.ok) {
    // Best-effort writes: a failure (e.g. quota) must never replace the
    // fresh response with a stale cached copy.
    try {
      const body = await res.clone().text();
      const meta = JSON.stringify({ url: req.url, ts: Date.now() });
      await Promise.all([
        caches.open(API_CACHE).then((c) => c.put(req.url, new Response(body, { status: 200 }))),
        caches.open(META_CACHE).then((c) => c.put(req.url, new Response(meta, { status: 200 }))),
      ]);
    } catch (err) {
      /* write failed — return the fresh response anyway */
    }
  }
  return res;
}

/* Fetch the {url, ts} sidecar entry; null when absent or malformed. */
async function metaTs(url) {
  const entry = await (await caches.open(META_CACHE)).match(url);
  if (!entry) return null;
  try {
    const data = await entry.json();
    return typeof data.ts === "number" ? data.ts : null;
  } catch (err) {
    return null;
  }
}

/* Page listens on the same-named BroadcastChannel for offline signals. */
function signalOffline(url) {
  try {
    const channel = new BroadcastChannel(OFFLINE_CHANNEL);
    channel.postMessage({ type: "offline", url });
    channel.close();
  } catch (err) {
    /* channel unavailable — the page's own online/offline events cover it */
  }
}
