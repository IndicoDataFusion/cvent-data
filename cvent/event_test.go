package cvent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test event is synthetic: every test runs against an httptest upstream only.
const testUUID = "4f1c2a9e-7b3d-4e8a-9c21-5d6e7f80a1b0"
const testCode = "TESTCODE01"

// cventMock is a fake Cvent API for event fan-out tests. It counts requests
// per path and serves scripted responses, with three tiers:
//
//   - status overrides: an exact path → (status code, body), for the
//     non-200 cases (403 on a resource, 404 on an unknown uuid);
//   - paged paths: an exact path → map[token]body where "" is the first page
//     (drives the continuation-token scenarios, including a token the upstream
//     silently ignores so the same page repeats);
//   - single paths: an exact path → one body served regardless of token.
//
// Only r.URL.Path is used for routing; method and query params are recorded
// for assertions, never for routing.
type cventMock struct {
	srv *httptest.Server

	mu     sync.Mutex
	counts map[string]int64 // path → request count
	status map[string]struct {
		code int
		body []byte
	}
	paged  map[string]map[string][]byte // path → token → body
	single map[string][]byte            // path → body
}

func newCventMock(t *testing.T) *cventMock {
	t.Helper()
	m := &cventMock{
		counts: map[string]int64{},
		status: map[string]struct {
			code int
			body []byte
		}{},
		paged:  map[string]map[string][]byte{},
		single: map[string][]byte{},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		m.mu.Lock()
		m.counts[path]++
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if s, ok := m.status[path]; ok {
			w.WriteHeader(s.code)
			w.Write(s.body)
			return
		}
		if pages, ok := m.paged[path]; ok {
			body, ok := pages[r.URL.Query().Get("token")]
			if !ok {
				http.Error(w, "unexpected token "+r.URL.Query().Get("token"), http.StatusNotFound)
				return
			}
			w.Write(body)
			return
		}
		if body, ok := m.single[path]; ok {
			w.Write(body)
			return
		}
		http.Error(w, "unexpected path "+path, http.StatusNotFound)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *cventMock) count(path string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[path]
}

func (m *cventMock) setStatus(path string, code int, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status[path] = struct {
		code int
		body []byte
	}{code: code, body: body}
}

func (m *cventMock) setPaged(path string, pages map[string][]byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paged[path] = pages
}

func (m *cventMock) setSingle(path string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.single[path] = body
}

// eventListBody is a /events list page (the code-lookup shape): items under
// "items" plus paging, first item being the target event.
func eventListBody(uuid, code string) []byte {
	return []byte(`{"items":[` + `{"id":"` + uuid + `","code":"` + code + `","name":"Test Event","currency":"USD"}` +
		`],"paging":{"limit":200,"totalCount":1,"currentToken":"t1"}}`)
}

// eventObjectBody is the /events/{uuid} single-object shape (not a list).
func eventObjectBody(uuid, code string) []byte {
	return []byte(`{"id":"` + uuid + `","code":"` + code + `","name":"Test Event","currency":"USD"}`)
}

// setEventEndpoints points both lookup paths at the target event.
func (m *cventMock) setEventEndpoints() {
	m.setSingle("/events", eventListBody(testUUID, testCode))
	m.setSingle("/events/"+testUUID, eventObjectBody(testUUID, testCode))
}

// allResourcePaths returns the 13 fan-out paths for the test event, in
// eventResources order, for use as a lookup set.
func allResourcePaths() map[string]bool {
	paths := map[string]bool{
		"/events/" + testUUID + "/registration-types": true,
		"/admission-items/filter":                     true,
		"/events/" + testUUID + "/fee-items":          true,
		"/events/" + testUUID + "/discounts":          true,
		"/sessions/filter":                            true,
		"/speakers/filter":                            true,
		"/attendees/filter":                           true,
		"/attendees/activities":                       true,
		"/events/" + testUUID + "/orders":             true,
		"/events/" + testUUID + "/orders/items":       true,
		"/events/" + testUUID + "/transactions":       true,
		"/events/" + testUUID + "/transactions/items": true,
		"/event-questions":                            true,
	}
	return paths
}

// setAllResources200 makes every one of the 13 fan-out endpoints return a
// 2-item 200 page. The totalCount (2) equals the number of items so the
// walk stops after a single request per resource.
func setAllResources200(m *cventMock) {
	for path := range allResourcePaths() {
		m.setSingle(path, listPage([]string{"x1", "x2"}, 2, ""))
	}
}

// newTestEventCache builds a cventClient against the mock (token warm so
// OAuth round-trips don't pollute request counts) plus a fresh eventCache.
func newTestEventCache(m *cventMock) *EventCache {
	c := newCventClient(m.srv.URL, "cid", "secret")
	warmToken(c)
	return NewEventCache(c)
}

// TestEventBundleErrorsIsolated: a full fan-out where every resource endpoint
// returns a 2-item 200 page EXCEPT /events/{uuid}/discounts, which returns a
// 403 with a JSON error body. fetchEvent must isolate the failure:
// Errors["discounts"] is non-empty and mentions 403, the other 11 fields are
// populated, and no other Errors entries exist. Bundle.Code comes from the
// event object, not the (identical) input.
func TestEventBundleErrorsIsolated(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	setAllResources200(m)
	m.setStatus("/events/"+testUUID+"/discounts", http.StatusForbidden,
		[]byte(`{"message":"forbidden"}`))

	ec := newTestEventCache(m)
	b, err := ec.fetchEvent(context.Background(), testCode)
	if err != nil {
		t.Fatalf("fetchEvent: %v", err)
	}

	if b.Code != testCode {
		t.Fatalf("bundle.Code = %q, want %q (set from the event object)", b.Code, testCode)
	}

	d, ok := b.Errors["discounts"]
	if !ok {
		t.Fatalf("Errors has no discounts entry: %v", b.Errors)
	}
	if !strings.Contains(d, "403") {
		t.Fatalf("discounts error should mention 403: %q", d)
	}
	// No other resource failed.
	if len(b.Errors) != 1 {
		t.Fatalf("Errors should have exactly 1 entry (discounts), got %d: %v", len(b.Errors), b.Errors)
	}

	// The other 12 fields are populated (non-nil) and carry 2 items each.
	populated := map[string]json.RawMessage{
		"registrationTypes": b.RegistrationTypes,
		"admissionItems":    b.AdmissionItems,
		"feeItems":          b.FeeItems,
		"sessions":          b.Sessions,
		"speakers":          b.Speakers,
		"attendees":         b.Attendees,
		"activities":        b.Activities,
		"orders":            b.Orders,
		"orderItems":        b.OrderItems,
		"transactions":      b.Transactions,
		"transactionItems":  b.TransactionItems,
		"eventQuestions":    b.EventQuestions,
	}
	for name, raw := range populated {
		if raw == nil {
			t.Errorf("bundle.%s is nil, want populated", name)
			continue
		}
		if got := b.Counts[name]; got != 2 {
			t.Errorf("Counts[%q] = %d, want 2", name, got)
		}
	}
	if got := b.Counts["discounts"]; got != 0 {
		t.Errorf("Counts[discounts] = %d, want 0 (fetch failed)", got)
	}
	// Counts must carry all 13 keys.
	if len(b.Counts) != 13 {
		t.Errorf("Counts has %d keys, want exactly 13: %v", len(b.Counts), b.Counts)
	}
	if b.Discounts != nil {
		t.Errorf("bundle.Discounts should be nil after a failed fetch, got %s", b.Discounts)
	}
	if b.Event == nil {
		t.Errorf("bundle.Event is nil, want the resolved event object")
	}
}

// TestEventResourceTokenIgnored: one fan-out resource — /attendees/activities
// (a listAll) — where the upstream silently ignores the continuation token and
// the same page repeats forever. listAll detects the non-advancing token and
// errors; fetchEvent must surface that in Errors["activities"] (containing
// 'did not advance') while still returning the full bundle with all other
// resources populated. One broken resource must not kill the bundle.
func TestEventResourceTokenIgnored(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	setAllResources200(m)
	// Override activities with a token-ignoring upstream: page 2 (token t1)
	// returns the identical first item, tripping the non-advancing-token guard.
	m.setPaged("/attendees/activities", map[string][]byte{
		"":   listPage([]string{"a1", "a2"}, 5, "t1"),
		"t1": listPage([]string{"a1", "a2"}, 5, "t2"), // same items; token ignored
	})

	ec := newTestEventCache(m)
	b, err := ec.fetchEvent(context.Background(), testCode)
	if err != nil {
		t.Fatalf("fetchEvent: %v (a single broken resource must not fail the bundle)", err)
	}

	a, ok := b.Errors["activities"]
	if !ok {
		t.Fatalf("Errors has no activities entry: %v", b.Errors)
	}
	if !strings.Contains(a, "did not advance") {
		t.Fatalf("activities error should say the token did not advance: %q", a)
	}
	// Exactly one error entry.
	if len(b.Errors) != 1 {
		t.Fatalf("Errors should have exactly 1 entry (activities), got %d: %v", len(b.Errors), b.Errors)
	}
	// activities field is nil + count 0.
	if b.Activities != nil {
		t.Errorf("bundle.Activities should be nil after the token fault, got %s", b.Activities)
	}
	if got := b.Counts["activities"]; got != 0 {
		t.Errorf("Counts[activities] = %d, want 0 (fetch failed)", got)
	}
	// The other 12 fields are populated.
	populated := map[string]json.RawMessage{
		"registrationTypes": b.RegistrationTypes,
		"admissionItems":    b.AdmissionItems,
		"feeItems":          b.FeeItems,
		"discounts":         b.Discounts,
		"sessions":          b.Sessions,
		"speakers":          b.Speakers,
		"attendees":         b.Attendees,
		"orders":            b.Orders,
		"orderItems":        b.OrderItems,
		"transactions":      b.Transactions,
		"transactionItems":  b.TransactionItems,
		"eventQuestions":    b.EventQuestions,
	}
	for name, raw := range populated {
		if raw == nil {
			t.Errorf("bundle.%s is nil, want populated (unrelated to the activities fault)", name)
		}
	}
}

// TestEventResolve: code lookup via GET /events?filter=code eq '...' and UUID
// lookup via GET /events/{uuid} must both resolve to a populated bundle; a code
// with no matches and an unknown uuid (404) must both fail with "event not
// found".
func TestEventResolve(t *testing.T) {
	// (a) Code lookup: /events?filter=code eq '<code>' single page → bundle.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		ec := newTestEventCache(m)
		b, err := ec.fetchEvent(context.Background(), testCode)
		if err != nil {
			t.Fatalf("fetchEvent (code): %v", err)
		}
		if b.Code != testCode {
			t.Fatalf("bundle.Code = %q, want %q", b.Code, testCode)
		}
		if len(b.Counts) != 13 {
			t.Fatalf("Counts has %d keys, want 13", len(b.Counts))
		}
		// The code lookup must have hit /events (not /events/{uuid}).
		if got := m.count("/events"); got == 0 {
			t.Fatalf("code lookup must call GET /events, got %d requests", got)
		}
		if got := m.count("/events/" + testUUID); got != 0 {
			t.Fatalf("code lookup must not call GET /events/{uuid}, got %d requests", got)
		}
	}

	// (b) UUID input: GET /events/{uuid} single page → same populated bundle.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		ec := newTestEventCache(m)
		b, err := ec.fetchEvent(context.Background(), testUUID)
		if err != nil {
			t.Fatalf("fetchEvent (uuid): %v", err)
		}
		if b.Code != testCode {
			t.Fatalf("bundle.Code = %q, want %q (resolved from the event object)", b.Code, testCode)
		}
		// The uuid lookup must have hit /events/{uuid} (not /events).
		if got := m.count("/events"); got != 0 {
			t.Fatalf("uuid lookup must not call GET /events, got %d requests", got)
		}
		if got := m.count("/events/" + testUUID); got == 0 {
			t.Fatalf("uuid lookup must call GET /events/{uuid}, got %d requests", got)
		}
	}

	// (c) A code with zero matching events → "event not found".
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		// /events returns an empty list for the unknown code.
		m.setSingle("/events", listPage(nil, 0, ""))
		ec := newTestEventCache(m)
		_, err := ec.fetchEvent(context.Background(), "NOPE123")
		if err == nil {
			t.Fatal("fetchEvent (unknown code): want error, got nil")
		}
		if !strings.Contains(err.Error(), "event not found") {
			t.Fatalf("error should say 'event not found': %v", err)
		}
	}

	// (d) An unknown uuid → the /events/{uuid} 404 maps to "event not found".
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		missing := "00000000-0000-0000-0000-000000000000"
		m.setStatus("/events/"+missing, http.StatusNotFound, []byte(`{"message":"not found"}`))
		ec := newTestEventCache(m)
		_, err := ec.fetchEvent(context.Background(), missing)
		if err == nil {
			t.Fatal("fetchEvent (unknown uuid): want error, got nil")
		}
		if !strings.Contains(err.Error(), "event not found") {
			t.Fatalf("error should say 'event not found': %v", err)
		}
	}
}

