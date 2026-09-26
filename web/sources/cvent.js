/* Cvent source module — views + data access.
 *
 * The data-access functions (getEvents, getEvent, getPayments,
 * searchAttendees, refresh) are the ONLY place in this file that talks to
 * the network. The views (events, home, attendees) are thin renderers on
 * top of them; they call the same data-access functions.
 *
 * Multi-event: every data-access call takes the event code explicitly —
 * the shell's router (app.js) reads it from the hash (#/<code>/<view>).
 * The code is never hardcoded in JS.
 *
 * Static mode (?static=1 or *.github.io): the data-access functions read
 * the --dump snapshot files from staticDataPath instead of the live API
 * and mark results with `snapshot: true` so the UI can show a badge.
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
  eventQuestions: "event-questions.json",
};
const RESOURCES = Object.keys(RESOURCE_FILES);
// Resources older dumps may lack: a missing file loads as "no rows" instead
// of failing the whole snapshot.
const OPTIONAL_RESOURCES = new Set(["eventQuestions"]);

const STATIC =
  (typeof location !== "undefined" &&
    new URLSearchParams(location.search || "").has("static")) ||
  (typeof location !== "undefined" &&
    (location.hostname || "").endsWith(".github.io"));

/* ---------- module state ---------- */

// Static-mode caches, keyed by event code (the site serves multiple events).
const staticBundles = new Map();  // code -> getEvent() result
const staticPayments = new Map(); // code -> getPayments() result
let pollTimer = null;        // repull-status poll
let repullRunning = false;

