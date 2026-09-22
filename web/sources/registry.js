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
 *     views: { <name>: fn(mount) },  // rendered by the hash router;
 *                                     // each receives the mount element
 *                                     // and manages its own data access
 *     staticDataPath: string, // snapshot dir served at the same origin
 *                             // (the --dump output dir for static mode)
 *   }
 */

import * as cvent from "./cvent.js";

export const SOURCES = {
  cvent: {
    label: "Cvent",
    views: {
      home: cvent.home,
      attendees: cvent.attendees,
    },
    staticDataPath: "data/",
    // Extra surface beyond the minimal contract: the topbar Refresh button
    // triggers the server repull; sources without a live repull omit this.
    refresh: cvent.refresh,
    isRepulling: cvent.isRepulling,
  },
};
