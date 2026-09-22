/* Cvent Data PWA — pre-paint theme bootstrap + shell wiring.
 *
 * Loaded in <head> WITHOUT defer, before the styles.css <link>, so
 * document.documentElement.dataset.theme is set before first paint (no
 * flash of the wrong theme). It is an external file on purpose: the server's
 * CSP sends `default-src 'self'`, which blocks inline <script> elements
 * anywhere in index.html (style-src 'unsafe-inline' only covers inline
 * styles). This file is that vehicle.
 *
 * Responsibilities:
 *   1. Pre-paint: read localStorage "cvent-data-theme" (default|light|dark),
 *      set <html data-theme>, and update the color-scheme + theme-color
 *      metas to match the *effective* scheme.
 *   2. Wire #theme-toggle (cycles default -> light -> dark, persists).
 *   3. Follow OS scheme changes live while in "default" mode.
 *   4. Graceful fallback: if app.js (Task 9) never loads, show minimal
 *      "loading…" text in #app-view so the page is never blank.
 */
(function () {
  "use strict";

  var STORAGE_KEY = "cvent-data-theme";
  var MODES = ["default", "light", "dark"];
  var LABELS = { default: "Auto", light: "Light", dark: "Dark" };
  var THEME_COLOR = { light: "#f5f7f5", dark: "#0d1512" };

  var root = document.documentElement;

  // ---------- 1. pre-paint: resolve stored mode, set data-theme ----------
  var saved = null;
  try { saved = window.localStorage.getItem(STORAGE_KEY); } catch (e) { /* storage blocked */ }
  var initial = MODES.indexOf(saved) !== -1 ? saved : "default";

  function isDark(m) {
    if (m === "dark") return true;
    if (m === "light") return false;
    return !!(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
  }

  root.dataset.theme = initial;

  // ---------- meta tags: color-scheme + theme-color (per effective scheme)
  function upsertMeta(name, content) {
    var m = document.querySelector('meta[name="' + name + '"]');
    if (!m) {
      m = document.createElement("meta");
      m.setAttribute("name", name);
      document.head.appendChild(m);
    }
    m.setAttribute("content", content);
  }

  function applyMeta() {
    var dark = isDark(root.dataset.theme || "default");
    upsertMeta("color-scheme", dark ? "dark" : "light");
    upsertMeta("theme-color", dark ? THEME_COLOR.dark : THEME_COLOR.light);
  }
  applyMeta();

  // OS scheme change while in "default" mode: tokens flip via the CSS media
  // query; only the metas need refreshing.
  if (window.matchMedia) {
    var mq = window.matchMedia("(prefers-color-scheme: dark)");
    var onMq = function () { if (root.dataset.theme === "default") applyMeta(); };
    if (mq.addEventListener) mq.addEventListener("change", onMq);
    else if (mq.addListener) mq.addListener(onMq);
  }

  // ---------- 2. theme toggle ----------
  var ICONS = {
    default: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="20" height="14" x="2" y="3" rx="2"/><path d="M8 21h8"/></svg>',
    light: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/></svg>',
    dark: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/></svg>'
  };

  function updateToggle() {
    var btn = document.getElementById("theme-toggle");
    if (!btn) return;
    var m = root.dataset.theme || "default";
    var icon = btn.querySelector(".topbar-btn-icon");
    var label = document.getElementById("theme-label");
    if (icon) icon.innerHTML = ICONS[m];
    if (label) label.textContent = LABELS[m];
    btn.setAttribute("aria-label", "Theme: " + LABELS[m] + " (tap to change)");
    btn.title = "Theme: " + LABELS[m];
  }

  function setMode(m) {
    root.dataset.theme = m;
    try { window.localStorage.setItem(STORAGE_KEY, m); } catch (e) { /* storage blocked */ }
    applyMeta();
    updateToggle();
    try {
      document.dispatchEvent(new CustomEvent("cvent-themechange", { detail: { theme: m } }));
    } catch (e) { /* CustomEvent unsupported — non-fatal */ }
  }

  function cycle() {
    var m = root.dataset.theme || "default";
    setMode(MODES[(MODES.indexOf(m) + 1) % MODES.length]);
  }

  // ---------- 4. graceful fallback for the (not yet existing) app.js ------
  // The Go static handler SPA-fallbacks unknown paths to index.html, so a
  // missing /app.js arrives as a 200 HTML body that the module loader
  // rejects (MIME) — either way the script element's error event fires, but
  // the 2s timeout is the guaranteed catch-all (e.g. slow-but-never network).
  function showFallback() {
    var view = document.getElementById("app-view");
    if (!view || view.hasChildNodes()) return;
    var p = document.createElement("p");
    p.className = "loading-note";
    p.textContent = "Loading\u2026";
    view.appendChild(p);
  }

  function wireShell() {
    var btn = document.getElementById("theme-toggle");
    if (btn) btn.addEventListener("click", cycle);
    updateToggle();

    var appScript = document.querySelector('script[type="module"][src="app.js"]');
    var failed = false;
    function fail() {
      if (failed) return;
      failed = true;
      showFallback();
    }
    if (appScript) appScript.addEventListener("error", fail);
    setTimeout(fail, 2000);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", wireShell);
  } else {
    wireShell();
  }
})();