// Drop client-side static caches. Live mode needs no client cache: the
// server's 15-min cache (busted by the repull) is the single source of
// freshness, so a plain re-fetch is always correct.
function bustCache() {
  staticBundles.clear();
  staticPayments.clear();
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

async function getEventStatic(code) {
  if (staticBundles.has(code)) return staticBundles.get(code);
  const idx = await fetchJSON(staticDataPath + "index.json");
  const entries = Array.isArray(idx) ? idx : [idx];
  const entry = entries.find((e) => e && e.code === code) || entries[0];
  if (!entry || !entry.code) throw new Error("static index has no entry for " + code);
  const dir = staticDataPath + entry.code + "/";
  const [meta, event, ...raws] = await Promise.all([
    fetchJSON(dir + "meta.json"),
    fetchJSON(dir + "event.json"),
    ...RESOURCES.map((k) => {
      const p = fetchJSON(dir + RESOURCE_FILES[k]);
      return OPTIONAL_RESOURCES.has(k) ? p.catch(() => null) : p;
    }),
  ]);
  const bundle = assembleStaticBundle(entry, meta, event, raws);
  staticBundles.set(bundle.code, bundle);
  return bundle;
}

// The site's event list (live: /api/cvent/events; static: the dump's
// index.json). Each entry: {code, title, start, end, pulledAt}.
export async function getEvents() {
  if (STATIC) {
    const idx = await fetchJSON(staticDataPath + "index.json");
    const entries = Array.isArray(idx) ? idx : [idx];
    return { events: entries.filter((e) => e && e.code), default: entries[0] ? entries[0].code : "" };
  }
  return fetchJSON(API + "/events");
}

// The full event bundle for code (live: /api/cvent/events/<code>;
// static: that code's dump files).
export async function getEvent(code) {
  if (STATIC) return getEventStatic(code);
  return fetchJSON(API + "/events/" + encodeURIComponent(code));
}

// Payment summary for code (live: /api/cvent/events/<code>/payments;
// static: a precomputed payments.json in the event dir if the dump wrote
// one — v1 dumps do not, so a zeroed summary with the snapshot marker is
// the graceful fallback).
export async function getPayments(code) {
  if (STATIC) {
    if (staticPayments.has(code)) return staticPayments.get(code);
    try {
      const p = await fetchJSON(staticDataPath + code + "/payments.json");
      p.snapshot = true;
      staticPayments.set(code, p);
      return p;
    } catch (e) {
      const p = {
        totals: { ordered: 0, paid: 0, due: 0, refunded: 0 },
        orders: [],
        cancelled: [],
        snapshot: true,
      };
      staticPayments.set(code, p);
      return p;
    }
  }
  return fetchJSON(API + "/events/" + encodeURIComponent(code) + "/payments");
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

// question id → {text, type} for the questions these attendees answered —
// the static-mode twin of the server's answeredQuestions (handlers.go).
function answeredQuestions(eventQuestions, items) {
  const byId = new Map();
  (Array.isArray(eventQuestions) ? eventQuestions : []).forEach((q) => {
    if (q && q.id) byId.set(q.id, { text: q.text || "", type: q.type || "" });
  });
  const out = {};
  items.forEach((a) => {
    ((a && a.answers) || []).forEach((ans) => {
      const id = ans && ans.question && ans.question.id;
      if (id && byId.has(id)) out[id] = byId.get(id);
    });
  });
  return out;
}

// Attendee search for code (live: /api/cvent/events/<code>/attendees
// ?q=&limit=&offset=; static: the same filter run over the dumped
// attendees array).
export async function searchAttendees(code, q, limit, offset) {
  if (STATIC) {
    const b = await getEventStatic(code);
    const all = Array.isArray(b.attendees) ? b.attendees : [];
    const needle = String(q || "").toLowerCase().trim();
    const matched = needle ? all.filter((a) => attendeeHaystack(a).includes(needle)) : all;
    const lim = Math.min(Math.max(Number(limit) || 50, 0), 200);
    const off = Math.max(Number(offset) || 0, 0);
    const items = matched.slice(off, off + lim);
    return {
      total: matched.length,
      offset: off,
      limit: lim,
      items: items,
      questions: answeredQuestions(b.eventQuestions, items),
      snapshot: true,
    };
  }
  const params = new URLSearchParams();
  if (q) params.set("q", q);
  if (limit != null) params.set("limit", String(limit));
  if (offset != null) params.set("offset", String(offset));
  const qs = params.toString();
  return fetchJSON(API + "/events/" + encodeURIComponent(code) + "/attendees" + (qs ? "?" + qs : ""));
}

/* ---------- repull (live mode only) ---------- */

// True while a background repull is in flight (so the topbar Refresh button
// can disable itself — see app.js).
export function isRepulling() {
  return repullRunning;
}

// Triggers the server repull for code (POST /api/cvent/events/<code>/
// repull) and polls its repull-status every POLL_MS while running,
// stopping when running is false. onComplete(null) fires when the repull
// finishes (caches busted first) or onComplete(err) when the trigger
// itself failed. Static mode is a no-op — there is no live source to
// repull.
export async function refresh(code, onComplete) {
  if (STATIC) {
    if (onComplete) onComplete(null);
    return;
  }
  if (repullRunning) return; // a repull is already in flight
  repullRunning = true;
  const base = API + "/events/" + encodeURIComponent(code);
  try {
    const res = await fetch(base + "/repull", { method: "POST" });
    if (!res.ok) throw new Error("HTTP " + res.status);
  } catch (e) {
    repullRunning = false;
    if (onComplete) onComplete(e);
    return;
  }
  pollTimer = setInterval(async () => {
    let st;
    try {
      st = await fetchJSON(base + "/repull-status");
    } catch (e) {
      return; // transient failure — still running, try again next tick
    }
    if (st.running) return; // repull still in flight on this tick
    clearInterval(pollTimer);
    pollTimer = null;
    repullRunning = false;
    bustCache();
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

/* ---------- toast (Task 11; styles.css .toast — no helper existed before) ----------

   One toast element at a time (replaced per call, auto-dismissed). Used by
   the attendee check-in (success + error paths). */
function toastMsg(msg) {
  const old = document.getElementById("cvent-toast");
  if (old) old.remove();
  const t = document.createElement("div");
  t.id = "cvent-toast";
  t.className = "toast";
  t.textContent = String(msg || "Error");
  document.body.appendChild(t);
  t.classList.add("show"); // opacity transition in (styles.css .toast.show)
  setTimeout(() => {
    t.classList.remove("show");
    setTimeout(() => t.remove(), 300);
  }, 2600);
}

/* ---------- home dashboard (Task 10) ----------
   All numbers, badges and labels come from the fetched payloads — nothing
   data-derived is hardcoded. Data strings go through esc() before innerHTML.
   Static and live share one render path (same field names); refresh lives in
   the topbar (app.js), so the views carry no repull controls. */

const esc = escapeHtml; // one small escape helper, per the render rules

const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

// "May 23, 2027" (full month) — used where space allows.
function fmtDate(iso) {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "";
  const d = new Date(t);
  return MONTHS[d.getUTCMonth()] + " " + d.getUTCDate() + ", " + d.getUTCFullYear();
}

const MONTHS_SHORT = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];

// "Feb 27, 2027" — compact form for tight cells (early-bird deadlines).
function fmtDateShort(iso) {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "";
  const d = new Date(t);
  return MONTHS_SHORT[d.getUTCMonth()] + " " + d.getUTCDate() + ", " + d.getUTCFullYear();
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
    return MONTHS[s.getUTCMonth()] + " " + s.getUTCDate() + " - " + e.getUTCDate() + ", " + year;
  }
  if (s.getUTCFullYear() === year) {
    return (
      MONTHS[s.getUTCMonth()] + " " + s.getUTCDate() +
      " - " + MONTHS[e.getUTCMonth()] + " " + e.getUTCDate() + ", " + year
    );
  }
  return fmtDate(startIso) + " - " + fmtDate(endIso);
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

// money() that drops the cents when they are zero — for whole-dollar price
// tables where "1,300" reads better and narrower than "1,300.00".
function money0(amount, currency) {
  const n = Number(amount);
  if (!Number.isFinite(n)) return money(0, currency);
  const neg = n < 0;
  const intPart = Math.floor(Math.abs(n));
  const cents = Math.round((Math.abs(n) - intPart) * 100);
  const grouped = String(intPart).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const body = cents === 0
    ? grouped
    : grouped + "." + String(cents).padStart(2, "0");
  return (neg ? "-" : "") + body + (currency ? " " + currency : "");
}

function card(inner) {
  return '<div class="card">' + inner + "</div>";
}

function sectionTitle(label, badgeHtml) {
  return (
    '<div class="section-head">' + escapeHtml(label) + (badgeHtml || "") + "</div>"
  );
}

/* ---------- collapsible sections ----------
   The dashboard's data sections (Registrations, Pricing, Payments, Program)
   collapse under their section head, which is a <button> (44px touch
   target, keyboard-operable, aria-expanded). The open/closed state
   persists per event in localStorage (cvent-data-sec-<code>). The event
   header card is never collapsible. */

function secStorageKey(code) {
  return "cvent-data-sec-" + code;
}

function isSecCollapsed(code, key) {
  try {
    const s = JSON.parse(localStorage.getItem(secStorageKey(code)) || "{}");
    return s[key] === true;
  } catch (e) {
    return false;
  }
}

function setSecCollapsed(code, key, collapsed) {
  let s = {};
  try {
    s = JSON.parse(localStorage.getItem(secStorageKey(code)) || "{}");
  } catch (e) {
    /* fresh state */
  }
  s[key] = collapsed;
  try {
    localStorage.setItem(secStorageKey(code), JSON.stringify(s));
  } catch (e) {
    /* storage unavailable — state just won't persist */
  }
}

// A card whose section head toggles its body. key is the stable section
// id ("registrations", "pricing", …) used for the persisted state. The
// head is the house-style .cat-head look: 3px accent rail, Space Grotesk
// uppercase label, an SVG chevron at the right edge (dim, rotates -90°
// when collapsed). Hover changes the label color to --text (no
// background); there is no border-radius so the rail stays a straight
// line.
function collapsibleCard(code, key, label, badgeHtml, bodyHtml) {
  const collapsed = isSecCollapsed(code, key);
  return (
    '<div class="card sec-card' + (collapsed ? " collapsed" : "") + '">' +
    '<button class="section-head sec-toggle" type="button" data-sec="' + esc(key) + '"' +
    ' aria-expanded="' + String(!collapsed) + '">' +
    '<span class="sec-label">' + escapeHtml(label) + (badgeHtml || "") + "</span>" +
    '<span class="chev" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" ' +
    'stroke="currentColor" stroke-width="2" stroke-linecap="round" ' +
    'stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg></span></button>' +
    '<div class="sec-body">' + bodyHtml + "</div>" +
    "</div>"
  );
}

// Header card: title, code, dates, format badge, stale/snapshot state.
// (Refresh lives in the topbar — app.js — not here.)
function headerCard(bundle) {
  const ev = bundle.event || {};
  const counts = bundle.counts || {};
  const total = Number.isFinite(counts.attendees) ? counts.attendees : null;
  const badges = [];
  if (bundle.snapshot) badges.push('<span class="badge badge-slate">Snapshot</span>');
  if (bundle.stale) badges.push('<span class="badge badge-amber">Stale</span>');
  if (total != null) badges.push('<span class="badge badge-slate">' + esc(total) + " registered</span>");
  return card(
    '<div class="head-row">' +
    "<div><div class=\"dash-title\">" + esc(ev.title || "Event dashboard") + "</div>" +
    '<div class="dash-sub">' +
    (bundle.code ? esc(bundle.code) : "") +
    (bundle.code && (ev.start || ev.end) ? " · " : "") +
    esc(fmtRange(ev.start, ev.end)) +
    "</div></div>" +
    '<div class="badge-row">' + badges.join("") + "</div>" +
    "</div>"
  );
}

// One registration-type row (name, code, capacity, Open/Full). Every cell
// carries a data-val sort key: strings as text, capacity as a number
// (unlimited → a value larger than any finite total), status as 0/1.
function regRow(t) {
  const cap = t.capacity || {};
  const unlimited = cap.total == null || cap.total < 0;
  const full = !unlimited && cap.total - (cap.consumed || 0) <= 0;
  return (
    "<tr>" +
    '<td data-val="' + esc(t.name || "") + '">' + esc(t.name) + "</td>" +
    '<td data-val="' + esc(t.code || "") + '">' + esc(t.code) + "</td>" +
    '<td class="num" data-val="' + (unlimited ? 999999999 : cap.total) + '">' +
    (unlimited ? "Unlimited" : esc(cap.total)) + "</td>" +
    '<td data-val="' + (full ? 1 : 0) + '">' +
    (full
      ? '<span class="badge badge-red">Full</span>'
      : '<span class="badge badge-green">Open</span>') +
    "</td></tr>"
  );
}

// Sortable column header. type: "str" (locale compare) or "num".
function sortableTh(label, numeric) {
  return (
    '<th data-sort' + (numeric ? ' class="num"' : "") +
    ' data-type="' + (numeric ? "num" : "str") + '" tabindex="0">' +
    label + '<span class="sort-ind" aria-hidden="true">&#8597;</span></th>'
  );
}

// Registrations card: big dynamic attendee count + one table row per
// registration type (name, code, capacity, Open/Full). Columns sort on
// header tap (asc → desc → original order).
function registrationsCard(bundle) {
  const code = bundle.code || "";
  const counts = bundle.counts || {};
  const total = Number.isFinite(counts.attendees) ? counts.attendees : 0;
  const types = Array.isArray(bundle.registrationTypes) ? bundle.registrationTypes : [];
  const named = types.filter((t) => t.code || t.name); // skip blank placeholder
  const countHtml =
    '<div class="dash-count">' + esc(total) +
    '<div class="dash-sub">attendees</div></div>';
  if (!named.length) {
    return collapsibleCard(code, "registrations", "Registrations", "",
      countHtml + '<div class="empty-state"><div class="hint">No registration types</div></div>');
  }
  return collapsibleCard(code, "registrations", "Registrations", "",
    countHtml +
    '<div class="tbl-wrap"><table class="tbl sortable"><thead><tr>' +
    sortableTh("Name") + sortableTh("Code") +
    sortableTh("Capacity", true) + sortableTh("Status") +
    "</tr></thead><tbody>" +
    named.map(regRow).join("") + "</tbody></table></div>");
}

// Per registration type, its primary (type-specific) admission fee. A fee
// item applies to one or more registration types (fee.registrationTypes is
// a list of type ids); the type's own standard price is the AdmissionItem
// linked to the FEWEST types — the generic "All Registrants" fee is shared
// across nearly every type, so it is never any single type's price. Among
// candidates we prefer a displayed fee, then break ties by higher amount.
// Returns null for a type that has no admission fee.
function primaryFeeForType(bundle) {
  const fees = Array.isArray(bundle.feeItems) ? bundle.feeItems : [];
  const rts = Array.isArray(bundle.registrationTypes) ? bundle.registrationTypes : [];
  const links = {}; // fee id -> number of registration types it applies to
  const feesByRt = {}; // rt id -> [fee]
  rts.forEach((r) => { feesByRt[r.id] = []; });
  fees.forEach((f) => {
    const ids = Array.isArray(f.registrationTypes) ? f.registrationTypes : [];
    links[f.id] = ids.length;
    ids.forEach((id) => { if (feesByRt[id]) feesByRt[id].push(f); });
  });
  return (rtId) => {
    const fl = feesByRt[rtId] || [];
    let adm = fl.filter((f) => (f.product || {}).type === "AdmissionItem" && f.display);
    if (!adm.length) adm = fl.filter((f) => (f.product || {}).type === "AdmissionItem");
    if (!adm.length) return null;
    return adm.slice().sort((a, b) =>
      (links[a.id] - links[b.id]) || ((Number(b.amount) || 0) - (Number(a.amount) || 0))
    )[0];
  };
}

// Pricing card: one row per registration type — its standard admission fee
// and, when the fee carries an early-bird tier, the early-bird amount. When
// every early-bird deadline is identical, one "Early bird through …" note
// sits above the table and the cells carry amounts only; mixed deadlines
// keep their per-row "by <date>". Columns sort on header tap. The blank
// placeholder type is skipped; a type with no fee shows "—" (sorts last).
function pricingCard(bundle, currency) {
  const code = bundle.code || "";
  const rts = Array.isArray(bundle.registrationTypes) ? bundle.registrationTypes : [];
  const cur = (bundle.event && bundle.event.currency) || currency || "";
  const curBadge = cur ? ' <span class="badge badge-slate">' + esc(cur) + "</span>" : "";
  if (!rts.length || !(Array.isArray(bundle.feeItems) && bundle.feeItems.length)) {
    return collapsibleCard(code, "pricing", "Pricing", curBadge,
      '<div class="empty-state"><div class="hint">No pricing</div></div>');
  }
  const primaryFee = primaryFeeForType(bundle);
  // State the currency once (fees all share the event currency); cells then
  // show bare numbers so three columns fit a phone.
  const named = rts.filter((t) => t.name || t.code); // skip blank placeholder
  if (!named.length) {
    return collapsibleCard(code, "pricing", "Pricing", curBadge,
      '<div class="empty-state"><div class="hint">No pricing</div></div>');
  }
  // Resolve each type's fee/early-bird once; detect a uniform deadline.
  const data = named.map((t) => {
    const fee = primaryFee(t.id);
    const stdOk = !!(fee && Number.isFinite(Number(fee.amount)));
    const eb = fee && Array.isArray(fee.earlyBirdPricing) && fee.earlyBirdPricing.length
      ? fee.earlyBirdPricing[0]
      : null;
    const ebOk = !!(eb && Number.isFinite(Number(eb.amount)));
    return { t, fee, stdOk, eb, ebOk };
  });
  const dates = [...new Set(data.filter((d) => d.ebOk && d.eb.registerByDate).map((d) => d.eb.registerByDate))];
  const uniformDate = dates.length === 1 ? dates[0] : null;
  const body = data.map((d) => {
    const { t, fee, stdOk, eb, ebOk } = d;
    const std = stdOk ? esc(money0(fee.amount)) : '<span class="muted-line">n/a</span>';
    const ebCell = ebOk
      ? '<div class="eb-amount">' + esc(money0(eb.amount)) + "</div>" +
        (!uniformDate && eb.registerByDate
          ? '<div class="muted-line">by ' + esc(fmtDateShort(eb.registerByDate)) + "</div>"
          : "")
      : '<span class="muted-line">n/a</span>';
    return (
      "<tr>" +
      '<td data-val="' + esc(t.name || t.code || "") + '">' + esc(t.name || t.code) + "</td>" +
      '<td class="num" data-val="' + (stdOk ? fee.amount : -1) + '">' + std + "</td>" +
      '<td class="num" data-val="' + (ebOk ? eb.amount : -1) + '">' + ebCell + "</td></tr>"
    );
  }).join("");
  const note = uniformDate
    ? '<div class="muted-line">Early bird through ' + esc(fmtDate(uniformDate)) + "</div>"
    : "";
  return collapsibleCard(code, "pricing", "Pricing", curBadge,
    note +
    '<div class="tbl-wrap"><table class="tbl sortable"><thead><tr>' +
    sortableTh("Registration type") +
    sortableTh("Standard", true) + sortableTh("Early bird", true) +
    "</tr></thead><tbody>" +
    body +
    "</tbody></table></div>");
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
function paymentsCard(code, payments, currency) {
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
      '<div class="tbl-wrap"><table class="tbl"><thead><tr><th>Attendee</th><th>Invoice</th>' +
      '<th class="num">Ordered</th><th class="num">Paid</th><th class="num">Due</th>' +
      "<th>Method</th><th>Status</th></tr></thead><tbody>" + visible +
      "</tbody>" + more + "</table></div>";
  }
  const cancelledLine = cancelled.length
    ? '<div class="muted-line">' +
      esc(cancelled.length) + " cancelled orders</div>"
    : "";
  return collapsibleCard(code, "payments", "Payments", "", totals + body + cancelledLine);
}

// Program card: sessions count + speakers (name, affiliation) capped at 50
// with a "Show all (N)" expander; nothing at all → empty state.
function programCard(bundle) {
  const code = bundle.code || "";
  const sessions = Array.isArray(bundle.sessions) ? bundle.sessions : [];
  const speakers = Array.isArray(bundle.speakers) ? bundle.speakers : [];
  const countBadge =
    ' <span class="badge badge-slate">' + esc(sessions.length) +
    " sessions · " + esc(speakers.length) + " speakers</span>";
  if (!sessions.length && !speakers.length) {
    return collapsibleCard(code, "program", "Program", countBadge,
      '<div class="empty-state"><div class="hint">No program yet</div></div>');
  }
  let body = "";
  const CAP = 50;
  const rows = speakers.map((s) => {
    const name = [s.firstName, s.middleName, s.lastName].filter(Boolean).join(" ");
    return "<tr><td>" + esc(name) + "</td><td>" + esc(s.company) + "</td></tr>";
  });
  if (rows.length) {
    const visible = rows.slice(0, CAP).join("");
    const rest = rows.slice(CAP);
    body +=
      '<div class="tbl-wrap"><table class="tbl"><thead><tr><th>Name</th><th>Affiliation</th></tr></thead><tbody>' +
      visible + "</tbody>" +
      (rest.length
        ? '<tbody data-more hidden>' + rest.join("") + "</tbody>"
        : "") +
      "</table></div>" +
      (rest.length
        ? '<button class="expander" data-expand type="button">Show all (' +
          esc(rows.length) + ")</button>"
        : "");
  } else {
    // Sessions exist but no speakers: the badge carries the count, the body
    // explains there is no per-session list.
    body += '<div class="empty-state"><div class="hint">Session list not available</div></div>';
  }
  return collapsibleCard(code, "program", "Program", countBadge, body);
}

function dashboardHtml(bundle, payments) {
  // Currency: the fee items' code first, then the event's (typically the
  // same); absent → money() renders a bare number.
  const fee = Array.isArray(bundle.feeItems) ? bundle.feeItems : [];
  const currency =
    (fee[0] && fee[0].currency) || (bundle.event && bundle.event.currency) || "";
  return (
    headerCard(bundle) +
    registrationsCard(bundle) +
    pricingCard(bundle, currency) +
    paymentsCard(bundle.code || "", payments, currency) +
    programCard(bundle)
  );
}

// Wires the rendered dashboard: the "Show all (N)" expanders reveal their
// hidden rows in place, and sortable tables (Registrations, Pricing) sort
// their rows on header tap — asc → desc → original order. (Refresh is a
// topbar control — app.js — so there is nothing view-local for repulls.)
function wireDashboard(mount, code) {
  // Collapsible sections: the section head is the toggle. Toggling flips the
  // card's .collapsed class (CSS hides .sec-body), syncs aria-expanded,
  // rotates the chevron, and persists per-event in localStorage.
  mount.querySelectorAll(".sec-toggle").forEach((btn) => {
    btn.addEventListener("click", () => {
      const cardEl = btn.closest(".sec-card");
      if (!cardEl) return;
      const collapsed = cardEl.classList.toggle("collapsed");
      btn.setAttribute("aria-expanded", String(!collapsed));
      setSecCollapsed(code || "", btn.getAttribute("data-sec"), collapsed);
    });
  });
  mount.querySelectorAll("[data-expand]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const scope = btn.closest(".card") || mount;
      const more = scope.querySelector("[data-more]");
      if (more) more.removeAttribute("hidden");
      btn.remove();
    });
  });
  mount.querySelectorAll(".tbl.sortable").forEach((tbl) => {
    const tbody = tbl.querySelector("tbody");
    if (!tbody) return;
    const rows = Array.from(tbody.querySelectorAll("tr"));
    const original = rows.slice(); // untouched order (phase 3 resets to this)
    const state = { col: -1, dir: 0 };
    const headers = Array.from(tbl.querySelectorAll("th[data-sort]"));
    const setIndicators = () => {
      headers.forEach((th) => {
        const i = headers.indexOf(th);
        const ind = th.querySelector(".sort-ind");
        if (!ind) return;
        if (i !== state.col || state.dir === 0) {
          ind.innerHTML = "&#8597;"; // ⇇ neutral
          th.removeAttribute("aria-sort");
        } else {
          ind.innerHTML = state.dir === 1 ? "&#9650;" : "&#9660;"; // ▲ / ▼
          th.setAttribute("aria-sort", state.dir === 1 ? "ascending" : "descending");
        }
      });
    };
    const apply = () => {
      const col = state.col;
      const dir = state.dir;
      if (col < 0 || dir === 0) {
        // Phase 3: back to the untouched Cvent order.
        original.forEach((r) => tbody.appendChild(r));
        setIndicators();
        return;
      }
      const isNum = headers[col].getAttribute("data-type") === "num";
      const cmp = (a, b) => {
        const av = a.children[col] ? a.children[col].getAttribute("data-val") : "";
        const bv = b.children[col] ? b.children[col].getAttribute("data-val") : "";
        if (isNum) return (Number(av) || 0) - (Number(bv) || 0);
        return String(av).localeCompare(String(bv), undefined, { numeric: true, sensitivity: "base" });
      };
      rows.sort((a, b) => cmp(a, b) * dir || (original.indexOf(a) - original.indexOf(b)));
      rows.forEach((r) => tbody.appendChild(r));
      setIndicators();
    };
    const toggle = (th) => {
      const col = headers.indexOf(th);
      if (state.col !== col) { state.col = col; state.dir = 1; }
      else if (state.dir === 1) state.dir = -1;
      else { state.col = -1; state.dir = 0; }
      apply();
    };
    headers.forEach((th) => {
      th.style.cursor = "pointer";
      th.addEventListener("click", () => toggle(th));
      th.addEventListener("keydown", (e) => {
        if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(th); }
      });
    });
  });
}