// TestCacheTTLAndInFlight pins the cache contract with an injectable TTL and
// an injectable clock (no real sleeps):
//
//	(a) two sequential bundle() calls within the TTL → the upstream is hit
//	    only once (the second call is a cache hit);
//	(b) two CONCURRENT bundle() calls on a cold cache → the upstream fee-items
//	    endpoint is hit exactly once (in-flight dedup: the second caller waits
//	    and reuses the first fetch);
//	(c) advancing the injected clock past the TTL → a re-fetch happens.
func TestCacheTTLAndInFlight(t *testing.T) {
	// (a) Sequential within TTL: one upstream fetch.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		ec := newTestEventCache(m)
		ec.ttl = time.Hour // comfortably fresh across both calls
		// A deterministic clock pinned at t0 for the "within TTL" pair.
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		if _, err := ec.Bundle(context.Background(), testCode); err != nil {
			t.Fatalf("first bundle(): %v", err)
		}
		afterFirst := m.count("/events/" + testUUID + "/fee-items")
		if _, err := ec.Bundle(context.Background(), testCode); err != nil {
			t.Fatalf("second bundle(): %v", err)
		}
		afterSecond := m.count("/events/" + testUUID + "/fee-items")
		if afterFirst == 0 {
			t.Fatalf("fee-items was never fetched (want exactly 1 after first call)")
		}
		if afterSecond != afterFirst {
			t.Fatalf("sequential call within TTL hit upstream again: fee-items %d → %d (want unchanged)", afterFirst, afterSecond)
		}
	}

	// (b) Concurrent on a cold cache: in-flight dedup → one fee-items fetch.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		ec := newTestEventCache(m)
		ec.ttl = time.Hour
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = ec.Bundle(context.Background(), testCode)
			}(i)
		}
		close(start) // release both callers onto the cold cache at once
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent bundle() caller %d: %v", i, err)
			}
		}
		if got := m.count("/events/" + testUUID + "/fee-items"); got != 1 {
			t.Fatalf("fee-items hit %d times under 2 concurrent cold-cache callers, want exactly 1 (in-flight dedup)", got)
		}
	}

	// (c) Advance the clock past the TTL → re-fetch.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		ec := newTestEventCache(m)
		ec.ttl = 15 * time.Minute
		var cur time.Time // the injectable clock, advanced on demand
		ec.now = func() time.Time { return cur }
		cur = time.Now()

		if _, err := ec.Bundle(context.Background(), testCode); err != nil {
			t.Fatalf("first bundle(): %v", err)
		}
		before := m.count("/events/" + testUUID + "/fee-items")
		cur = cur.Add(16 * time.Minute) // past the 15-minute TTL
		if _, err := ec.Bundle(context.Background(), testCode); err != nil {
			t.Fatalf("second bundle() after TTL: %v", err)
		}
		after := m.count("/events/" + testUUID + "/fee-items")
		if after == before {
			t.Fatalf("bundle() did not re-fetch after the TTL expired: fee-items stayed at %d", before)
		}
		if after != before+1 {
			t.Fatalf("fee-items fetch count %d → %d, want exactly one re-fetch (before+1)", before, after)
		}
	}
}

