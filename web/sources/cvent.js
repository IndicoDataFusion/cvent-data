/* Cvent source module — views + data access.
 *
 * The data-access functions (getEvent, getPayments, searchAttendees,
 * refresh) are the ONLY place in this file that talks to the network.
 * The views (home, attendees) are thin renderers on top of them — Tasks
 * 10-11 replace the view rendering logic IN PLACE and call the same
 * data-access functions.
 *
 * Static mode (?static=1 or *.github.io): the data-access functions read
 * the --dump snapshot files from staticDataPath instead of the live API
 * and mark results with `snapshot: true` so the UI can show a badge.
 * The event code comes from the server payload (live) or the index.json
 * entry (static) — it is never hardcoded in JS.
 */

const API = "/api/cvent";
export const staticDataPath = "data/";
const STALE_MS = 24 * 60 * 60 * 1000; // server's read-time staleness rule
const POLL_MS = 3000;                 // repull-status poll interval

// bundle key → dump file name (the --dump layout; see server/dump.go).
const RESOURCE_FILES = {
  registrationTypes: "registration-types.json",
  admissionItems: "admission-items.json",
  feeItems: "fee-items.json",
  discounts: "discounts.json",
  sessions: "sessions.json",
  speakers: "speakers.json",
  attendees: "attendees.json",
  activities: "activities.json",
  orders: "orders.json",
  orderItems: "order-items.json",
  transactions: "transactions.json",
  transactionItems: "transaction-items.json",
};
const RESOURCES = Object.keys(RESOURCE_FILES);

const STATIC =
  (typeof location !== "undefined" &&
    new URLSearchParams(location.search || "").has("static")) ||
  (typeof location !== "undefined" &&
    (location.hostname || "").endsWith(".github.io"));

/* ---------- module state ---------- */

let eventCode = null;        // from the server payload / index.json entry
let staticBundle = null;     // static-mode getEvent() cache
let staticPayments = null;   // static-mode getPayments() cache
let pollTimer = null;        // repull-status poll
let repullRunning = false;

// Drop client-side static caches. Live mode needs no client cache: the
// server's 15-min cache (busted by the repull) is the single source of
// freshness, so a plain re-fetch is always correct.
function bustCache() {
  staticBundle = null;
  staticPayments = null;
}

/* ---------- data access ---------- */

async function fetchJSON(url) {
  const res = await fetch(url, { cache: "no-store" });
  if (!res.ok) throw new Error("HTTP " + res.status + " for " + url);
  return res.json();
}

// A dump snapshot older than 24h is stale — the same read-time rule the
// server applies (meta.json carries no stale flag; pulledAt drives it).
function isStale(pulledAt) {
  if (!pulledAt) return false;
  const t = new Date(pulledAt).getTime();
  return Number.isFinite(t) && Date.now() - t > STALE_MS;
}

// Assembles the bundle-shaped object from the dump files: same field
// names as the API (code, pulledAt, stale, event, counts, the 12 raw
// resource arrays, errors) plus the snapshot marker. Empty resources are
// literal null on disk and are OMITTED from the bundle, exactly like the
// live API omits them; failed resources are error objects on disk and
// surface via meta.json's errors map.
function assembleStaticBundle(entry, meta, event, raws) {
  const bundle = {
    code: entry.code,
    pulledAt: meta.pulledAt,
    stale: isStale(meta.pulledAt),
    event: event,
    counts: meta.counts || {},
    snapshot: true,
  };
  if (meta.errors && Object.keys(meta.errors).length) bundle.errors = meta.errors;
  RESOURCES.forEach((k, i) => {
    if (Array.isArray(raws[i])) bundle[k] = raws[i];
  });
  return bundle;
}

async function getEventStatic() {
  if (staticBundle) return staticBundle;
  const idx = await fetchJSON(staticDataPath + "index.json");
  const entry = Array.isArray(idx) ? idx[0] : idx; // exactly one entry
  if (!entry || !entry.code) throw new Error("static index has no entry");
  eventCode = entry.code;
  const dir = staticDataPath + entry.code + "/";
  const [meta, event, ...raws] = await Promise.all([
    fetchJSON(dir + "meta.json"),
    fetchJSON(dir + "event.json"),
    ...RESOURCES.map((k) => fetchJSON(dir + RESOURCE_FILES[k])),
  ]);
  staticBundle = assembleStaticBundle(entry, meta, event, raws);
  return staticBundle;
}

// The full event bundle (live: /api/cvent/event; static: dump files).
export async function getEvent() {
  if (STATIC) return getEventStatic();
  const b = await fetchJSON(API + "/event");
  if (b.code) eventCode = b.code;
  return b;
}

// Payment summary (live: /api/cvent/event/payments; static: a precomputed
// payments.json in the event dir if the dump wrote one — v1 dumps do not,
// so a zeroed summary with the snapshot marker is the graceful fallback).
export async function getPayments() {
  if (STATIC) {
    if (staticPayments) return staticPayments;
    if (!eventCode) await getEventStatic();
    try {
      const p = await fetchJSON(staticDataPath + eventCode + "/payments.json");
      p.snapshot = true;
      staticPayments = p;
      return p;
    } catch (e) {
      staticPayments = {
        totals: { ordered: 0, paid: 0, due: 0, refunded: 0 },
        orders: [],
        cancelled: [],
        snapshot: true,
      };
      return staticPayments;
    }
  }
  return fetchJSON(API + "/event/payments");
}

