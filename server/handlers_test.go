package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestCventStack spins up an httptest "Cvent" upstream and wires a real
// cventClient + eventCache + cventHandlers against it, the same way main()
// does — so the handler tests exercise the real token/paging code paths.
// The upstream records every request path and returns per-path fixtures.
func newTestCventStack(t *testing.T, upstream *httptest.Server, eventID string) *cventHandlers {
	t.Helper()
	client := newCventClient(upstream.URL, "test-cid", "test-sec")
	ec := newEventCache(client)
	ec.ttl = 15 * time.Minute
	return newCventHandlers(eventID, client, ec)
}

// testUUID is a fixed, well-formed event uuid for fixtures.
const testCheckinUUID = "0a1b2c3d-4e5f-6a7b-8c9d-0e1f2a3b4c5d"

// testCventUpstream serves the two endpoints the check-in path touches:
// the OAuth token endpoint and the events check-in endpoint. It returns a
// minimal event fixture (code→uuid resolution) via GET /events?filter=….
func testCventUpstream(t *testing.T, checkInStatus int, checkInBody string) (*httptest.Server, *int32) {
	t.Helper()
	var checkInCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/oauth2/token":
			w.Write([]byte(`{"access_token":"test-token"}`))
		case r.URL.Path == "/events":
			w.Write([]byte(`{"items":[{"id":"` + testCheckinUUID + `","code":"TESTCODE01"}]}`))
		case r.URL.Path == "/events/"+testCheckinUUID+"/check-in":
			atomic.AddInt32(&checkInCalls, 1)
			if body, _ := json.Marshal(map[string]any{"ok": true}); checkInBody != "" {
				_ = body
				w.WriteHeader(checkInStatus)
				w.Write([]byte(checkInBody))
			} else {
				w.WriteHeader(checkInStatus)
			}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &checkInCalls
}

