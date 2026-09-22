/* Cvent source module — views + data access.
 *
 * The data-access functions (getEvent, getPayments, searchAttendees,
 * refresh) are the ONLY place in this file that talks to the network.
 * The views (home, attendees) are thin renderers on top of them; Task 10
 * replaced the home rendering in place (the attendees placeholder lands in
 * Task 11) and they call the same data-access functions.
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
let repullObservers = new Set(); // (running) => void — home's Re-pull button

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
// number — the same haystack the server's attendeeMatches (handlers.go)
// builds: name.* + top-level name fields + contact.name parts +
// contact.email + email + confirmationNumber, so static and live
// results agree.
function attendeeHaystack(a) {
  if (!a || typeof a !== "object") return "";
  const parts = [];
  const nameKeys = [
    "first", "middle", "last",
    "firstName", "middleName", "lastName", "givenName", "familyName",
  ];
  const name = a.name && typeof a.name === "object" ? a.name : a;
  for (const k of nameKeys) {
    if (typeof name[k] === "string") parts.push(name[k]);
  }
  if (a.contact && typeof a.contact === "object") {
    for (const k of nameKeys) {
      if (typeof a.contact[k] === "string") parts.push(a.contact[k]);
    }
    if (typeof a.contact.email === "string") parts.push(a.contact.email);
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

// Subscribe to repull-state ticks (every poll tick + start + completion).
// The home view uses this to keep its Re-pull button dimmed/labelled while a
// repull is in flight, updating on the 3s poll ticks. Returns an
// unsubscribe fn. Live mode only — static mode never repulls.
export function subscribeRepull(fn) {
  repullObservers.add(fn);
  fn(repullRunning);
  return () => repullObservers.delete(fn);
}

function notifyRepull(running) {
  repullObservers.forEach((fn) => fn(running));
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
  notifyRepull(true);
  try {
    const res = await fetch(API + "/event/repull", { method: "POST" });
    if (!res.ok) throw new Error("HTTP " + res.status);
  } catch (e) {
    repullRunning = false;
    notifyRepull(false);
    if (onComplete) onComplete(e);
    return;
  }
  pollTimer = setInterval(async () => {
    let st;
    try {
      st = await fetchJSON(API + "/event/repull-status");
    } catch (e) {
      notifyRepull(true); // transient failure — still running, keep the button dim
      return;
    }
    if (st.running) {
      notifyRepull(true); // repull still in flight on this tick
      return;
    }
    clearInterval(pollTimer);
    pollTimer = null;
    repullRunning = false;
    bustCache();
    notifyRepull(false);
    if (onComplete) onComplete(null);
  }, POLL_MS);
}

/* ---------- views (home = Task 10 dashboard; attendees placeholder until
   Task 11) ---------- */

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

// One-line empty state (the attendees placeholder until Task 11): the
// design language (card + empty-state + dynamic badges) is verifiable now.
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

/* ---------- home dashboard (Task 10) ----------
   All numbers, badges and labels come from the fetched payloads — nothing
   data-derived is hardcoded. Data strings go through esc() before innerHTML.
   Static and live share one render path (same field names); only the
   Re-pull button visibility differs (no server to repull in static mode). */

const esc = escapeHtml; // one small escape helper, per the render rules

const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

// Deterministic, locale-safe dates (fixed month names, UTC parts):
// "May 23, 2027".
function fmtDate(iso) {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "";
  const d = new Date(t);
  return MONTHS[d.getUTCMonth()] + " " + d.getUTCDate() + ", " + d.getUTCFullYear();
}

// "May 23 – 28, 2027" (same month), "May 28 – June 1, 2027" (same year),
// "Dec 30, 2026 – Jan 2, 2027" (cross-year). Single day → one date.
function fmtRange(startIso, endIso) {
  const ts = Date.parse(startIso);
  const te = Date.parse(endIso);
  if (!Number.isFinite(ts)) return fmtDate(endIso);
  if (!Number.isFinite(te)) return fmtDate(startIso);
  const s = new Date(ts);
  const e = new Date(te);
  if (ts === te) return fmtDate(startIso);
  const year = e.getUTCFullYear();
  if (s.getUTCFullYear() === year && s.getUTCMonth() === e.getUTCMonth()) {
    return MONTHS[s.getUTCMonth()] + " " + s.getUTCDate() + " – " + e.getUTCDate() + ", " + year;
  }
  if (s.getUTCFullYear() === year) {
    return (
      MONTHS[s.getUTCMonth()] + " " + s.getUTCDate() +
      " – " + MONTHS[e.getUTCMonth()] + " " + e.getUTCDate() + ", " + year
    );
  }
  return fmtDate(startIso) + " – " + fmtDate(endIso);
}