/* ---------- events landing (the multi-event index) ----------
   One card per event in the catalog. Each card links to #/<code> (that
   event's dashboard). All text comes from the catalog entries — nothing
   data-derived is hardcoded. */

function eventCardHtml(entry) {
  const title = entry.title || entry.code;
  const dates = entry.start ? fmtRange(entry.start, entry.end || entry.start) : "";
  const meta = [];
  if (entry.format) meta.push(esc(entry.format));
  if (entry.currency) meta.push(esc(entry.currency));
  const metaHtml = meta.length
    ? '<div class="event-card-meta">' + meta.join(" · ") + "</div>"
    : "";
  return (
    '<a class="event-card" href="#/' + encodeURIComponent(entry.code) + '">' +
    '<div class="event-card-title">' + esc(title) + "</div>" +
    (dates ? '<div class="event-card-dates">' + esc(dates) + "</div>" : "") +
    metaHtml +
    '<div class="event-card-code">' + esc(entry.code) + "</div>" +
    "</a>"
  );
}

function eventsListHtml(entries) {
  if (!entries.length) {
    return card(
      '<div class="empty-state">' +
      '<div class="title">No events</div>' +
      '<div class="hint">The catalog is empty. Pull an event to get started.</div>' +
      "</div>"
    );
  }
  const cards = entries.map(eventCardHtml).join("");
  return '<div class="event-grid">' + cards + "</div>";
}

