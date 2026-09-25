/* Multi-source contract — the single registry the app shell reads.
 *
 * Every source module lives at web/sources/<name>.js and exports the
 * shape documented below. app.js builds its nav/tabs from this object —
 * NEVER from hardcoded source names. Adding a source = one new file +
 * one line here; the shell, themes, offline banner, and service worker
 * are inherited unchanged.
 *
 * Source module contract:
 *   {
 *     label: string,          // display name in nav/tabs
 *     views: { <name>: fn(mount, code) },
 *                             // rendered by the hash router; each receives
 *                             // the mount element and the event code from
 *                             // the route (#/<code>/<view>). The special
 *                             // "events" view is the multi-event landing
 *                             // and takes no code (it lists them).
 *     getEvents: fn() -> {events, default},
 *                             // the source's catalog (event list). Drives
 *                             // the landing view and the topbar selector.
 *     staticDataPath: string, // snapshot dir served at the same origin
 *                             // (the --dump output dir for static mode)
 *   }
 */

import * as cvent from "./cvent.js";

// tabs: the views shown in the per-event tab bar, in display order, with
// their labels. "events" (the landing) and "home" (the dashboard, shown as
// the first tab) are reserved view keys the shell understands.
const TABS = [
  { key: "home", label: "Overview" },
  { key: "attendees", label: "Attendees" },
];

export const SOURCES = {
  cvent: {
    label: "Cvent",
    tabs: TABS,
    views: {
      events: cvent.events,
      home: cvent.home,
      attendees: cvent.attendees,
    },
    getEvents: cvent.getEvents,
    staticDataPath: "data/",
    // Extra surface beyond the minimal contract: the topbar Refresh button
    // triggers the server repull; sources without a live repull omit this.
    refresh: cvent.refresh,
    isRepulling: cvent.isRepulling,
  },
};