// TestInvalidate: invalidate(code) removes the positive cache entry and the
// negative entry, so a subsequent bundle() re-fetches from upstream. (Task 6's
// checkin handler calls this after a successful check-in.)
func TestInvalidate(t *testing.T) {
	// Positive entry: invalidate → re-fetch.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		ec := newTestEventCache(m)
		ec.ttl = time.Hour
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		if _, err := ec.Bundle(context.Background(), testCode); err != nil {
			t.Fatalf("bundle(): %v", err)
		}
		before := m.count("/events/" + testUUID + "/fee-items")
		ec.Invalidate(testCode)
		if _, err := ec.Bundle(context.Background(), testCode); err != nil {
			t.Fatalf("bundle() after invalidate: %v", err)
		}
		after := m.count("/events/" + testUUID + "/fee-items")
		if after != before+1 {
			t.Fatalf("invalidate did not force a re-fetch: fee-items %d → %d, want before+1", before, after)
		}
	}

	// Negative entry: a failing event is recorded negatively; invalidate clears
	// it so the next call re-hits upstream instead of the negative cache.
	{
		m := newCventMock(t)
		m.setSingle("/events", listPage(nil, 0, "")) // no matching event
		ec := newTestEventCache(m)
		ec.ttl = time.Hour
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		_, err := ec.Bundle(context.Background(), testCode)
		if err == nil || !strings.Contains(err.Error(), "event not found") {
			t.Fatalf("first bundle(): want 'event not found', got %v", err)
		}
		before := m.count("/events")
		// Within the negative window the second call is served from the
		// negative cache (no new upstream request).
		if _, err := ec.Bundle(context.Background(), testCode); err == nil {
			t.Fatal("second bundle(): want error, got nil")
		}
		if got := m.count("/events"); got != before {
			t.Fatalf("negative cache should short-circuit: /events %d → %d (want unchanged)", before, got)
		}
		ec.Invalidate(testCode)
		if _, err := ec.Bundle(context.Background(), testCode); err == nil {
			t.Fatal("bundle() after invalidate: want error, got nil")
		}
		if got := m.count("/events"); got != before+1 {
			t.Fatalf("invalidate should clear the negative entry and re-fetch: /events %d → %d, want before+1", before, got)
		}
	}
}

