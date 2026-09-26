package cvent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
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
	// Re-pull button (which calls Invalidate + refetch).
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
	// EventQuestions resolves attendees[].answers[].question.id to the
	// question text/type (raw, including Cvent's SL_* system fields).
	EventQuestions json.RawMessage `json:"eventQuestions,omitempty"`

	// Errors maps resource name → error string for resources that failed to
	// fetch. Never fatal: the bundle is returned as long as the event
	// itself resolved.
	Errors map[string]string `json:"errors,omitempty"`
}

// EventCache is the per-event fetch cache. It follows the house-style pattern
// (map + mutex, in-flight dedup with a WaitGroup, evict-oldest cap,
// injectable clock): a second caller arriving while a fetch is in progress
// waits and reuses the result instead of fanning out a second set of
// upstream requests.
type EventCache struct {
	c *Client

	mu       sync.Mutex
	entries  map[string]*bundleEntry // code → cached bundle
	neg      map[string]negEntry     // code → last failed fetch (60s negative cache)
	inflight map[string]*eventInflight
	ttl      time.Duration // fresh window for a cached bundle (default 15m; tests may shrink)
	now      func() time.Time
}

const (
	EventCacheTTL = 15 * time.Minute // default bundle freshness
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

// NewEventCache builds an EventCache backed by client.
func NewEventCache(c *Client) *EventCache {
	return &EventCache{
		c:        c,
		entries:  map[string]*bundleEntry{},
		neg:      map[string]negEntry{},
		inflight: map[string]*eventInflight{},
		ttl:      EventCacheTTL,
		now:      time.Now,
	}
}

// Bundle returns the cached bundle for code when fresh (within TTL),
// otherwise fetches it. Concurrent callers on the same code are deduped onto
// the single in-flight fetch; a failed fetch is recorded in the negative
// cache (60s) so a broken event isn't hammered. Only event-resolution
// failure surfaces as an error; per-resource failures live in bundle.Errors.
func (ec *EventCache) Bundle(ctx context.Context, code string) (*EventBundle, error) {
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
		if call.err != nil {
			return nil, call.err
		}
		// Each waiter gets its own copy, not the fetcher's.
		return ec.stamped(call.bundle), nil
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

	// call.bundle keeps the canonical fetched bundle (the same pointer the
	// cache stores); every caller — fetcher and in-flight waiters alike —
	// receives its own copy via stamped() so no one holds the cached object.
	call.bundle, call.err = b, err
	call.wg.Done()
	if err != nil {
		return nil, err
	}
	return ec.stamped(b), nil
}

// Invalidate drops the positive cache entry and the negative entry for code,
// so the next Bundle call re-fetches from upstream. The check-in handler calls
// this after a successful check-in.
func (ec *EventCache) Invalidate(code string) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	delete(ec.entries, code)
	delete(ec.neg, code)
}

// PulledAt returns the cached bundle's pull time for code, if present. It lets
// the server report a repull's freshness without reaching into the cache
// internals. ok is false when there is no cached bundle for code.
func (ec *EventCache) PulledAt(code string) (string, bool) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	if e, ok := ec.entries[code]; ok && e.bundle != nil {
		return e.bundle.PulledAt, true
	}
	return "", false
}

// Put inserts a bundle into the cache, bypassing the fetch path (no negative-
// cache consult, no fan-out). Intended for tests and for seeding after an
// external write; the entry becomes immediately fresh.
func (ec *EventCache) Put(code string, b *EventBundle) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	ec.store(code, b)
}

// store inserts a fresh entry, evicting the oldest first when the cap is
// reached. Callers must hold ec.mu.
func (ec *EventCache) store(code string, b *EventBundle) {
	if len(ec.entries) >= eventCacheCap {
		ec.evictOldest()
	}
	ec.entries[code] = &bundleEntry{bundle: b, at: ec.now()}
}

// evictOldest drops the entry with the oldest fetch time. Callers must hold
// ec.mu.
func (ec *EventCache) evictOldest() {
	var oldestKey string
	var oldestAt time.Time
	for k, e := range ec.entries {
		if oldestKey == "" || e.at.Before(oldestAt) {
			oldestKey, oldestAt = k, e.at
		}
	}
	delete(ec.entries, oldestKey)
}

// stamped returns a copy of b safe for hand to a caller: Stale is derived at
// read time (true when the bundle is older than 24h), and the Counts/Errors
// maps are deep-copied so caller mutation can never poison the cached entry.
// Staleness never triggers a re-fetch here — the UI shows the badge and the
// user can Re-pull.
func (ec *EventCache) stamped(b *EventBundle) *EventBundle {
	cp := *b
	if t, err := time.Parse(time.RFC3339, b.PulledAt); err == nil {
		if ec.now().Sub(t) > eventStaleTTL {
			cp.Stale = true
		}
	}
	if b.Counts != nil {
		cp.Counts = make(map[string]int, len(b.Counts))
		for k, v := range b.Counts {
			cp.Counts[k] = v
		}
	}
	if b.Errors != nil {
		cp.Errors = make(map[string]string, len(b.Errors))
		for k, v := range b.Errors {
			cp.Errors[k] = v
		}
	}
	return &cp
}

// EventUUID returns the resolved event uuid for code, read from the cached
// bundle's Event field WITHOUT fetching (no fan-out, no negative cache
// consult). ok is false when there is no cached bundle or its Event field has
// no resolvable uuid. The check-in handler uses this to reach
// /events/{uuid}/check-in.
func (ec *EventCache) EventUUID(code string) (string, bool) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	e, ok := ec.entries[code]
	if !ok || e.bundle == nil || len(e.bundle.Event) == 0 {
		return "", false
	}
	var ev struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(e.bundle.Event, &ev); err != nil || !IsUUID(ev.ID) {
		return "", false
	}
	return ev.ID, true
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s looks like a UUID (8-4-4-4-12 hex).
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

// fetchEvent resolves the event (code→uuid via a single-page GET, or uuid
// directly) and fans out the 13 bundle resources in parallel. Per-resource
// failures are recorded in Errors (resource field left nil); the bundle is
// returned as long as the event itself resolved.
func (ec *EventCache) fetchEvent(ctx context.Context, code string) (*EventBundle, error) {
	var eventRaw json.RawMessage
	var err error
	if IsUUID(code) {
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
	// Exactly the 13 bundle resources, one goroutine each.
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
		{"eventQuestions", func(ctx context.Context) ([]json.RawMessage, error) {
			return ec.c.listAll(ctx, "/event-questions", url.Values{"filter": {"event.id eq '" + uuid + "'"}})
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
		"eventQuestions":    func(r json.RawMessage) { b.EventQuestions = r },
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