// Money: 2 decimals, thousands separators, currency code from the data
// (no symbol; when no code is known, just the number).
function money(amount, currency) {
  const n = Number(amount);
  const v = (Number.isFinite(n) ? n : 0).toFixed(2);
  const [i, d] = v.split(".");
  const neg = i.startsWith("-");
  const digits = neg ? i.slice(1) : i;
  const grouped = digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return (neg ? "-" : "") + grouped + "." + d + (currency ? " " + currency : "");
}

function card(inner) {
  return '<div class="card">' + inner + "</div>";
}

function sectionTitle(label, badgeHtml) {
  return (
    '<div class="section-head">' + escapeHtml(label) + (badgeHtml || "") + "</div>"
  );
}

// Header card: title, code, dates, format badge, stale/snapshot state,
// Re-pull (live only).
function headerCard(bundle) {
  const ev = bundle.event || {};
  const badges = [];
  if (bundle.snapshot) badges.push('<span class="badge badge-slate">Snapshot</span>');
  if (bundle.stale) badges.push('<span class="badge badge-amber">Stale</span>');
  if (ev.format) badges.push('<span class="badge badge-slate">' + esc(ev.format) + "</span>");
  const repull = bundle.snapshot
    ? ""
    : '<button class="btn" data-repull type="button">Re-pull</button>';
  return card(
    '<div class="head-row">' +
    "<div><div class=\"dash-title\">" + esc(ev.title || "Event dashboard") + "</div>" +
    '<div class="dash-sub">' +
    (bundle.code ? esc(bundle.code) : "") +
    (bundle.code && (ev.start || ev.end) ? " · " : "") +
    esc(fmtRange(ev.start, ev.end)) +
    "</div></div>" +
    '<div class="badge-row">' + badges.join("") + repull + "</div>" +
    "</div>"
  );
}

// Registrations card: big dynamic attendee count + one table row per
// registration type (code, name, capacity, Open/Full).
function registrationsCard(bundle) {
  const counts = bundle.counts || {};
  const total = Number.isFinite(counts.attendees) ? counts.attendees : 0;
  const types = Array.isArray(bundle.registrationTypes) ? bundle.registrationTypes : [];
  const countHtml =
    '<div class="dash-count">' + esc(total) +
    '<div class="dash-sub">attendees</div></div>';
  if (!types.length) {
    return card(sectionTitle("Registrations") + countHtml + '<div class="empty-state"><div class="hint">No registration types</div></div>');
  }
  const rows = types
    .map((t) => {
      const cap = t.capacity || {};
      const unlimited = cap.total == null || cap.total < 0;
      const full = !unlimited && cap.total - (cap.consumed || 0) <= 0;
      return (
        "<tr><td>" + esc(t.code) + "</td><td>" + esc(t.name) + "</td>" +
        '<td class="num">' + (unlimited ? "Unlimited" : esc(cap.total)) + "</td>" +
        "<td>" +
        (full
          ? '<span class="badge badge-red">Full</span>'
          : '<span class="badge badge-green">Open</span>') +
        "</td></tr>"
      );
    })
    .join("");
  return (
    card(sectionTitle("Registrations") + countHtml +
    '<table class="tbl"><thead><tr><th>Code</th><th>Name</th>' +
    '<th class="num">Capacity</th><th>Status</th></tr></thead><tbody>' +
    rows + "</tbody></table>")
  );
}

// Pricing card: fee items joined to their product. feeItems[i].product is
// {id, type, name}; type "AdmissionItem" links product.id into
// admissionItems (joined on id, the admission's code shown); other types
// (QuantityItem: tours, extra tickets) carry no admission — their product
// name is shown instead.
function pricingCard(bundle, currency) {
  const fees = Array.isArray(bundle.feeItems) ? bundle.feeItems : [];
  if (!fees.length) {
    return card(sectionTitle("Pricing") + '<div class="empty-state"><div class="hint">No pricing</div></div>');
  }
  const adms = Array.isArray(bundle.admissionItems) ? bundle.admissionItems : [];
  const admCode = {};
  adms.forEach((a) => { if (a.id) admCode[a.id] = a.code || a.name || ""; });
  let total = 0;
  const rows = fees
    .map((f) => {
      const amt = Number(f.amount) || 0;
      total += amt;
      const p = f.product || {};
      const item =
        p.type === "AdmissionItem" && admCode[p.id]
          ? admCode[p.id]
          : p.name || "";
      const earlyBird = Array.isArray(f.earlyBirdPricing) && f.earlyBirdPricing.length > 0;
      return (
        "<tr><td>" + esc(f.name) +
        (earlyBird ? ' <span class="badge badge-amber">Early bird</span>' : "") +
        "</td><td>" + esc(item) + "</td>" +
        '<td class="num">' + esc(money(amt, f.currency || currency)) + "</td></tr>"
      );
    })
    .join("");
  return (
    card(sectionTitle("Pricing") +
    '<table class="tbl"><thead><tr><th>Name</th><th>Item</th>' +
    '<th class="num">Amount</th></tr></thead><tbody>' +
    rows +
    '<tr><td><strong>Total</strong></td><td></td><td class="num"><strong>' +
    esc(money(total, currency)) + "</strong></td></tr>" +
    "</tbody></table>")
  );
}