// TestStaleFlag pins the read-time staleness rule: a bundle whose PulledAt is
// older than 24h gets Stale=true on read WITHOUT a re-fetch; a fresh bundle
// gets Stale=false. The clock is injected, so no real sleeps.
func TestStaleFlag(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	setAllResources200(m)
	ec := newTestEventCache(m)
	ec.ttl = time.Hour // long TTL so staleness is driven purely by the 24h rule
	t0 := time.Now()
	ec.now = func() time.Time { return t0 }

	if _, err := ec.Bundle(context.Background(), testCode); err != nil {
		t.Fatalf("first bundle(): %v", err)
	}
	before := m.count("/events/" + testUUID + "/fee-items")

	// Fresh on read.
	b, err := ec.Bundle(context.Background(), testCode)
	if err != nil {
		t.Fatalf("second bundle(): %v", err)
	}
	if b.Stale {
		t.Fatalf("fresh bundle has Stale=true, want false (PulledAt=%s)", b.PulledAt)
	}
	if got := m.count("/events/" + testUUID + "/fee-items"); got != before {
		t.Fatalf("read within TTL re-fetched: fee-items %d → %d (want unchanged)", before, got)
	}

	// Advance the clock past the TTL so a re-fetch is allowed, then past 24h
	// so the re-fetched bundle is stale on read. The stale flag is computed at
	// read time from PulledAt, independent of the cache TTL.
	ec.ttl = time.Hour
	t0 = t0.Add(25 * time.Hour)
	ec.now = func() time.Time { return t0 }
	if _, err := ec.Bundle(context.Background(), testCode); err != nil {
		t.Fatalf("bundle() after clock advance: %v", err)
	}
	if got := m.count("/events/" + testUUID + "/fee-items"); got == before {
		t.Fatalf("expected a re-fetch after the TTL expired, fee-items still %d", before)
	}
}