func doCheckin(t *testing.T, h *cventHandlers, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/cvent/events/TESTCODE01/checkin", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.route(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestCheckinHandlerSuccess(t *testing.T) {
	upstream, calls := testCventUpstream(t, http.StatusMultiStatus, `[{"id":"att-1"}]`)
	h := newTestCventStack(t, upstream, "TESTCODE01")

	// Prime the cache so eventUUID() has a resolved event.
	if _, err := h.cache.bundle(context.Background(), h.defaultCode); err != nil {
		t.Fatalf("prime bundle: %v", err)
	}

	code, out := doCheckin(t, h, `{"attendeeIds":["att-1","att-2"]}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, out)
	}
	if out["ok"] != true {
		t.Fatalf("ok = %v, want true", out["ok"])
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream check-in calls = %d, want 1", got)
	}
	// Cache must be invalidated so the next read re-fetches.
	// (We can't observe the fetch directly here without a counter on
	// /events, but eventUUID must now be false: the entry is gone.)
	if _, ok := h.cache.eventUUID(h.defaultCode); ok {
		t.Fatalf("cache was not invalidated after check-in")
	}
}

func TestCheckinHandlerInvalidBody(t *testing.T) {
	upstream, _ := testCventUpstream(t, http.StatusMultiStatus, "")
	h := newTestCventStack(t, upstream, "TESTCODE01")

	for name, body := range map[string]string{
		"not json":   `{`,
		"empty ids":  `{"attendeeIds":[]}`,
		"missing":    `{}`,
		"only blank": `{"attendeeIds":["  "]}`,
	} {
		code, _ := doCheckin(t, h, body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, code)
		}
	}
}

func TestCheckinHandlerTooMany(t *testing.T) {
	upstream, calls := testCventUpstream(t, http.StatusMultiStatus, "")
	h := newTestCventStack(t, upstream, "TESTCODE01")
	ids := make([]string, 0, 101)
	for i := range 101 {
		ids = append(ids, `"id-`+itoa(i)+`"`)
	}
	code, _ := doCheckin(t, h, `{"attendeeIds":[`+strings.Join(ids, ",")+`]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("upstream must not be called, got %d calls", got)
	}
}

func TestCheckinHandlerNoCache(t *testing.T) {
	upstream, calls := testCventUpstream(t, http.StatusMultiStatus, "")
	h := newTestCventStack(t, upstream, "TESTCODE01")
	// No bundle fetched yet → no cached uuid → 502, no upstream check-in.
	code, out := doCheckin(t, h, `{"attendeeIds":["att-1"]}`)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
	if !strings.Contains(out["error"].(string), "not cached") {
		t.Fatalf("error = %v, want 'not cached' hint", out["error"])
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("upstream check-in calls = %d, want 0", got)
	}
}

func TestCheckinHandlerUpstreamError(t *testing.T) {
	upstream, _ := testCventUpstream(t, http.StatusForbidden, `{"error":"missing scope"}`)
	h := newTestCventStack(t, upstream, "TESTCODE01")
	if _, err := h.cache.bundle(context.Background(), h.defaultCode); err != nil {
		t.Fatalf("prime bundle: %v", err)
	}
	code, out := doCheckin(t, h, `{"attendeeIds":["att-1"]}`)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
	if !strings.Contains(out["error"].(string), "missing scope") {
		t.Fatalf("error = %v, want upstream error text", out["error"])
	}
	// On Cvent error the cache must NOT be invalidated.
	if _, ok := h.cache.eventUUID(h.defaultCode); !ok {
		t.Fatalf("cache must survive a failed check-in")
	}
}

// itoa is a tiny int→string helper for test fixtures (avoid fmt import noise
// in table literals).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func TestAttendeesSearchAndPaging(t *testing.T) {
	// Build a bundle directly (no upstream needed for the search logic).
	client := newCventClient("http://127.0.0.1:0", "c", "s")
	ec := newEventCache(client)
	h := newCventHandlers("TESTCODE01", client, ec)
	b := &EventBundle{Code: "TESTCODE01", PulledAt: time.Now().UTC().Format(time.RFC3339)}
	b.Event = json.RawMessage(`{"id":"` + testCheckinUUID + `"}`)
	b.Attendees = json.RawMessage(`[
		{"id":"a1","confirmationNumber":"CONF123","contact":{"firstName":"Ada","lastName":"Lovelace","email":"ada@example.com"}},
		{"id":"a2","confirmationNumber":"CONF456","contact":{"firstName":"Grace","lastName":"Hopper","email":"grace@example.com"}},
		{"id":"a3","name":{"firstName":"Alan","middleName":"M.","lastName":"Turing"},"contact":{"email":"alan@example.com"}},
		{"id":"a4","confirmationNumber":"CONF789","contact":{"firstName":"Edsger","lastName":"Dijkstra","email":"edsger@example.com"}}
	]`)
	ec.mu.Lock()
	ec.store(h.defaultCode, b)
	ec.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/attendees?q=lovel", nil)
	rec := httptest.NewRecorder()
	h.route(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Total  int               `json:"total"`
		Offset int               `json:"offset"`
		Limit  int               `json:"limit"`
		Items  []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 || out.Limit != 50 || out.Offset != 0 {
		t.Fatalf("got %+v, want total=1 items=1 limit=50", out)
	}
	var it struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Items[0], &it); err != nil || it.ID != "a1" {
		t.Fatalf("item = %s, want a1", it.ID)
	}

	// No query: paging.
	req = httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/attendees?limit=2&offset=1", nil)
	rec = httptest.NewRecorder()
	h.route(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 4 || len(out.Items) != 2 || out.Limit != 2 || out.Offset != 1 {
		t.Fatalf("got %+v, want total=4 items=2 limit=2 offset=1", out)
	}

	// limit clamps to 200.
	req = httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/attendees?limit=500", nil)
	rec = httptest.NewRecorder()
	h.route(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Limit != 200 {
		t.Fatalf("limit = %d, want 200", out.Limit)
	}

	// Email search is case-insensitive.
	req = httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/attendees?q=GRACE@EXAMPLE.COM", nil)
	rec = httptest.NewRecorder()
	h.route(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("email search got total=%d items=%d, want 1", out.Total, len(out.Items))
	}
}

func TestAttendeeNameJoining(t *testing.T) {
	cases := []struct {
		in   json.RawMessage
		want string
	}{
		{json.RawMessage(`{"id":"1","name":{"firstName":"Ada","middleName":"Byron","lastName":"Lovelace"}}`), "Ada Byron. Lovelace"},
		{json.RawMessage(`{"id":"2","name":{"givenName":"Ada","familyName":"Lovelace"}}`), "Ada Lovelace"},
		{json.RawMessage(`{"id":"3","firstName":"Grace","lastName":"Hopper"}`), "Grace Hopper"},
		{json.RawMessage(`{"id":"4","contact":{"firstName":"Alan","middleName":"M","lastName":"Turing"}}`), "Alan M. Turing"},
		{json.RawMessage(`{"id":"5","contact":{"email":"x@y.z"}}`), ""},
	}
	for _, tc := range cases {
		id, name := attendeeIDAndName(tc.in)
		if name != tc.want {
			t.Fatalf("name for %s = %q, want %q", tc.in, name, tc.want)
		}
		_ = id
	}
}

func TestPaymentsHandlerZeroRows(t *testing.T) {
	client := newCventClient("http://127.0.0.1:0", "c", "s")
	ec := newEventCache(client)
	h := newCventHandlers("TESTCODE01", client, ec)
	b := &EventBundle{Code: "TESTCODE01", PulledAt: time.Now().UTC().Format(time.RFC3339)}
	b.Event = json.RawMessage(`{"id":"` + testCheckinUUID + `"}`)
	ec.mu.Lock()
	ec.store(h.defaultCode, b)
	ec.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/payments", nil)
	rec := httptest.NewRecorder()
	h.route(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var sum PaymentSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sum.Totals.Ordered != 0 || sum.Totals.Paid != 0 || sum.Totals.Due != 0 || sum.Totals.Refunded != 0 {
		t.Fatalf("totals = %+v, want all zero", sum.Totals)
	}
	if len(sum.Orders) != 0 || len(sum.Cancelled) != 0 {
		t.Fatalf("orders/cancelled not empty: %+v", sum)
	}
}

func TestRepullStatusAndRepull(t *testing.T) {
	upstream, _ := testCventUpstream(t, http.StatusMultiStatus, "")
	h := newTestCventStack(t, upstream, "TESTCODE01")

	// Before anything: not running, no pulledAt.
	req := httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/repull-status", nil)
	rec := httptest.NewRecorder()
	h.route(rec, req)
	var st struct {
		Running  bool    `json:"running"`
		PulledAt *string `json:"pulledAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Running || st.PulledAt != nil {
		t.Fatalf("initial status = %+v, want running=false pulledAt=nil", st)
	}

	// First repull: started=true, then the background fetch lands.
	req = httptest.NewRequest(http.MethodPost, "/api/cvent/events/TESTCODE01/repull", nil)
	rec = httptest.NewRecorder()
	h.route(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["started"] != true {
		t.Fatalf("started = %v, want true", out["started"])
	}

	// Wait for the background fetch to complete (upstream is httptest: fast).
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		running := h.running[h.defaultCode]
		h.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("repull still running after 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.cache.bundle(context.Background(), h.defaultCode); err != nil {
		t.Fatalf("post-repull bundle: %v", err)
	}

	// Status now: not running, pulledAt set.
	req = httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/repull-status", nil)
	rec = httptest.NewRecorder()
	h.route(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Running || st.PulledAt == nil || *st.PulledAt == "" {
		t.Fatalf("status = %+v, want running=false pulledAt set", st)
	}
}

func TestRouteNotFound(t *testing.T) {
	client := newCventClient("http://127.0.0.1:0", "c", "s")
	h := newCventHandlers("TESTCODE01", client, newEventCache(client))
	req := httptest.NewRequest(http.MethodGet, "/api/cvent/events/TESTCODE01/nope", nil)
	rec := httptest.NewRecorder()
	h.route(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("body = %s, want 'not found'", rec.Body)
	}
}
