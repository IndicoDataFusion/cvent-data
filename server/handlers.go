package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cventHandlers serves the single-event /api/cvent/… routes (Task 6). The
// event is fixed at construction (the app's one event); there is no event id
// in the URL and no /events list endpoint.
type cventHandlers struct {
	code   string // the one event this app serves
	client *cventClient
	cache  *eventCache

	mu      sync.Mutex
	running map[string]bool // code → background repull in flight
}

func newCventHandlers(code string, c *cventClient, ec *eventCache) *cventHandlers {
	return &cventHandlers{code: code, client: c, cache: ec, running: map[string]bool{}}
}

// route dispatches the /api/cvent/ sub-paths. Unknown sub-path → 404.
func (h *cventHandlers) route(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/api/cvent/") {
	case "event":
		h.handleEvent(w, r)
	case "event/payments":
		h.handlePayments(w, r)
	case "event/attendees":
		h.handleAttendees(w, r)
	case "event/checkin":
		h.handleCheckin(w, r)
	case "event/repull":
		h.handleRepull(w, r)
	case "event/repull-status":
		h.handleRepullStatus(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleEvent serves the cached event bundle. A bundle error (event
// resolution failed) is a 502 with the message; per-resource failures are
// NOT errors — they ride inside the bundle's Errors map.
func (h *cventHandlers) handleEvent(w http.ResponseWriter, r *http.Request) {
	b, err := h.cache.bundle(r.Context(), h.code)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// handlePayments builds the payment summary over the cached bundle's orders
// + transactions, joining attendee display names.
func (h *cventHandlers) handlePayments(w http.ResponseWriter, r *http.Request) {
	b, err := h.cache.bundle(r.Context(), h.code)
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
	writeJSON(w, http.StatusOK, BuildPayments(orders, txns, h.attendeeName(b)))
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
func (h *cventHandlers) attendeeName(b *EventBundle) func(string) string {
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
// is present: "Ada Lovelace", "Ada Byron Lovelace." → "Ada Byron. Lovelace".
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

// handleAttendees searches the cached attendees: case-insensitive substring
// over all name parts + email + confirmationNumber. limit defaults to 50
// and caps at 200; offset defaults to 0. items are the raw attendee objects.
func (h *cventHandlers) handleAttendees(w http.ResponseWriter, r *http.Request) {
	b, err := h.cache.bundle(r.Context(), h.code)
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

	var all []json.RawMessage
	if len(b.Attendees) > 0 {
		_ = json.Unmarshal(b.Attendees, &all)
	}
	var matched []json.RawMessage
	for _, raw := range all {
		if q != "" && !attendeeMatches(raw, q) {
			continue
		}
		matched = append(matched, raw)
	}
	items := []json.RawMessage{}
	if offset < len(matched) {
		end := offset + limit
		if end > len(matched) {
			end = len(matched)
		}
		items = matched[offset:end]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":  len(matched),
		"offset": offset,
		"limit":  limit,
		"items":  items,
	})
}

// attendeeMatches reports whether q (lowercased) occurs in any of the
// attendee's name parts, email, or confirmation number.
func attendeeMatches(raw json.RawMessage, q string) bool {
	var a map[string]any
	if err := json.Unmarshal(raw, &a); err != nil {
		return false
	}
	var fields []string
	add := func(m map[string]any, keys ...string) {
		for _, k := range keys {
			if s, ok := m[k].(string); ok {
				fields = append(fields, s)
			}
		}
	}
	nameKeys := []string{"firstName", "middleName", "lastName", "givenName", "familyName", "first", "middle", "last"}
	if nm, ok := a["name"].(map[string]any); ok {
		add(nm, nameKeys...)
	}
	add(a, nameKeys...)
	if ct, ok := a["contact"].(map[string]any); ok {
		add(ct, append(nameKeys, "email")...)
	}
	add(a, "email", "confirmationNumber")
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// handleCheckin checks in attendees. Request body (plan contract):
// {"attendeeIds": ["uuid", ...]}. The Cvent spec (openapi.json,
// POST /events/{id}/check-in, schema bulk-checkin) takes a BULK ARRAY of
// {"id", "checkIn"} objects instead — this handler maps the plan shape onto
// the spec, stamping each checkIn with the current time. At most 100
// attendees per call (spec limit). On success the event cache is
// invalidated so the next read re-fetches fresh check-in status.
func (h *cventHandlers) handleCheckin(w http.ResponseWriter, r *http.Request) {
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
	// bundle used); no fetch here — the UI always reads /event first, so a
	// missing cache means nothing to check in against.
	uuid, ok := h.cache.eventUUID(h.code)
	if !ok {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "event bundle not cached (fetch /api/cvent/event first)"})
		return
	}

	checkInAt := time.Now().UTC().Format(time.RFC3339)
	payload := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		payload = append(payload, map[string]string{"id": id, "checkIn": checkInAt})
	}
	status, raw, err := h.postCheckin(r.Context(), uuid, payload)
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
	h.cache.invalidate(h.code)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// postCheckin POSTs the spec's bulk-checkin array to /events/{uuid}/check-in
// and returns the upstream status and body.
func (h *cventHandlers) postCheckin(ctx context.Context, uuid string, payload []map[string]string) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("cvent check-in: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(h.client.base, "/")+"/events/"+uuid+"/check-in", strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, fmt.Errorf("cvent check-in: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	tok, err := h.client.token(ctx)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := h.client.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cvent check-in: %v", err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if rerr != nil {
		return 0, nil, fmt.Errorf("cvent check-in: reading body: %v", rerr)
	}
	return resp.StatusCode, raw, nil
}

// handleRepull forces a background re-fetch: invalidate the cache, then
// fetch on a detached 20s context (client disconnects must not cancel it).
// The request returns immediately; concurrent repulls for the same event
// report running:true instead of starting a second fetch.
func (h *cventHandlers) handleRepull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	h.mu.Lock()
	if h.running[h.code] {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"started": false, "running": true})
		return
	}
	h.running[h.code] = true
	h.mu.Unlock()

	h.cache.invalidate(h.code)
	fctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	go func() {
		defer cancel()
		_, err := h.cache.bundle(fctx, h.code)
		if err != nil {
			log.Printf("cvent repull %s: %v", h.code, err)
		}
		h.mu.Lock()
		delete(h.running, h.code)
		h.mu.Unlock()
	}()
	writeJSON(w, http.StatusOK, map[string]bool{"started": true})
}

// handleRepullStatus reports running (a background repull is in flight) and
// pulledAt (the cached bundle's pull time, null when the cache is empty).
func (h *cventHandlers) handleRepullStatus(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	running := h.running[h.code]
	h.mu.Unlock()

	var pulledAt *string
	h.cache.mu.Lock()
	if e, ok := h.cache.entries[h.code]; ok && e.bundle != nil {
		s := e.bundle.PulledAt
		pulledAt = &s
	}
	h.cache.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"running":  running,
		"pulledAt": pulledAt,
	})
}
