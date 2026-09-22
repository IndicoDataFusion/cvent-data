package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// EventBundle is the per-event payload served to the PWA. Each resource field
// is the full collected item array as raw JSON; empty/failed resources are
// nil (omitted in JSON, never []) so the UI can distinguish "no rows" from
// "fetch failed" via the Errors map.
type EventBundle struct {
	Code     string `json:"code"`
	PulledAt string `json:"pulledAt"` // RFC3339

	// Stale is set by the cache layer at read time when the bundle is older
	// than 24h. It is NOT a trigger for a re-fetch; the UI shows a badge and a
	// Re-pull button (which calls invalidate + refetch).
	Stale bool `json:"stale,omitempty"`

	Event json.RawMessage `json:"event"`

	// Counts always carries every resource key (0 on failure/empty) so the
	// UI badge logic is uniform.
	Counts map[string]int `json:"counts"`

	RegistrationTypes json.RawMessage `json:"registrationTypes,omitempty"`
	AdmissionItems    json.RawMessage `json:"admissionItems,omitempty"`
	FeeItems          json.RawMessage `json:"feeItems,omitempty"`
	Discounts         json.RawMessage `json:"discounts,omitempty"`
	Sessions          json.RawMessage `json:"sessions,omitempty"`
	Speakers          json.RawMessage `json:"speakers,omitempty"`
	Attendees         json.RawMessage `json:"attendees,omitempty"`
	Activities        json.RawMessage `json:"activities,omitempty"`
	Orders            json.RawMessage `json:"orders,omitempty"`
	OrderItems        json.RawMessage `json:"orderItems,omitempty"`
	Transactions      json.RawMessage `json:"transactions,omitempty"`
	TransactionItems  json.RawMessage `json:"transactionItems,omitempty"`

	// Errors maps resource name → error string for resources that failed to
	// fetch. Never fatal: the bundle is returned as long as the event
	// itself resolved.
	Errors map[string]string `json:"errors,omitempty"`
}

// eventCache is the per-event fetch cache. It follows the house-style pattern
// (map + mutex, in-flight dedup with a WaitGroup, evict-oldest cap,
// injectable clock): a second caller arriving while a fetch is in progress
// waits and reuses the result instead of fanning out a second set of
// upstream requests.
type eventCache struct {
	c *cventClient

	mu       sync.Mutex
	entries  map[string]*bundleEntry // code → cached bundle
	neg      map[string]negEntry     // code → last failed fetch (60s negative cache)
	inflight map[string]*eventInflight
	ttl      time.Duration // fresh window for a cached bundle (default 15m; tests may shrink)
	now      func() time.Time
}

const (
	eventCacheTTL = 15 * time.Minute // default bundle freshness
	eventCacheCap = 200              // max distinct events held
	eventNegTTL   = 60 * time.Second // negative-cache window after a failed fetch
	eventStaleTTL = 24 * time.Hour   // read-time staleness badge threshold
	fanoutTimeout = 20 * time.Second // overall timeout for the resource fan-out
)

type bundleEntry struct {
	bundle *EventBundle
	at     time.Time
}

type negEntry struct {
	at  time.Time
	err error
}

// eventInflight lets callers racing on the same code wait for the one
// in-progress fetch and reuse its result.
type eventInflight struct {
	wg     sync.WaitGroup
	bundle *EventBundle
	err    error
}

func newEventCache(c *cventClient) *eventCache {
	return &eventCache{
		c:        c,
		entries:  map[string]*bundleEntry{},
		neg:      map[string]negEntry{},
		inflight: map[string]*eventInflight{},
		ttl:      eventCacheTTL,
		now:      time.Now,
	}
}