export function events(mount) {
  mount.innerHTML = skeletonHtml(3);
  getEvents()
    .then((res) => {
      const entries = Array.isArray(res.events) ? res.events : [];
      mount.innerHTML = eventsListHtml(entries);
    })
    .catch((e) => {
      const msg = (e && e.message) ? e.message : "unknown error";
      mount.innerHTML = card(
        '<div class="empty-state">' +
        '<div class="title">Couldn&#39;t load events</div>' +
        '<div class="hint" data-err></div>' +
        '<button class="btn" data-retry type="button">Retry</button>' +
        "</div>"
      );
      const hint = mount.querySelector("[data-err]");
      if (hint) hint.textContent = msg;
      const retry = mount.querySelector("[data-retry]");
      if (retry) retry.addEventListener("click", () => events(mount));
    });
}

export function home(mount, code) {
  loadHome(mount, code);
}

function loadHome(mount, code) {
  mount.innerHTML = skeletonHtml(5);
  Promise.all([getEvent(code), getPayments(code)])
    .then(([bundle, payments]) => {
      mount.innerHTML = dashboardHtml(bundle, payments);
      wireDashboard(mount, code);
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
      if (retry) retry.addEventListener("click", () => loadHome(mount, code));
    });
}

/* ---------- module state: the open attendee sheet (one at a time) ----------
   Tracked at module level so a route change (app.js clears the mount) can
   close it and restore body scroll. */