// Status badge from the payment row's own status (server: paid/partial/
// unpaid), with the spec's arithmetic as fallback when the field is absent.
function orderStatusBadge(o) {
  const ordered = Number(o.amountOrdered) || 0;
  const paid = Number(o.amountPaid) || 0;
  const due = Number(o.amountDue) || 0;
  let s = o.status;
  if (s !== "paid" && s !== "partial" && s !== "unpaid") {
    s = due === 0 && paid > 0 ? "paid" : due > 0 && due < ordered ? "partial" : "unpaid";
  }
  if (s === "paid") return '<span class="badge badge-green">Paid</span>';
  if (s === "partial") return '<span class="badge badge-amber">Partial</span>';
  return '<span class="badge badge-red">Unpaid</span>';
}

// Payments card: totals (ordered/paid/due/refunded) + order table capped at
// 20 rows with a "Show all (N)" expander; zero orders → empty state; a
// non-empty cancelled array is a muted count line, never tabulated.
function paymentsCard(payments, currency) {
  const t = (payments && payments.totals) || {};
  const totals =
    '<table class="tbl"><tbody>' +
    "<tr><td>Ordered</td><td class=\"num\">" + esc(money(t.ordered, currency)) + "</td></tr>" +
    "<tr><td>Paid</td><td class=\"num\">" + esc(money(t.paid, currency)) + "</td></tr>" +
    "<tr><td>Due</td><td class=\"num\">" + esc(money(t.due, currency)) + "</td></tr>" +
    "<tr><td>Refunded</td><td class=\"num\">" + esc(money(t.refunded, currency)) + "</td></tr>" +
    "</tbody></table>";
  const orders = Array.isArray(payments && payments.orders) ? payments.orders : [];
  const cancelled = Array.isArray(payments && payments.cancelled) ? payments.cancelled : [];
  let body;
  if (!orders.length) {
    body = '<div class="empty-state"><div class="hint">No registrations yet</div></div>';
  } else {
    const rows = orders.map((o) =>
      "<tr><td>" + esc(o.attendee) + "</td><td>" + esc(o.invoice) + "</td>" +
      '<td class="num">' + esc(money(o.amountOrdered, currency)) + "</td>" +
      '<td class="num">' + esc(money(o.amountPaid, currency)) + "</td>" +
      '<td class="num">' + esc(money(o.amountDue, currency)) + "</td>" +
      "<td>" + esc(o.method) + "</td><td>" + orderStatusBadge(o) + "</td></tr>"
    );
    const CAP = 20;
    const visible = rows.slice(0, CAP).join("");
    const rest = rows.slice(CAP);
    const more = rest.length
      ? '<tbody data-more hidden>' + rest.join("") + "</tbody>" +
        '<button class="expander" data-expand type="button">Show all (' +
        esc(orders.length) + ")</button>"
      : "";
    body =
      '<table class="tbl"><thead><tr><th>Attendee</th><th>Invoice</th>' +
      '<th class="num">Ordered</th><th class="num">Paid</th><th class="num">Due</th>' +
      "<th>Method</th><th>Status</th></tr></thead><tbody>" + visible +
      "</tbody>" + more + "</table>";
  }
  const cancelledLine = cancelled.length
    ? '<div class="muted-line">' +
      esc(cancelled.length) + " cancelled orders</div>"
    : "";
  return card(sectionTitle("Payments") + totals + body + cancelledLine);
}