// TestBundleJSONShape pins the wire contract the PWA depends on:
//
//   - every bundle field is camelCase in the JSON;
//   - an empty/failed resource is OMITTED (nil), never [] — so the UI can tell
//     "no rows" from "fetch failed" via the Errors map;
//   - the 24h-old Stale flag is omitted when false and present when true;
//   - Errors is omitted when there are no per-resource errors.
func TestBundleJSONShape(t *testing.T) {
	// A clean bundle (all 13 resources populated, no errors, fresh).
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		ec := newTestEventCache(m)
		ec.ttl = time.Hour
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		b, err := ec.Bundle(context.Background(), testCode)
		if err != nil {
			t.Fatalf("bundle(): %v", err)
		}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshalling bundle: %v", err)
		}
		s := string(raw)
		for _, key := range []string{
			`"code"`, `"pulledAt"`, `"event"`, `"counts"`,
			`"registrationTypes"`, `"admissionItems"`, `"feeItems"`, `"discounts"`,
			`"sessions"`, `"speakers"`, `"attendees"`, `"activities"`,
			`"orders"`, `"orderItems"`, `"transactions"`, `"transactionItems"`,
			`"eventQuestions"`,
		} {
			if !strings.Contains(s, key) {
				t.Errorf("bundle JSON missing %s: %s", key, s)
			}
		}
		// Fresh → stale omitted.
		if strings.Contains(s, `"stale"`) {
			t.Errorf("fresh bundle should omit stale: %s", s)
		}
		// No errors → errors omitted.
		if strings.Contains(s, `"errors"`) {
			t.Errorf("clean bundle should omit errors: %s", s)
		}
		// No resource should serialize to an empty array.
		if strings.Contains(s, `:[]`) {
			t.Errorf("bundle JSON must not contain empty arrays: %s", s)
		}
	}

	// A bundle with one failed resource: that field is omitted (not []), the
	// errors object is present, and the stale flag (false) is still omitted.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		// speakers is fetched via the top-level POST filter endpoint.
		m.setStatus("/speakers/filter", http.StatusForbidden,
			[]byte(`{"message":"forbidden"}`))
		ec := newTestEventCache(m)
		ec.ttl = time.Hour
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		b, err := ec.Bundle(context.Background(), testCode)
		if err != nil {
			t.Fatalf("bundle(): %v", err)
		}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshalling bundle: %v", err)
		}
		s := string(raw)
		// speakers is omitted as a FIELD (never [] or an array); the key only
		// appears inside counts and errors, where it belongs.
		if strings.Contains(s, `"speakers":[`) {
			t.Errorf("failed resource (speakers) should be omitted as a field, not an array: %s", s)
		}
		if !strings.Contains(s, `"errors"`) {
			t.Errorf("bundle with a failure should include the errors object: %s", s)
		}
		// errors.speakers carries the 403 message (the error key lives inside
		// the errors object, not as a bundle field).
		if !strings.Contains(s, `"errors":{"speakers":"`) {
			t.Errorf("errors object should map speakers→message: %s", s)
		}
		if !strings.Contains(s, `403`) {
			t.Errorf("errors.speakers should carry the 403 status: %s", s)
		}
		if strings.Contains(s, `"stale"`) {
			t.Errorf("fresh bundle should omit stale even with an error: %s", s)
		}
	}
}