let sheetEl = null;

/* Attendees instance sequence: bumped on every attendees() entry so a
   re-entry (or a route change away) invalidates the previous instance's
   pending continuations. app.js renders all views into one shared mount
   and clears it on route change, so a stale instance's debounce timer or
   in-flight fetch must NOT render into the mount once it has moved on —
   every async continuation in the attendees view checks isCurrent()
   (captured per entry) before touching the mount. */
let attSeq = 0;
let attDebounce = null; // previous instance's pending search timer (cleared on entry)

/* app.js has no view-unmount hook, so navigation away from the attendees
   view is observed via the route hash itself. Leaving the attendees route
   is the "close" path: bump the sequence (invalidating this instance's
   pending continuations) and close the sheet (restores body scroll).
   Registered once. */
let attNavWatch = false;
function ensureNavWatch() {
  if (attNavWatch) return;
  attNavWatch = true;
  window.addEventListener("hashchange", () => {
    const h = (location.hash || "").replace(/^#/, "") || "/";
    // attendees route is #/<code>/attendees — any other path is "away".
    if (!/\/attendees\/?$/.test(h)) {
      attSeq++; // "close": invalidate the torn-down instance
      closeSheet();
    }
  });
}
function closeSheet() {
  if (sheetEl) {
    sheetEl.remove();
    sheetEl = null;
  }
  document.body.style.overflow = ""; // restore body scroll
}

/* ---------- attendees view (Task 11) ----------
   Search (300 ms debounce) + table + bottom-sheet detail + optimistic
   check-in. Same render rules as the dashboard: every data string through
   esc(), skeleton rows while the first search loads, an error card with
   Retry on failure, no console.log, no hardcoded event code. Static and
   live share one render path — the only differences are the Snapshot
   badge (sheet header), the hidden check-in button, and the static-mode
   file search inside searchAttendees.

   Attendee field ground truth (openapi.json "attendee" schema):
     name        contact.{firstName,middleName,lastName} — no top-level name
     email       contact.email — no top-level email
     ticket      registrationType is a Lookup {id, code, name} → .name
     confirmation confirmationNumber (top-level string)
     checkedIn   top-level BOOLEAN (true = checked in); the checkIn
                 date-time is a separate, non-boolean field
     answers     [{ question: {id}, value: [string, ...] }] — the
                 question object carries ONLY an id; the search response's
                 `questions` map (from /event-questions) supplies the text
     questions   [{ id, name, type, value: [...] }] — contact custom fields
                 (e.g. Emergency Contact), labelled inline by name
     due amount  NOT present: the attendee object carries no order or
                 payment data → no due column (no fabricated join)
     activities  NOT embedded on the attendee → no activities block
     transactions NOT embedded on the attendee → no transactions block */

function attendeeName(a) {
  const c = (a && a.contact) || {};
  return [c.firstName, c.middleName, c.lastName].filter(Boolean).join(" ").trim();
}

// Checked-in = the boolean flag; a truthy checkIn date-time counts too
// (defensive — the schema says the boolean is authoritative).
function attendeeCheckedIn(a) {
  return !!(a && (a.checkedIn === true || a.checkIn));
}

function attendeeEmail(a) {
  return (a && a.contact && a.contact.email) || (a && a.email) || "";
}

function attendeeTicket(a) {
  const rt = a && a.registrationType;
  if (!rt || typeof rt !== "object") return "";
  return rt.name || rt.code || "";
}

// Detail-sheet rows, as [section title, rows]. Contact custom fields
// (`questions: [{ name, value }]`, else `contact.customFields`) are
// labelled inline; registration answers (`answers: [{ question: {id},
// value }]`) are labelled via qmap (id → {text}), falling back to the raw id.
// Both are shown — an attendee commonly has both.
function attendeeDetailSections(a, qmap) {
  const c = (a && a.contact) || {};
  const norm = (v) =>
    Array.isArray(v) ? v : v != null && v !== "" ? [v] : [];
  const named = (list) =>
    (Array.isArray(list) ? list : [])
      .map((f) => ({ label: (f && f.name) || "", vals: norm(f && f.value) }))
      .filter((r) => r.label || r.vals.length);
  const custom = Array.isArray(a && a.questions) && a.questions.length ? a.questions : c.customFields;
  const answers = (Array.isArray(a && a.answers) ? a.answers : [])
    .map((ans) => {
      const id = (ans && ans.question && ans.question.id) || "";
      const q = (qmap && id && qmap[id]) || null;
      return { label: (q && q.text) || id, vals: norm(ans && ans.value) };
    })
    .filter((r) => r.label || r.vals.length);
  return [
    ["Registration answers", answers],
    ["Additional information", named(custom)],
  ].filter(([, rows]) => rows.length);
}

// Work (or home) address flattened to a single line.
function attendeeAddress(a) {
  const c = (a && a.contact) || {};
  const ad = c.workAddress || c.homeAddress || {};
  const city = [ad.city, [ad.regionCode, ad.postalCode].filter(Boolean).join(" ")]
    .filter(Boolean)
    .join(", ");
  return [ad.address1, city, ad.country || ad.countryCode].filter(Boolean).join(", ");
}

function attendeeSheetHtml(a, qmap) {
  const name = attendeeName(a);
  const c = (a && a.contact) || {};
  const conf = a.confirmationNumber || "";
  const ticket = attendeeTicket(a);
  const checked = attendeeCheckedIn(a);
  const company = c.company || "";
  const title = c.title || "";
  const status = a.status || "";
  const headBadges =
    (STATIC ? '<span class="badge badge-slate">Snapshot</span>' : "") +
    (ticket ? '<span class="badge badge-slate">' + esc(ticket) + "</span>" : "") +
    (status ? '<span class="badge badge-slate">' + esc(status) + "</span>" : "") +
    (checked
      ? '<span class="badge badge-green">Checked in</span>'
      : '<span class="badge badge-amber">Not checked in</span>');
  let inner =
    '<div class="sheet-head-row"><div>' +
    "<h2>" + esc(name || "Attendee") + "</h2>" +
    '<div class="sheet-meta">' +
    (conf ? esc(conf) : "") +
    (conf && a.checkIn ? " · " : "") +
    (a.checkIn ? "in " + esc(fmtDate(a.checkIn)) : "") +
    "</div></div>" +
    '<button class="sheet-close" data-sheet-close type="button" aria-label="Close">' +
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" ' +
    'stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>' +
    "</button></div>" +
    (company || title
      ? '<div class="sheet-affiliation">' +
        esc([company, title].filter(Boolean).join(" · ")) +
        "</div>"
      : "") +
    '<div class="badge-row">' + headBadges + "</div>";
  // Contact block: only fields that are actually present in the payload.
  const contactRows = [
    ["Email", attendeeEmail(a)],
    ["Phone", c.mobilePhone || ""],
    ["Address", attendeeAddress(a)],
  ].filter(([, v]) => v);
  if (contactRows.length) {
    inner += '<div class="sheet-sec">Contact</div><dl class="kv-list">';
    for (const [k, v] of contactRows) {
      inner += "<dt>" + esc(k) + "</dt><dd>" + esc(v) + "</dd>";
    }
    inner += "</dl>";
  }
  for (const [title, rows] of attendeeDetailSections(a, qmap)) {
    inner += '<div class="sheet-sec">' + esc(title) + '</div><dl class="kv-list">';
    for (const r of rows) {
      inner +=
        "<dt>" + esc(r.label) + "</dt><dd>" +
        (r.vals.length ? r.vals.map((v) => esc(v)).join(", ") : "n/a") + "</dd>";
    }
    inner += "</dl>";
  }
  // Check in: live mode only. Already checked in → disabled 'Checked in'
  // (check-out is out of scope for v1). Static mode → no button (the
  // Snapshot badge above marks the snapshot state).
  if (a.id && !STATIC) {
    inner += checked
      ? '<button class="btn" disabled type="button">Checked in</button>'
      : '<button class="btn" data-checkin type="button">Check in</button>';
  }
  return inner;
}

// POST /api/cvent/events/<code>/checkin. Client body (the plan contract
// the server handler implements, handlers.go handleCheckin):
// { attendeeIds: [uuid] }.
// The server maps it onto the Cvent bulk-checkin spec and returns
// { ok: true } on success; errors surface as non-OK JSON { error }.
async function checkInAttendee(code, id) {
  const res = await fetch(API + "/events/" + encodeURIComponent(code) + "/checkin", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ attendeeIds: [id] }),
  });
  if (!res.ok) {
    let msg = "HTTP " + res.status;
    try {
      const j = await res.json();
      if (j && j.error) msg = j.error;
    } catch (e) { /* keep the HTTP message */ }
    throw new Error(msg);
  }
}

