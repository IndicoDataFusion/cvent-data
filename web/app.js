/* Cvent Data — SPA entry (hash router + shell wiring).
 *
 * The shell is source-agnostic: it reads the SOURCES registry and never
 * hardcodes a source name. v1 has one source (cvent) — the first
 * registered one is active; a source picker UI is not in v1.
 *
 * Multi-event routing:
 *   #/                    → the source's "events" landing (event list)
 *   #/<code>              → that event's "home" dashboard
 *   #/<code>/<viewKey>    → that event's <viewKey> view (e.g. attendees)
 *   …?k=v                 → optional query, passed to the view as params
 *                           (e.g. #/<code>/attendees?q=…&open=<id>)
 * The event code is read from the hash and passed to the view; it is never
 * hardcoded. The topbar event selector mirrors the route and lets the user
 * jump between events without losing the current view.
 *
 * Responsibilities:
 *   1. Pick the active source from the registry.
 *   2. Hash router over the source's views (see scheme above). The mount
 *      point is cleared before every render; unknown hashes redirect to '#/'.
 *   3. Topbar event selector: populated from the source's catalog
 *      (getEvents); reflects the current route's event and navigates on
 *      change. Hidden when the source exposes no catalog.
 *   4. Topbar Refresh → the source's refresh(code) (server repull) for the
 *      current event; the button is disabled while a repull is in flight,
 *      and the current view re-fetches when it finishes.
 *   5. Service-worker registration — guarded: sw.js must exist (plain
 *      fetch probe) AND the page must be https or localhost, otherwise
 *      registration is skipped silently. The probe is an awaited fetch:
 *      a non-OK response means "not there yet, skip"; a network failure
 *      rejects, which the surrounding try/catch also skips.
 *   6. Offline banner: browser online/offline events plus the
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
const eventSelect = document.getElementById("event-select");
const viewTabs = document.getElementById("view-tabs");

/* ---------- hash router ----------
   Routes are derived from the source's view keys (registry-driven):
   "events" is the bare hash (the landing), "home" is "#/<code>", and every
   other key is "#/<code>/<key>". The event code is a path segment, not a
   view key, so it can't collide with a view name. */