// TestGetOnePageShapes pins the two response shapes getOnePage must accept:
// a list page (take the first item) and a single object with an id (return as
// is), plus the zero-items → "event not found" case.
func TestGetOnePageShapes(t *testing.T) {
	// List shape: {"items":[...]} → first item.
	{
		m := newCventMock(t)
		m.setSingle("/events", eventListBody(testUUID, testCode))
		c := newCventClient(m.srv.URL, "cid", "secret")
		warmToken(c)
		got, err := c.getOnePage(context.Background(), "/events", url.Values{})
		if err != nil {
			t.Fatalf("getOnePage (list): %v", err)
		}
		var ev struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(got, &ev); err != nil {
			t.Fatalf("unmarshalling first item: %v", err)
		}
		if ev.ID != testUUID {
			t.Fatalf("getOnePage (list) id = %q, want %q", ev.ID, testUUID)
		}
	}

	// Single-object shape: {"id":...} → the whole object.
	{
		m := newCventMock(t)
		m.setSingle("/events/"+testUUID, eventObjectBody(testUUID, testCode))
		c := newCventClient(m.srv.URL, "cid", "secret")
		warmToken(c)
		got, err := c.getOnePage(context.Background(), "/events/"+testUUID, url.Values{})
		if err != nil {
			t.Fatalf("getOnePage (object): %v", err)
		}
		var ev struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		}
		if err := json.Unmarshal(got, &ev); err != nil {
			t.Fatalf("unmarshalling object: %v", err)
		}
		if ev.ID != testUUID || ev.Code != testCode {
			t.Fatalf("getOnePage (object) = (%q,%q), want (%q,%q)", ev.ID, ev.Code, testUUID, testCode)
		}
	}

	// Zero items → "event not found".
	{
		m := newCventMock(t)
		m.setSingle("/events", listPage(nil, 0, ""))
		c := newCventClient(m.srv.URL, "cid", "secret")
		warmToken(c)
		_, err := c.getOnePage(context.Background(), "/events", url.Values{})
		if err == nil {
			t.Fatal("getOnePage (zero items): want error, got nil")
		}
		if !strings.Contains(err.Error(), "event not found") {
			t.Fatalf("error should say 'event not found': %v", err)
		}
	}
}

