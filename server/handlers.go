package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IndicoDataFusion/cvent-data/cvent"
)

// cventHandlers serves the /api/cvent/… routes. Every event-scoped route
// carries the event code in the path:
//
//	GET  /api/cvent/events/<code>                bundle
//	GET  /api/cvent/events/<code>/payments
//	GET  /api/cvent/events/<code>/attendees?q=&limit=&offset=
//	POST /api/cvent/events/<code>/checkin
//	POST /api/cvent/events/<code>/repull
//	GET  /api/cvent/events/<code>/repull-status
//
// The catalog (GET /api/cvent/events — the list of events the site shows) is
// served by handleAPI in main.go, not here: it is a local-file read of
// data/index.json and must work even when Cvent credentials are missing.
type cventHandlers struct {
	defaultCode string // the --event default; surfaced as the catalog "default"
	client      *cvent.Client
	cache       *cvent.EventCache

	mu      sync.Mutex
	running map[string]bool // code → background repull in flight
}

func newCventHandlers(defaultCode string, c *cvent.Client, ec *cvent.EventCache) *cventHandlers {
	return &cventHandlers{defaultCode: defaultCode, client: c, cache: ec, running: map[string]bool{}}
}

// route dispatches the /api/cvent/ sub-paths. The bare /api/cvent/events
// catalog is handled upstream in handleAPI; everything else here is
// events/<code>[/<sub>]. Unknown sub-path → 404.
func (h *cventHandlers) route(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/cvent/")
	// rest should be "events/<code>" or "events/<code>/<sub>".
	after := strings.TrimPrefix(rest, "events/")
	if after == rest { // not an events/ path
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	parts := strings.SplitN(after, "/", 2)
	code := parts[0]
	if code == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	switch sub {
	case "":
		h.handleEvent(w, r, code)
	case "payments":
		h.handlePayments(w, r, code)
	case "attendees":
		h.handleAttendees(w, r, code)
	case "checkin":
		h.handleCheckin(w, r, code)
	case "repull":
		h.handleRepull(w, r, code)
	case "repull-status":
		h.handleRepullStatus(w, r, code)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleEvent serves the cached event bundle for code. A bundle error (event
// resolution failed) is a 502 with the message; per-resource failures are
// NOT errors — they ride inside the bundle's Errors map.
func (h *cventHandlers) handleEvent(w http.ResponseWriter, r *http.Request, code string) {
	b, err := h.cache.Bundle(r.Context(), code)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// handlePayments builds the payment summary over the cached bundle's orders
// + transactions, joining attendee display names.
func (h *cventHandlers) handlePayments(w http.ResponseWriter, r *http.Request, code string) {
	b, err := h.cache.Bundle(r.Context(), code)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	var orders, txns []json.RawMessage
	if len(b.Orders) > 0 {
		_ = json.Unmarshal(b.Orders, &orders) // unparseable → nil, still clean
	}
	if len(b.Transactions) > 0 {
		_ = json.Unmarshal(b.Transactions, &txns)
	}
	writeJSON(w, http.StatusOK, cvent.BuildPayments(orders, txns, h.attendeeName(b)))
}

// attendeeName builds the id→display-name callback from the bundle's
// Attendees, parsed leniently (real payloads vary):
//
//  1. attendee.name.first/middle/last (or givenName/familyName),
//  2. flat attendee.firstName/middleName/lastName,
//  3. the same fields under attendee.contact (the attendee path in
//     openapi.json).
//
// Non-empty parts join as "First Middle. Last" (period after middle);
// no parts → "" and BuildPayments substitutes #id.
func (h *cventHandlers) attendeeName(b *cvent.EventBundle) func(string) string {
	names := map[string]string{}
	if len(b.Attendees) > 0 {
		var atts []json.RawMessage
		if err := json.Unmarshal(b.Attendees, &atts); err == nil {
			for _, raw := range atts {
				if id, name := attendeeIDAndName(raw); id != "" {
					names[id] = name
				}
			}
		}
	}
	return func(id string) string { return names[id] }
}

func strField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// nameParts reads the name fields from one object, tolerating both the
// givenName/familyName and firstName/middleName/lastName spellings.
func nameParts(m map[string]any) (first, middle, last string) {
	first = strField(m, "firstName")
	if first == "" {
		first = strField(m, "givenName")
	}
	if first == "" {
		first = strField(m, "first")
	}
	middle = strField(m, "middleName")
	if middle == "" {
		middle = strField(m, "middle")
	}
	last = strField(m, "lastName")
	if last == "" {
		last = strField(m, "familyName")
	}
	if last == "" {
		last = strField(m, "last")
	}
	return
}

// joinName assembles the parts with a period after the middle name when one
// is present: "Ada Byron Lovelace." → "Ada Byron. Lovelace".
func joinName(first, middle, last string) string {
	var parts []string
	if first != "" {
		parts = append(parts, first)
	}
	if middle != "" {
		parts = append(parts, middle+".")
	}
	if last != "" {
		parts = append(parts, last)
	}
	return strings.Join(parts, " ")
}

// attendeeIDAndName extracts the id and display name from one raw attendee.
func attendeeIDAndName(raw json.RawMessage) (string, string) {
	var a map[string]any
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", ""
	}
	var first, middle, last string
	if nm, ok := a["name"].(map[string]any); ok {
		first, middle, last = nameParts(nm)
	}
	if first == "" && middle == "" && last == "" {
		first, middle, last = nameParts(a)
	}
	if first == "" && middle == "" && last == "" {
		if ct, ok := a["contact"].(map[string]any); ok {
			first, middle, last = nameParts(ct)
		}
	}
	return strField(a, "id"), joinName(first, middle, last)
}

// handleAttendees searches the cached event's Registrants
// (cvent.RegistrantsFromBundle): case-insensitive substring over the name,
// email and confirmation number. limit defaults to 50 and caps at 200;
// offset defaults to 0. items are cvent.Registrant records, with labelled
// answers, guest links, payment and order lines.
func (h *cventHandlers) handleAttendees(w http.ResponseWriter, r *http.Request, code string) {
	b, err := h.cache.Bundle(r.Context(), code)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			offset = n
		}
	}

	matched := []cvent.Registrant{}
	for _, reg := range cvent.RegistrantsFromBundle(b, "") {
		if q == "" || registrantMatches(&reg, q) {
			matched = append(matched, reg)
		}
	}
	items := []cvent.Registrant{}
	if offset < len(matched) {
		items = matched[offset:min(offset+limit, len(matched))]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":  len(matched),
		"offset": offset,
		"limit":  limit,
		"items":  items,
	})
}

// registrantMatches reports whether q (lowercased) occurs in the
// registrant's name parts, full name, email, or confirmation number — the
// same haystack as the static-mode search in web/sources/cvent.js.
func registrantMatches(r *cvent.Registrant, q string) bool {
	for _, f := range []string{r.FullName, r.FirstName, r.MiddleName, r.LastName, r.Email, r.ConfirmationNumber} {
		if f != "" && strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// handleCheckin checks in attendees for code. Request body (plan contract):
// {"attendeeIds": ["uuid", ...]}. The Cvent spec (openapi.json,
// POST /events/{id}/check-in, schema bulk-checkin) takes a BULK ARRAY of
// {"id", "checkIn"} objects instead — this handler maps the plan shape onto
// the spec, stamping each checkIn with the current time. At most 100
// attendees per call (spec limit). On success the event cache is
// invalidated so the next read re-fetches fresh check-in status.
func (h *cventHandlers) handleCheckin(w http.ResponseWriter, r *http.Request, code string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var body struct {
		AttendeeIds []string `json:"attendeeIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}
	ids := make([]string, 0, len(body.AttendeeIds))
	for _, id := range body.AttendeeIds {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "attendeeIds must be a non-empty array"})
		return
	}
	if len(ids) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "up to 100 attendees per check-in call"})
		return
	}

	// uuid comes from the cached bundle's Event field (same resolution the
	// bundle used); no fetch here — the UI always reads /events/<code> first,
	// so a missing cache means nothing to check in against.
	uuid, ok := h.cache.EventUUID(code)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "event bundle not cached (fetch /api/cvent/events/" + code + " first)"})
		return
	}

	checkInAt := time.Now().UTC().Format(time.RFC3339)
	payload := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		payload = append(payload, map[string]string{"id": id, "checkIn": checkInAt})
	}
	status, raw, err := h.client.Checkin(r.Context(), uuid, payload)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	if status < 200 || status >= 300 { // spec: 207 on success
		text := strings.TrimSpace(string(raw))
		if text == "" {
			text = "check-in failed with status " + strconv.Itoa(status)
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": text})
		return
	}
	h.cache.Invalidate(code)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleRepull forces a background re-fetch for code: invalidate the cache,
// then fetch on a detached 20s context (client disconnects must not cancel
// it). The request returns immediately; concurrent repulls for the same
// event report running:true instead of starting a second fetch.
func (h *cventHandlers) handleRepull(w http.ResponseWriter, r *http.Request, code string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	h.mu.Lock()
	if h.running[code] {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"started": false, "running": true})
		return
	}
	h.running[code] = true
	h.mu.Unlock()

	h.cache.Invalidate(code)
	fctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	go func() {
		defer cancel()
		_, err := h.cache.Bundle(fctx, code)
		if err != nil {
			log.Printf("cvent repull %s: %v", code, err)
		}
		h.mu.Lock()
		delete(h.running, code)
		h.mu.Unlock()
	}()
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

// handleRepullStatus reports running (a background repull is in flight for
// code) and pulledAt (the cached bundle's pull time, null when the cache is
// empty).
func (h *cventHandlers) handleRepullStatus(w http.ResponseWriter, r *http.Request, code string) {
	h.mu.Lock()
	running := h.running[code]
	h.mu.Unlock()

	var pulledAt *string
	if s, ok := h.cache.PulledAt(code); ok {
		pulledAt = &s
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"running":  running,
		"pulledAt": pulledAt,
	})
}