// bundle returns the cached bundle for code when fresh (within TTL),
// otherwise fetches it. Concurrent callers on the same code are deduped onto
// the single in-flight fetch; a failed fetch is recorded in the negative
// cache (60s) so a broken event isn't hammered. Only event-resolution
// failure surfaces as an error; per-resource failures live in bundle.Errors.
func (ec *eventCache) bundle(ctx context.Context, code string) (*EventBundle, error) {
	ec.mu.Lock()
	now := ec.now()
	if e, ok := ec.entries[code]; ok && now.Sub(e.at) < ec.ttl {
		ec.mu.Unlock()
		return ec.stamped(e.bundle), nil
	}
	if ne, ok := ec.neg[code]; ok && now.Sub(ne.at) < eventNegTTL {
		ec.mu.Unlock()
		return nil, ne.err
	}
	if call, ok := ec.inflight[code]; ok {
		ec.mu.Unlock()
		call.wg.Wait()
		return call.bundle, call.err
	}
	call := &eventInflight{}
	call.wg.Add(1)
	ec.inflight[code] = call
	ec.mu.Unlock()

	// Detached from the caller: a client disconnecting mid-fetch must not
	// cancel the fetch that populates the shared cache for everyone else.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fanoutTimeout)
	defer cancel()

	b, err := ec.fetchEvent(fctx, code)

	ec.mu.Lock()
	if err == nil {
		ec.store(code, b)
		delete(ec.neg, code)
	} else {
		ec.neg[code] = negEntry{at: ec.now(), err: err}
	}
	delete(ec.inflight, code)
	ec.mu.Unlock()

	call.bundle, call.err = b, err
	call.wg.Done()
	return b, err
}

// invalidate drops the positive cache entry and the negative entry for code,
// so the next bundle() call re-fetches from upstream. (Task 6's checkin
// handler calls this after a successful check-in.)
func (ec *eventCache) invalidate(code string) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	delete(ec.entries, code)
	delete(ec.neg, code)
}

// store inserts a fresh entry, evicting the oldest first when the cap is
// reached. Callers must hold ec.mu.
func (ec *eventCache) store(code string, b *EventBundle) {
	if len(ec.entries) >= eventCacheCap {
		ec.evictOldest()
	}
	ec.entries[code] = &bundleEntry{bundle: b, at: ec.now()}
}

// evictOldest drops the entry with the oldest fetch time. Callers must hold
// ec.mu.
func (ec *eventCache) evictOldest() {
	var oldestKey string
	var oldestAt time.Time
	for k, e := range ec.entries {
		if oldestKey == "" || e.at.Before(oldestAt) {
			oldestKey, oldestAt = k, e.at
		}
	}
	delete(ec.entries, oldestKey)
}