function parseRoute() {
  const raw = location.hash.replace(/^#/, "") || "/";
  const qi = raw.indexOf("?");
  const path = qi < 0 ? raw : raw.slice(0, qi);
  const params = new URLSearchParams(qi < 0 ? "" : raw.slice(qi + 1));
  const parts = path.split("/").filter(Boolean);
  if (parts.length === 0) {
    // #/ → the events landing (no event code).
    if (views.events) return { view: "events", code: null };
    return null;
  }
  const code = decodeURIComponent(parts[0]);
  if (parts.length === 1) {
    // #/<code> → that event's home dashboard.
    if (views.home) return { view: "home", code, params };
    return null;
  }
  if (parts.length === 2) {
    // #/<code>/<viewKey> → that event's named view.
    const view = parts[1];
    if (views[view]) return { view, code, params };
  }
  return null; // unknown shape
}

function currentView() {
  return parseRoute();
}

function route() {
  const r = currentView();
  if (!r) {
    location.replace("#/"); // hashchange fires route() again
    return;
  }
  mount.innerHTML = ""; // clear the mount before every render
  if (r.view === "events") views.events(mount);
  else views[r.view](mount, r.code, r.params);
  syncEventSelect(r.code);
  syncViewTabs(r);
}

window.addEventListener("hashchange", route);

/* ---------- view tab bar ----------
   Built once from the active source's `tabs` (registry-driven, source-
   agnostic). Shown on every per-event route (home + named views), hidden
   on the events landing. The active tab mirrors the current route; a tap
   navigates to #/<code>/<key> (home → #/<code>). */

function buildViewTabs() {
  if (!viewTabs || !Array.isArray(source.tabs) || !source.tabs.length) return;
  viewTabs.innerHTML = "";
  for (const t of source.tabs) {
    const a = document.createElement("a");
    a.className = "view-tab";
    a.setAttribute("role", "tab");
    a.dataset.tab = t.key;
    a.textContent = t.label;
    a.addEventListener("click", (e) => {
      e.preventDefault();
      const r = currentView();
      if (!r || !r.code) return;
      const hash = t.key === "home" ? "#/" + encodeURIComponent(r.code)
        : "#/" + encodeURIComponent(r.code) + "/" + t.key;
      if (location.hash === hash) return;
      location.hash = hash;
    });
    viewTabs.appendChild(a);
  }
}

function syncViewTabs(r) {
  if (!viewTabs) return;
  // Hidden on the events landing (no event code → no per-event views).
  viewTabs.hidden = !r || !r.code;
  if (viewTabs.hidden) return;
  viewTabs.querySelectorAll(".view-tab").forEach((a) => {
    const on = a.dataset.tab === r.view;
    a.classList.toggle("active", on);
    a.setAttribute("aria-selected", String(on));
    if (r.code) {
      a.href = a.dataset.tab === "home"
        ? "#/" + encodeURIComponent(r.code)
        : "#/" + encodeURIComponent(r.code) + "/" + a.dataset.tab;
    }
  });
}

/* ---------- topbar event selector ----------
   Populated once from the source's catalog. Reflects the current route's
   event; changing it navigates to that event's home (keeping the user on
   the dashboard). Hidden when the source exposes no catalog or the catalog
   is empty (single-event or pre-first-pull). */

let catalog = null; // { events: [...], default: code }

async function loadCatalog() {
  if (typeof source.getEvents !== "function") return;
  try {
    catalog = await source.getEvents();
  } catch (e) {
    catalog = null;
    return;
  }
  if (!eventSelect) return;
  const events = Array.isArray(catalog.events) ? catalog.events : [];
  if (!events.length) {
    eventSelect.hidden = true;
    return;
  }
  eventSelect.innerHTML = "";
  events.forEach((e) => {
    const opt = document.createElement("option");
    opt.value = e.code;
    // Compact label first (shortName in the catalog, e.g. "CONF27"); the
    // full title only when no short label exists.
    opt.textContent = e.shortName || e.title || e.code;
    opt.title = e.title || e.code;
    eventSelect.appendChild(opt);
  });
  eventSelect.hidden = false;
  syncEventSelect(currentView() ? currentView().code : null);
}

function syncEventSelect(code) {
  if (!eventSelect || !catalog) return;
  const events = Array.isArray(catalog.events) ? catalog.events : [];
  if (!events.length) {
    eventSelect.hidden = true;
    return;
  }
  eventSelect.hidden = false;
  // Match by code; fall back to the catalog default, then the first event.
  const match = events.find((e) => e.code === code);
  const fallback = events.find((e) => e.code === catalog.default) || events[0];
  eventSelect.value = (match || fallback).code;
}

if (eventSelect) {
  eventSelect.addEventListener("change", () => {
    const code = eventSelect.value;
    if (!code) return;
    // Navigate to the event's home dashboard.
    if (location.hash === "#/" + encodeURIComponent(code)) route();
    else location.hash = "#/" + encodeURIComponent(code);
  });
}

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
    const r = currentView();
    const code = r ? r.code : (catalog && catalog.default) || null;
    if (!code) return; // on the events landing with no default — nothing to repull
    setRefreshing(true);
    source.refresh(code, (err) => {
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
  else if (on) offlineBanner.textContent = "Offline. Showing last fetched data";
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
          detail: "Offline. Showing last fetched data",
        })
      );
    }
  });
}

/* ---------- boot ---------- */

// Initial render: the active view for the current hash (or the events
// landing when the hash is empty). This also clears theme.js's "Loading…"
// fallback, which only appears in the child-less window before first paint
// of the app. The catalog loads in parallel to populate the selector. The
// tab bar is built before the first route so syncViewTabs has tabs to mark.
buildViewTabs();
route();
loadCatalog();