function searchCardHtml(value) {
  return card(
    '<input class="search-input" data-search type="search" ' +
    'placeholder="Search name, email, or confirmation number" ' +
    'aria-label="Search attendees" value="' + esc(value || "") + '">'
  );
}

// The table card: count line, the table over ALL loaded rows (state.items),
// and the Load-more button with the remaining count. The two empty states
// live here too: "No attendees yet" (no q, total 0) vs "No matches for q".
function attendeesTableHtml(st) {
  const r = st.result;
  if (!r) return "";
  if (r.total === 0) {
    if (st.q) {
      return card(
        '<div class="empty-state">' +
        '<div class="icon">' + ICON + "</div>" +
        '<div class="title">No matches for “' + esc(st.q) +
        "”</div></div>"
      );
    }
    return card(
      '<div class="empty-state">' +
      '<div class="icon">' + ICON + "</div>" +
      '<div class="title">No attendees yet</div>' +
      '<div class="hint">Registrations will appear here once the event has ' +
      "attendees.</div></div>"
    );
  }
  const rows = st.items
    .map((a, i) => {
      const checked = attendeeCheckedIn(a);
      return (
        '<tr data-row="' + i + '">' +
        "<td>" + esc(attendeeName(a) || "n/a") + "</td>" +
        '<td class="muted-cell">' + esc(attendeeEmail(a) || "n/a") + "</td>" +
        "<td>" + esc(attendeeTicket(a) || "n/a") + "</td>" +
        '<td class="mono-cell">' + esc(a.confirmationNumber || "n/a") + "</td>" +
        "<td>" +
        (checked
          ? '<span class="badge badge-green">Checked in</span>'
          : '<span class="badge badge-slate">Not checked in</span>') +
        "</td></tr>"
      );
    })
    .join("");
  const remaining = Math.max(r.total - (r.offset + r.limit), 0);
  return (
    card('<div class="att-count-line">' + esc(r.total) + " attendees</div>") +
    card(
      '<div class="tbl-wrap"><table class="tbl"><thead><tr><th>Name</th><th>Email</th><th>Ticket</th>' +
      "<th>Confirmation</th><th>Check-in</th></tr></thead><tbody>" +
      rows + "</tbody></table></div>" +
      (remaining > 0
        ? '<button class="expander" data-loadmore type="button">Load more (' +
          esc(remaining) + ")</button>"
        : "")
    )
  );
}