// Case-insensitive substring over name parts + email + confirmation
// number — the same haystack the server's /attendees search uses, so
// static and live results agree.
function attendeeHaystack(a) {
  if (!a || typeof a !== "object") return "";
  const parts = [];
  const name = a.name && typeof a.name === "object" ? a.name : a;
  for (const k of ["first", "middle", "last", "firstName", "middleName", "lastName", "givenName", "familyName"]) {
    if (typeof name[k] === "string") parts.push(name[k]);
  }
  for (const k of ["email", "confirmationNumber"]) {
    if (typeof a[k] === "string") parts.push(a[k]);
  }
  return parts.join(" ").toLowerCase();
}

// Attendee search (live: /api/cvent/event/attendees?q=&limit=&offset=;
// static: the same filter run over the dumped attendees array).
export async function searchAttendees(q, limit, offset) {
  if (STATIC) {
    const b = await getEventStatic();
    const all = Array.isArray(b.attendees) ? b.attendees : [];
    const needle = String(q || "").toLowerCase().trim();
    const matched = needle ? all.filter((a) => attendeeHaystack(a).includes(needle)) : all;
    const lim = Math.min(Math.max(Number(limit) || 50, 0), 200);
    const off = Math.max(Number(offset) || 0, 0);
    return {
      total: matched.length,
      offset: off,
      limit: lim,
      items: matched.slice(off, off + lim),
      snapshot: true,
    };
  }
  const params = new URLSearchParams();
  if (q) params.set("q", q);
  if (limit != null) params.set("limit", String(limit));
  if (offset != null) params.set("offset", String(offset));
  const qs = params.toString();
  return fetchJSON(API + "/event/attendees" + (qs ? "?" + qs : ""));
}

/* ---------- repull (live mode only) ---------- */

// True while a background repull is in flight (so a Re-pull button can
// dim itself — Task 10).
export function isRepulling() {
  return repullRunning;
}

// Triggers the server repull (POST /api/cvent/event/repull) and polls
// /api/cvent/event/repull-status every POLL_MS while running, stopping
// when running is false. onComplete(null) fires when the repull finishes
// (caches busted first) or onComplete(err) when the trigger itself
// failed. Static mode is a no-op — there is no live source to repull.
export async function refresh(onComplete) {
  if (STATIC) {
    if (onComplete) onComplete(null);
    return;
  }
  if (repullRunning) return; // a repull is already in flight
  repullRunning = true;
  try {
    const res = await fetch(API + "/event/repull", { method: "POST" });
    if (!res.ok) throw new Error("HTTP " + res.status);
  } catch (e) {
    repullRunning = false;
    if (onComplete) onComplete(e);
    return;
  }
  pollTimer = setInterval(async () => {
    let st;
    try {
      st = await fetchJSON(API + "/event/repull-status");
    } catch (e) {
      return; // transient failure — keep polling
    }
    if (!st.running) {
      clearInterval(pollTimer);
      pollTimer = null;
      repullRunning = false;
      bustCache();
      if (onComplete) onComplete(null);
    }
  }, POLL_MS);
}

/* ---------- views (placeholders — Tasks 10-11 render in place) ---------- */

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

const ICON =
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
  'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' +
  '<rect width="18" height="18" x="3" y="4" rx="2"/><path d="M16 2v4"/><path d="M8 2v4"/>' +
  '<path d="M3 10h18"/></svg>';

// The ONLY loading indicator: skeleton rows (styles.css .skeleton).
function skeletonHtml(rows) {
  const rs = Array.from({ length: rows }, () => '<div class="skeleton-row"></div>').join("");
  return '<div class="card"><div class="skeleton">' + rs + "</div></div>";
}

// One-line empty state after the data access resolves: the design language
// (card + empty-state + dynamic badges) is verifiable now even though the
// real rendering lands in the next task.
function placeholderHtml(titleHtml, bundle) {
  const badges =
    (bundle && bundle.snapshot ? '<span class="badge badge-slate">Snapshot</span>' : "") +
    (bundle && bundle.stale ? '<span class="badge badge-amber">Stale</span>' : "");
  return (
    '<div class="card"><div class="empty-state">' +
    '<div class="icon">' + ICON + "</div>" +
    '<div class="title">' + titleHtml + badges + "</div>" +
    '<div class="hint">This view lands in the next task.</div>' +
    "</div></div>"
  );
}

// Home view (dashboard — real rendering in Task 10).
export function home(mount) {
  mount.innerHTML = skeletonHtml(3);
  getEvent()
    .then((b) => {
      mount.innerHTML = placeholderHtml(
        escapeHtml((b && b.event && b.event.title) || "Event dashboard"), b);
    })
    .catch(() => {
      mount.innerHTML = placeholderHtml("Couldn't load the event", null);
    });
}

// Attendees view (search + detail sheet — real rendering in Task 11).
export function attendees(mount) {
  mount.innerHTML = skeletonHtml(5);
  getEvent()
    .then((b) => searchAttendees("", 1, 0).then((r) => ({ b, total: r.total })))
    .then(({ b, total }) => {
      const title = escapeHtml((b && b.event && b.event.title) || "Attendees");
      // Dynamic count badge (never hardcoded markup): the total from the
      // data access, so a zero-row event reads "0" — CONF27's real state.
      const count = Number.isFinite(total) && total >= 0
        ? ' <span class="badge badge-slate">' + total + "</span>"
        : "";
      mount.innerHTML = placeholderHtml(title + count, b);
    })
    .catch(() => {
      mount.innerHTML = placeholderHtml("Couldn't load attendees", null);
    });
}