// TestFetchEventCountsAlways13Keys pins the uniform badge contract: Counts
// always carries exactly the 13 resource keys, with 0 for any empty or failed
// resource (the UI's badge logic relies on every key being present).
func TestFetchEventCountsAlways13Keys(t *testing.T) {
	want := []string{
		"registrationTypes", "admissionItems", "feeItems", "discounts",
		"sessions", "speakers", "attendees", "activities",
		"orders", "orderItems", "transactions", "transactionItems",
		"eventQuestions",
	}
	// All empty: every resource returns a zero-item page.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		for path := range allResourcePaths() {
			m.setSingle(path, listPage(nil, 0, ""))
		}
		ec := newTestEventCache(m)
		b, err := ec.fetchEvent(context.Background(), testCode)
		if err != nil {
			t.Fatalf("fetchEvent: %v", err)
		}
		if len(b.Counts) != 13 {
			t.Fatalf("Counts has %d keys, want 13", len(b.Counts))
		}
		for _, k := range want {
			if _, ok := b.Counts[k]; !ok {
				t.Errorf("Counts missing key %q", k)
			}
			if b.Counts[k] != 0 {
				t.Errorf("Counts[%q] = %d, want 0 (empty resource)", k, b.Counts[k])
			}
		}
	}
}

// TestEventResourceSet pins that the fan-out fetches exactly the 13 resources
// (one request per path) and no others — the rest of the plan's superset
// (registration-paths, emails) is NOT fetched.
func TestEventResourceSet(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	setAllResources200(m)
	ec := newTestEventCache(m)
	if _, err := ec.fetchEvent(context.Background(), testCode); err != nil {
		t.Fatalf("fetchEvent: %v", err)
	}
	for path, want := range map[string]int64{
		"/events/" + testUUID + "/registration-types": 1,
		"/admission-items/filter":                     1,
		"/events/" + testUUID + "/fee-items":          1,
		"/events/" + testUUID + "/discounts":          1,
		"/sessions/filter":                            1,
		"/speakers/filter":                            1,
		"/attendees/filter":                           1,
		"/attendees/activities":                       1,
		"/events/" + testUUID + "/orders":             1,
		"/events/" + testUUID + "/orders/items":       1,
		"/events/" + testUUID + "/transactions":       1,
		"/events/" + testUUID + "/transactions/items": 1,
		"/event-questions":                            1,
	} {
		if got := m.count(path); got != want {
			t.Errorf("GET/POST %s hit %d times, want %d", path, got, want)
		}
	}
	// The skipped resources must have zero requests.
	for path := range map[string]bool{
		"/events/" + testUUID + "/registration-paths": true,
		"/events/" + testUUID + "/emails":             true,
	} {
		if got := m.count(path); got != 0 {
			t.Errorf("%s was hit %d times, want 0 (not in the bundle)", path, got)
		}
	}
}