// Program card: sessions count + speakers (name, affiliation) capped at 50
// with a "Show all (N)" expander; nothing at all → empty state.
function programCard(bundle) {
  const sessions = Array.isArray(bundle.sessions) ? bundle.sessions : [];
  const speakers = Array.isArray(bundle.speakers) ? bundle.speakers : [];
  const countBadge =
    ' <span class="badge badge-slate">' + esc(sessions.length) +
    " sessions · " + esc(speakers.length) + " speakers</span>";
  if (!sessions.length && !speakers.length) {
    return card(sectionTitle("Program") + '<div class="empty-state"><div class="hint">No program yet</div></div>');
  }
  let inner = sectionTitle("Program", countBadge);
  const CAP = 50;
  const rows = speakers.map((s) => {
    const name = [s.firstName, s.middleName, s.lastName].filter(Boolean).join(" ");
    return "<tr><td>" + esc(name) + "</td><td>" + esc(s.company) + "</td></tr>";
  });
  if (rows.length) {
    const visible = rows.slice(0, CAP).join("");
    const rest = rows.slice(CAP);
    inner +=
      '<table class="tbl"><thead><tr><th>Name</th><th>Affiliation</th></tr></thead><tbody>' +
      visible + "</tbody>" +
      (rest.length
        ? '<tbody data-more hidden>' + rest.join("") + "</tbody>" +
          '<button class="expander" data-expand type="button">Show all (' +
          esc(rows.length) + ")</button>"
        : "") +
      "</table>";
  }
  return card(inner);
}

function dashboardHtml(bundle, payments) {
  // Currency: the fee items' code first, then the event's (both "USD" for
  // CONF27); absent → money() renders a bare number.
  const fee = Array.isArray(bundle.feeItems) ? bundle.feeItems : [];
  const currency =
    (fee[0] && fee[0].currency) || (bundle.event && bundle.event.currency) || "";
  return (
    headerCard(bundle) +
    registrationsCard(bundle) +
    pricingCard(bundle, currency) +
    paymentsCard(payments, currency) +
    programCard(bundle)
  );
}

// Wires the rendered dashboard: the Re-pull button (live mode) tracks the
// module's repull state on the 3s poll ticks, and the "Show all" expanders
// reveal their hidden rows in place. Returns nothing; the repull
// unsubscribe is stashed on the mount so the next load can drop it.
function wireDashboard(mount, bundle) {
  // "Show all (N)" expanders: reveal the hidden <tbody data-more> of the
  // button's own card, in place.
  mount.querySelectorAll("[data-expand]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const scope = btn.closest(".card") || mount;
      const more = scope.querySelector("[data-more]");
      if (more) more.removeAttribute("hidden");
      btn.remove();
    });
  });
  if (bundle.snapshot) return;
  const btn = mount.querySelector("[data-repull]");
  if (!btn) return;
  let wasPulling = false;
  mount.__unsubRepull = subscribeRepull((running) => {
    btn.disabled = running;
    btn.textContent = running ? "Pulling…" : "Re-pull";
    if (running && !wasPulling) {
      wasPulling = true;
    } else if (!running && wasPulling) {
      wasPulling = false;
      // Repull just finished: re-fetch both payloads, re-render in place
      // (only while this view is still the current route).
      const h = (location.hash || "").replace(/^#/, "") || "/";
      if (h === "/") loadHome(mount);
    }
  });
  btn.addEventListener("click", () => refresh(null));
}

// Drop the repull observer stashed by wireDashboard (avoids accumulating
// observers across re-entries: hashchange, retry, repull completion).
function unwireDashboard(mount) {
  if (typeof mount.__unsubRepull === "function") {
    mount.__unsubRepull();
    mount.__unsubRepull = null;
  }
}

export function home(mount) {
  unwireDashboard(mount);
  loadHome(mount);
}

function loadHome(mount) {
  unwireDashboard(mount);
  mount.innerHTML = skeletonHtml(5);
  Promise.all([getEvent(), getPayments()])
    .then(([bundle, payments]) => {
      mount.innerHTML = dashboardHtml(bundle, payments);
      wireDashboard(mount, bundle);
    })
    .catch((e) => {
      const msg = (e && e.message) ? e.message : "unknown error";
      mount.innerHTML = card(
        '<div class="empty-state">' +
        '<div class="title">Couldn&#39;t load the event</div>' +
        '<div class="hint" data-err></div>' +
        '<button class="btn" data-retry type="button">Retry</button>' +
        "</div>"
      );
      const hint = mount.querySelector("[data-err]");
      if (hint) hint.textContent = msg;
      const retry = mount.querySelector("[data-retry]");
      if (retry) retry.addEventListener("click", () => loadHome(mount));
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