// stamped returns a copy of b with Stale derived at read time: true when the
// bundle is older than 24h. Staleness never triggers a re-fetch here — the
// UI shows the badge and the user can Re-pull.
func (ec *eventCache) stamped(b *EventBundle) *EventBundle {
	cp := *b
	if t, err := time.Parse(time.RFC3339, b.PulledAt); err == nil {
		if ec.now().Sub(t) > eventStaleTTL {
			cp.Stale = true
		}
	}
	return &cp
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isUUID reports whether s looks like a UUID (8-4-4-4-12 hex).
func isUUID(s string) bool { return uuidRe.MatchString(s) }

// getOnePage fetches a single page (no pagination walk) and returns the
// event it contains: a list page ({"items":[...]} or {"data":[...]}) yields
// the first item; a single object with an "id" field (GET /events/{uuid}) is
// returned as is. Zero matching items → "event not found". Used for
// code→uuid resolution, where a walk is unsafe (the /events token quirk
// silently ignores the token param and repeats the page).
func (c *cventClient) getOnePage(ctx context.Context, path string, extra url.Values) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("limit", fmt.Sprint(pageSize))
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.base, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("cvent %s: %v", path, err)
	}
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cvent %s: %v", path, err)
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("cvent %s: reading body: %v", path, rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cvent %s: HTTP %d", path, resp.StatusCode)
	}

	var probe struct {
		Items *json.RawMessage `json:"items"`
		Data  *json.RawMessage `json:"data"`
		ID    *string          `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("cvent %s: decoding response: %v", path, err)
	}
	var list *json.RawMessage
	switch {
	case probe.Items != nil:
		list = probe.Items
	case probe.Data != nil:
		list = probe.Data
	}
	if list != nil {
		var items []json.RawMessage
		if err := json.Unmarshal(*list, &items); err != nil {
			return nil, fmt.Errorf("cvent %s: decoding items: %v", path, err)
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("cvent %s: event not found", path)
		}
		return items[0], nil
	}
	if probe.ID != nil {
		return raw, nil // single-object shape (GET /events/{uuid})
	}
	return nil, fmt.Errorf("cvent %s: unrecognized response shape (no items, no id)", path)
}

// fetchEvent resolves the event (code→uuid via a single-page GET, or uuid
// directly) and fans out the 12 bundle resources in parallel. Per-resource
// failures are recorded in Errors (resource field left nil); the bundle is
// returned as long as the event itself resolved.
func (ec *eventCache) fetchEvent(ctx context.Context, code string) (*EventBundle, error) {
	var eventRaw json.RawMessage
	var err error
	if isUUID(code) {
		eventRaw, err = ec.c.getOnePage(ctx, "/events/"+code, nil)
	} else {
		eventRaw, err = ec.c.getOnePage(ctx, "/events", url.Values{"filter": {"code eq '" + code + "'"}})
	}
	if err != nil {
		return nil, fmt.Errorf("event not found: %s: %v", code, err)
	}
	var ev struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(eventRaw, &ev); err != nil || ev.ID == "" {
		return nil, fmt.Errorf("event not found: %s: resolved event has no id", code)
	}
	uuid := ev.ID

	type resSpec struct {
		name  string
		fetch func(ctx context.Context) ([]json.RawMessage, error)
	}
	// Exactly the 12 bundle resources, one goroutine each.
	specs := []resSpec{
		{"registrationTypes", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/events/"+uuid+"/registration-types", nil)
		}},
		{"admissionItems", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.filterAll(ctx, "/admission-items/filter", "event.id eq '"+uuid+"'")
		}},
		{"feeItems", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/events/"+uuid+"/fee-items", nil)
		}},
		{"discounts", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/events/"+uuid+"/discounts", nil)
		}},
		{"sessions", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.filterAll(ctx, "/sessions/filter", "event.id eq '"+uuid+"'")
		}},
		{"speakers", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.filterAll(ctx, "/speakers/filter", "event.id eq '"+uuid+"'")
		}},
		{"attendees", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.filterAll(ctx, "/attendees/filter", "event.id eq '"+uuid+"'")
		}},
		// activities takes its filter as a QUERY PARAM, not a POST body
		// (verified live).
		{"activities", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/attendees/activities", url.Values{"filter": {"event.id eq '" + uuid + "'"}})
		}},
		{"orders", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/events/"+uuid+"/orders", nil)
		}},
		{"orderItems", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/events/"+uuid+"/orders/items", nil)
		}},
		{"transactions", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/events/"+uuid+"/transactions", nil)
		}},
		{"transactionItems", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/events/"+uuid+"/transactions/items", nil)
		}},
	}

	fctx, cancel := context.WithTimeout(ctx, fanoutTimeout)
	defer cancel()

	results := make([][]json.RawMessage, len(specs))
	errs := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec resSpec) {
			defer wg.Done()
			results[i], errs[i] = spec.fetch(fctx)
		}(i, spec)
	}
	wg.Wait()

	b := &EventBundle{
		Event:    eventRaw,
		Code:     ev.Code,
		PulledAt: ec.now().UTC().Format(time.RFC3339),
		Counts:   make(map[string]int, len(specs)),
	}
	if b.Code == "" {
		b.Code = code
	}
	set := map[string]func(json.RawMessage){
		"registrationTypes": func(r json.RawMessage) { b.RegistrationTypes = r },
		"admissionItems":    func(r json.RawMessage) { b.AdmissionItems = r },
		"feeItems":          func(r json.RawMessage) { b.FeeItems = r },
		"discounts":         func(r json.RawMessage) { b.Discounts = r },
		"sessions":          func(r json.RawMessage) { b.Sessions = r },
		"speakers":          func(r json.RawMessage) { b.Speakers = r },
		"attendees":         func(r json.RawMessage) { b.Attendees = r },
		"activities":        func(r json.RawMessage) { b.Activities = r },
		"orders":            func(r json.RawMessage) { b.Orders = r },
		"orderItems":        func(r json.RawMessage) { b.OrderItems = r },
		"transactions":      func(r json.RawMessage) { b.Transactions = r },
		"transactionItems":  func(r json.RawMessage) { b.TransactionItems = r },
	}
	for i, spec := range specs {
		if errs[i] != nil {
			if b.Errors == nil {
				b.Errors = map[string]string{}
			}
			b.Errors[spec.name] = errs[i].Error()
			b.Counts[spec.name] = 0
			continue
		}
		if raw := marshalItems(results[i]); raw != nil {
			set[spec.name](raw)
		}
		b.Counts[spec.name] = len(results[i])
	}
	return b, nil
}

// marshalItems marshals collected items into a JSON array raw message; empty
// input yields nil so the field is omitted (not []) in the bundle JSON.
func marshalItems(items []json.RawMessage) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return out
}