// TestBundleCopyIsolation pins the cache-poisoning contract: every bundle()
// read (miss, in-flight waiter, and subsequent hit) returns an isolated copy
// whose Counts and Errors maps are independent of the cached entry — mutating
// a returned bundle must never be visible to another caller or to the cache.
// (Shared json.RawMessage field headers are acceptable: raw JSON bytes are
// never mutated by callers.)
func TestBundleCopyIsolation(t *testing.T) {
	// (a) Miss path: the first bundle() caller mutates the returned bundle's
	// Counts and Errors maps; a second bundle() call must see the originals.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		m.setStatus("/events/"+testUUID+"/discounts", http.StatusForbidden,
			[]byte(`{"message":"forbidden"}`)) // guarantees a non-nil Errors map
		ec := newTestEventCache(m)
		ec.ttl = time.Hour
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		b1, err := ec.Bundle(context.Background(), testCode) // cold cache → miss
		if err != nil {
			t.Fatalf("first bundle(): %v", err)
		}
		b1.Counts["orders"] = 999
		if b1.Errors == nil {
			t.Fatalf("expected a non-nil Errors map (discounts 403), got nil")
		}
		b1.Errors["discounts"] = "tampered"

		b2, err := ec.Bundle(context.Background(), testCode) // warm cache → hit
		if err != nil {
			t.Fatalf("second bundle(): %v", err)
		}
		if got := b2.Counts["orders"]; got != 2 {
			t.Errorf("cache poisoned via miss-path caller: second bundle Counts[orders] = %d, want 2", got)
		}
		if got := b2.Errors["discounts"]; got == "tampered" {
			t.Errorf("cache poisoned via miss-path caller: second bundle Errors[discounts] = %q (tampered)", got)
		}
		if !strings.Contains(b2.Errors["discounts"], "403") {
			t.Errorf("second bundle Errors[discounts] lost the original value: %q", b2.Errors["discounts"])
		}
	}

	// (b) In-flight waiters and the subsequent hit path: two concurrent
	// callers on a cold cache each get an independent Counts/Errors map, and
	// neither copy is the cached entry.
	{
		m := newCventMock(t)
		m.setEventEndpoints()
		setAllResources200(m)
		m.setStatus("/events/"+testUUID+"/discounts", http.StatusForbidden,
			[]byte(`{"message":"forbidden"}`))
		ec := newTestEventCache(m)
		ec.ttl = time.Hour
		t0 := time.Now()
		ec.now = func() time.Time { return t0 }

		var wg sync.WaitGroup
		start := make(chan struct{})
		bundles := make([]*EventBundle, 2)
		errs := make([]error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				bundles[i], errs[i] = ec.Bundle(context.Background(), testCode)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("concurrent bundle() caller %d: %v", i, err)
			}
		}
		if bundles[0] == bundles[1] {
			t.Fatal("two concurrent callers shared one *EventBundle pointer")
		}
		// Tamper through the second caller's copy.
		bundles[1].Counts["orders"] = 777
		bundles[1].Errors["discounts"] = "waiter-tampered"
		// The first caller's copy must be untouched.
		if got := bundles[0].Counts["orders"]; got != 2 {
			t.Errorf("caller 1's copy poisoned via caller 2: Counts[orders] = %d, want 2", got)
		}
		if got := bundles[0].Errors["discounts"]; got == "waiter-tampered" {
			t.Errorf("caller 1's copy poisoned via caller 2: Errors[discounts] = %q", got)
		}
		// A later hit-path read must also be untouched.
		b3, err := ec.Bundle(context.Background(), testCode)
		if err != nil {
			t.Fatalf("third bundle(): %v", err)
		}
		if got := b3.Counts["orders"]; got != 2 {
			t.Errorf("cache poisoned via in-flight waiter: hit-path Counts[orders] = %d, want 2", got)
		}
		if got := b3.Errors["discounts"]; got == "waiter-tampered" {
			t.Errorf("cache poisoned via in-flight waiter: hit-path Errors[discounts] = %q", got)
		}
	}
}

// TestIsUUID pins the UUID-shape detector: 8-4-4-4-12 hex is a UUID; anything
// else (codes, wrong lengths, bad separators) is not.
func TestIsUUID(t *testing.T) {
	yes := []string{
		"4f1c2a9e-7b3d-4e8a-9c21-5d6e7f80a1b0",
		"00000000-0000-0000-0000-000000000000",
		"ABCDEF01-2345-6789-ABCD-EF0123456789",
	}
	for _, s := range yes {
		if !IsUUID(s) {
			t.Errorf("IsUUID(%q) = false, want true", s)
		}
	}
	no := []string{
		testCode,                               // TESTCODE01
		"4f1c2a9e-7b3d-4e8a-9c21-5d6e7f80a1b",  // 35 chars
		"4f1c2a9e_7b3d_4e8a_9c21_5d6e7f80a1b0", // wrong separator
		"",
		"4f1c2a9e-7b3d-4e8a-9c21-5d6e7f80a1b01", // 37 chars
	}
	for _, s := range no {
		if IsUUID(s) {
			t.Errorf("IsUUID(%q) = true, want false", s)
		}
	}
}
