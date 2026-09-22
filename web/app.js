/* Cvent Data — SPA entry (hash router + shell wiring).
 *
 * The shell is source-agnostic: it reads the SOURCES registry and never
 * hardcodes a source name. v1 has one source (cvent) — the first
 * registered one is active; a source picker UI is not in v1.
 *
 * Responsibilities:
 *   1. Pick the active source from the registry.
 *   2. Hash router over the source's views: '' / '#/' → home,
 *      '#/<viewKey>' → that view, unknown hashes redirect to '#/'.
 *      The mount point is cleared before every render.
 *   3. Topbar Refresh → the source's refresh() (server repull); the
 *      button is disabled while a repull is in flight, and the current
 *      view re-fetches when it finishes.
 *   4. Service-worker registration — guarded: sw.js must exist (plain
 *      fetch probe) AND the page must be https or localhost, otherwise
 *      registration is skipped silently. The probe is an awaited fetch:
 *      a non-OK response means "not there yet, skip"; a network failure
 *      rejects, which the surrounding try/catch also skips.
 *   5. Offline banner: browser online/offline events plus the
 *      'cvent-data-offline' window event (detail = message). The worker
 *      can't dispatch window events, so it posts {type:'offline', url}
 *      on the "cvent-data" BroadcastChannel and this file forwards it to
 *      that window event.
 */

import { SOURCES } from "./sources/registry.js";

const sourceKey = Object.keys(SOURCES)[0];
const source = SOURCES[sourceKey];
const views = source.views;

const mount = document.getElementById("app-view");
const refreshBtn = document.getElementById("refresh-btn");
const offlineBanner = document.getElementById("offline-banner");

/* ---------- hash router ----------
   Routes are derived from the source's view keys (registry-driven):
   "home" maps to the bare hash, every other key to "#/<key>". */
const ROUTES = {};
Object.keys(views).forEach((key) => {
  ROUTES[key === "home" ? "/" : "/" + key] = key;
});

function currentRoute() {
  const path = location.hash.replace(/^#/, "") || "/";
  return ROUTES[path] ? path : null; // null = unknown
}

function route() {
  const path = currentRoute();
  if (path === null) {
    location.replace("#/"); // hashchange fires route() again
    return;
  }
  mount.innerHTML = ""; // clear the mount before every render
  views[ROUTES[path]](mount);
}

window.addEventListener("hashchange", route);

/* ---------- refresh (server repull) ---------- */

function setRefreshing(on) {
  if (!refreshBtn) return;
  refreshBtn.disabled = on;
  refreshBtn.classList.toggle("disabled", on);
  refreshBtn.setAttribute("aria-disabled", String(on));
  refreshBtn.title = on ? "Re-pull in progress…" : "Refresh";
}

if (refreshBtn && typeof source.refresh === "function") {
  refreshBtn.addEventListener("click", () => {
    if (refreshBtn.disabled || (source.isRepulling && source.isRepulling())) return;
    setRefreshing(true);
    source.refresh((err) => {
      setRefreshing(false);
      if (err) {
        console.error("repull failed:", err);
        return;
      }
      route(); // re-fetch the current view's data
    });
  });
}

/* ---------- service worker (guarded) ---------- */

async function registerSW() {
  if (!("serviceWorker" in navigator)) return;
  const secure =
    location.protocol === "https:" ||
    location.hostname === "localhost" ||
    location.hostname === "127.0.0.1";
  if (!secure) return;
  try {
    // Probe before registering: a non-OK response (404, or the SPA
    // fallback serving index.html for an unknown path) means the worker
    // is not deployed — skip. A network failure rejects this fetch; the
    // catch below treats it the same way (skip, silently).
    const res = await fetch("sw.js", { cache: "no-store" });
    if (!res.ok) return;
    await navigator.serviceWorker.register("sw.js");
  } catch (e) {
    /* not available / blocked — skip silently */
  }
}
registerSW();

/* ---------- offline banner ---------- */

function setOffline(on, msg) {
  if (!offlineBanner) return;
  offlineBanner.hidden = !on;
  if (on && msg) offlineBanner.textContent = msg;
  else if (on) offlineBanner.textContent = "Offline — showing last fetched data";
}

window.addEventListener("offline", () => setOffline(true));
window.addEventListener("online", () => setOffline(false));
// The worker dispatches this when its network-first fetch fails and it
// serves the cache fallback; detail carries the message to show.
window.addEventListener("cvent-data-offline", (e) => {
  setOffline(true, typeof e.detail === "string" ? e.detail : undefined);
});
// The worker can't dispatch window events — it posts on BroadcastChannel
// instead; forward to the window event above (same page, same channel).
if (typeof BroadcastChannel === "function") {
  new BroadcastChannel("cvent-data").addEventListener("message", (e) => {
    if (e.data && e.data.type === "offline") {
      window.dispatchEvent(
        new CustomEvent("cvent-data-offline", {
          detail: "Offline — showing last fetched data",
        })
      );
    }
  });
}

/* ---------- boot ---------- */

// Initial render: the active view for the current hash (or home when the
// hash is empty). This also clears theme.js's "Loading…" fallback, which
// only appears in the child-less window before first paint of the app.
route();