export function attendees(mount, code) {
  // New instance: bump the sequence (invalidating any previous instance's
  // pending continuations) and drop the previous instance's pending search
  // timer. The token is the authoritative stale guard; the clearTimeout is
  // belt-and-suspenders (the token would no-op the callback anyway).
  attSeq++;
  const token = attSeq;
  const isCurrent = () => token === attSeq;
  // The shared mount is considered torn down once our search card is no
  // longer inside it (route-away, or a direct mount clear). The first
  // render is exempt (the card does not exist yet) — the `rendered` flag
  // marks that boundary.
  let rendered = false;
  const isMounted = () => !rendered || !!mount.querySelector("[data-search]");
  clearTimeout(attDebounce);
  closeSheet(); // route change — drop any open sheet + restore body scroll
  ensureNavWatch(); // restore body scroll on navigation-away (no unmount hook)
  const state = { q: "", items: [], questions: {}, result: null, loading: false };

  // The one search listener, re-attached after every re-render (mount
  // innerHTML replacement drops it). 300 ms debounce per the view spec. The
  // timer is module-level so a re-entry clears the previous instance's.
  const onSearch = (e) => {
    const v = (e && e.target && e.target.value) || "";
    clearTimeout(attDebounce);
    attDebounce = setTimeout(() => {
      if (!isCurrent() || !isMounted()) return; // navigated away while pending
      state.q = v.trim();
      load(0);
    }, 300);
  };

  // Full re-render of the view: search card + (skeleton | table | error).
  // The typed input value is preserved across re-renders. No-op when this
  // instance is no longer current (the shared mount now holds another view).
  function render(mnt, bodyHtml) {
    if (!isCurrent() || !isMounted()) return;
    const inp = mnt.querySelector("[data-search]");
    const keep = inp && inp.value != null ? inp.value : state.q;
    mnt.innerHTML = searchCardHtml(keep) + bodyHtml;
    rendered = true;
    const ni = mnt.querySelector("[data-search]");
    if (ni) {
      ni.addEventListener("input", onSearch);
      ni.value = keep; // keep the text even if the attr was already set
    }
    wireTable(mnt);
  }

  function wireTable(mnt) {
    mnt.querySelectorAll("[data-row]").forEach((tr) => {
      const i = Number(tr.getAttribute("data-row"));
      const a = state.items[i];
      if (!a) return;
      tr.addEventListener("click", () => openSheet(a));
      // keyboard access: rows open the sheet on Enter/Space
      tr.setAttribute("tabindex", "0");
      tr.setAttribute("role", "button");
      tr.setAttribute("aria-label", "Open details for " + (attendeeName(a) || "attendee"));
      tr.addEventListener("keydown", (e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          openSheet(a);
        }
      });
    });
    const lm = mnt.querySelector("[data-loadmore]");
    if (lm) {
      lm.addEventListener("click", () => {
        if (state.loading) return;
        const y = window.scrollY; // restore after the append
        lm.disabled = true;
        load(state.result.offset + state.result.limit).then(() => {
          if (!isCurrent() || !isMounted()) return; // navigated away mid-fetch
          window.scrollTo(0, y);
          const b = mnt.querySelector("[data-loadmore]");
          if (b) b.disabled = false;
        });
      });
    }
  }

  async function load(offset) {
    if (!isCurrent() || !isMounted()) return; // stale instance — no-op
    if (state.loading) {
      // A search typed mid-flight must not be dropped: remember it and
      // re-run it when the in-flight load settles.
      if (offset === 0) state.pendingSearch = true;
      return;
    }
    state.loading = true;
    if (!state.result) render(mount, skeletonHtml(6)); // skeleton: first load
    let r;
    try {
      r = await searchAttendees(code, state.q, 50, offset);
    } catch (e) {
      state.loading = false;
      if (!isCurrent() || !isMounted()) return; // navigated away mid-fetch
      if (state.pendingSearch) {
        state.pendingSearch = false;
        load(0);
        return;
      }
      if (!state.result) {
        const msg = e && e.message ? e.message : "unknown error";
        render(
          mount,
          card(
            '<div class="empty-state">' +
            '<div class="title">Couldn&#39;t load attendees</div>' +
            '<div class="hint" data-err></div>' +
            '<button class="btn" data-retry type="button">Retry</button></div>'
          )
        );
        const hint = mount.querySelector("[data-err]");
        if (hint) hint.textContent = msg;
        const retry = mount.querySelector("[data-retry]");
        if (retry) retry.addEventListener("click", () => load(0));
      } else {
        // Load-more failure: keep the table, toast the error.
        const msg = e && e.message ? e.message : "unknown error";
        toastMsg("Couldn't load more attendees: " + msg);
        const b = mount.querySelector("[data-loadmore]");
        if (b) b.disabled = false;
      }
      return;
    }
    state.loading = false;
    state.result = r;
    state.items = offset === 0 ? r.items : state.items.concat(r.items);
    state.questions = Object.assign(offset === 0 ? {} : state.questions, r.questions || {});
    if (state.pendingSearch) {
      state.pendingSearch = false;
      load(0); // a search was typed mid-flight — run it now
      return;
    }
    if (!isCurrent() || !isMounted()) return; // navigated away mid-fetch
    render(mount, attendeesTableHtml(state));
  }

  function openSheet(a) {
    closeSheet(); // one sheet at a time
    const wrap = document.createElement("div");
    wrap.className = "bottom-sheet";
    wrap.innerHTML =
      '<div class="sheet-backdrop" data-sheet-close></div>' +
      '<div class="sheet-panel"><span class="sheet-handle" data-sheet-close></span>' +
      attendeeSheetHtml(a, state.questions) +
      "</div>";
    (document.body || document).appendChild(wrap);
    document.body.style.overflow = "hidden"; // body scroll lock (house-style)
    // Delegation on the wrapper only — innerHTML swaps (check-in flips)
    // never orphan the close/check-in listeners.
    wrap.addEventListener("click", (e) => {
      const t = e && e.target;
      if (!t || !t.closest) return;
      if (t.closest("[data-sheet-close]")) closeSheet();
      else if (t.closest("[data-checkin]")) doCheckin(a);
    });
    sheetEl = wrap;
  }

  // Swap just this row's check-in badge (no full re-render, so scroll and
  // the open sheet are undisturbed).
  function paintChecked(a, checked) {
    const i = state.items.indexOf(a);
    if (i < 0) return;
    const tr = mount.querySelector('[data-row="' + i + '"]');
    const td = tr && tr.children[tr.children.length - 1];
    if (!td) return;
    td.innerHTML = checked
      ? '<span class="badge badge-green">Checked in</span>'
      : '<span class="badge badge-slate">Not checked in</span>';
  }

  // Optimistic check-in: flip the row badge + sheet button NOW, POST, then
  // keep the flip on success or revert both + error toast on failure.
  function doCheckin(a) {
    if (!a.id || state.loading) return;
    const was = !!a.checkedIn;
    a.checkedIn = true; // optimistic flip (data + UI)
    paintChecked(a, true);
    if (sheetEl) {
      // Re-render the sheet panel (button → disabled 'Checked in'); the
      // wrapper keeps its delegated listeners.
      const panel = sheetEl.querySelector(".sheet-panel");
      if (panel) {
        panel.innerHTML =
          '<span class="sheet-handle" data-sheet-close></span>' +
          attendeeSheetHtml(a, state.questions);
      }
    }
    checkInAttendee(code, a.id)
      .then(() => {
        toastMsg("Checked in");
      })
      .catch((e) => {
        a.checkedIn = was; // revert the data
        const msg = e && e.message ? e.message : "unknown error";
        toastMsg("Check-in failed: " + msg); // app-level toast: keep even if stale
        if (!isCurrent() || !isMounted()) return; // no badge/sheet paint onto the new view
        paintChecked(a, false);
        if (sheetEl) {
          const panel = sheetEl.querySelector(".sheet-panel");
          if (panel) {
            panel.innerHTML =
              '<span class="sheet-handle" data-sheet-close></span>' +
              attendeeSheetHtml(a, state.questions);
          }
        }
      });
  }

  render(mount, skeletonHtml(6)); // initial: search + skeleton
  load(0);
}

