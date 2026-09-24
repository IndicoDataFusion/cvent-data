package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// dumpResources is the exact set of per-resource files a dump writes, in
// layout order. name is the bundle key (and the Errors-map key); file is
// the on-disk name under <dir>/<code>/ — the same names tests/pull_event.sh
// produces, so bash and Go dumps stay interchangeable.
var dumpResources = []struct {
	name string
	file string
}{
	{"attendees", "attendees.json"},
	{"activities", "activities.json"},
	{"orders", "orders.json"},
	{"orderItems", "order-items.json"},
	{"transactions", "transactions.json"},
	{"transactionItems", "transaction-items.json"},
	{"feeItems", "fee-items.json"},
	{"admissionItems", "admission-items.json"},
	{"registrationTypes", "registration-types.json"},
	{"discounts", "discounts.json"},
	{"sessions", "sessions.json"},
	{"speakers", "speakers.json"},
}

// resourceField returns the bundle's raw JSON array for key.
func resourceField(b *EventBundle, key string) json.RawMessage {
	switch key {
	case "attendees":
		return b.Attendees
	case "activities":
		return b.Activities
	case "orders":
		return b.Orders
	case "orderItems":
		return b.OrderItems
	case "transactions":
		return b.Transactions
	case "transactionItems":
		return b.TransactionItems
	case "feeItems":
		return b.FeeItems
	case "admissionItems":
		return b.AdmissionItems
	case "registrationTypes":
		return b.RegistrationTypes
	case "discounts":
		return b.Discounts
	case "sessions":
		return b.Sessions
	case "speakers":
		return b.Speakers
	}
	return nil
}

// dumpMeta is <dir>/<code>/meta.json. Errors is omitted when the bundle
// had no per-resource failures.
type dumpMeta struct {
	Code     string         `json:"code"`
	PulledAt string         `json:"pulledAt"`
	Counts   map[string]int `json:"counts"`
	Errors   map[string]string `json:"errors,omitempty"`
}

// dumpIndexEntry is one element of <dir>/index.json — the static-mode
// discovery file (?static=1): the SPA reads it to find the events without a
// live API. One entry per dumped event; writeDump upserts by code.
type dumpIndexEntry struct {
	Code     string `json:"code"`
	Title    string `json:"title"`
	Start    string `json:"start"`
	End      string `json:"end"`
	PulledAt string `json:"pulledAt"`
}

// runDump is the one-shot --dump mode: fetch the configured event's bundle
// ONCE and write the snapshot to dir. It never discovers or fans out
// across multiple events, and it exits (the caller os.Exits with the
// returned code). Returns 0 on success, 1 on any failure — including
// missing credentials, for which there is no server-start fallback.
func runDump(eventID, dir string) int {
	cid, sec, base, err := loadCventEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dump: %v\n", err)
		return 1
	}
	client := newCventClient(base, cid, sec)
	cache := newEventCache(client)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := cache.bundle(ctx, eventID)
	if err != nil {
		// Resolution failed: nothing is written.
		fmt.Fprintf(os.Stderr, "dump: %v\n", err)
		return 1
	}
	if err := writeDump(dir, b); err != nil {
		fmt.Fprintf(os.Stderr, "dump: %v\n", err)
		return 1
	}

	var title string
	if len(b.Event) > 0 {
		var ev struct {
			Title string `json:"title"`
		}
		_ = json.Unmarshal(b.Event, &ev)
		title = ev.Title
	}
	var counts []string
	for _, r := range dumpResources {
		if n := b.Counts[r.name]; n > 0 {
			counts = append(counts, fmt.Sprintf("%s=%d", r.name, n))
		}
	}
	if len(counts) == 0 {
		counts = []string{"all zero"}
	}
	fmt.Printf("dump: %s %q — %s → %s\n", b.Code, title, joinCountSummary(counts), filepath.Join(dir, b.Code))
	return 0
}

// writeDump writes the snapshot: <dir>/<code>/{event,meta}.json plus one
// file per resource, and <dir>/index.json. Parent dirs are created and
// existing files are overwritten, so a re-dump is a plain replacement.
func writeDump(dir string, b *EventBundle) error {
	codeDir := filepath.Join(dir, b.Code)
	if err := os.MkdirAll(codeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %v", codeDir, err)
	}
	write := func(name string, data []byte) error {
		p := filepath.Join(codeDir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %v", name, err)
		}
		return nil
	}

	// The Event object, raw.
	if err := write("event.json", append(b.Event, '\n')); err != nil {
		return err
	}

	// One file per resource: the raw array, the literal null when the
	// resource was empty/failed-empty (so "no rows" stays distinguishable
	// on disk), or an error object when the fetch itself failed.
	for _, r := range dumpResources {
		var data []byte
		if msg, failed := b.Errors[r.name]; failed {
			data, _ = json.Marshal(map[string]string{"error": msg})
		} else if raw := resourceField(b, r.name); raw != nil {
			data = raw
		} else {
			data = []byte("null")
		}
		data = append(data, '\n')
		if err := write(r.file, data); err != nil {
			return err
		}
	}

	meta := dumpMeta{Code: b.Code, PulledAt: b.PulledAt, Counts: b.Counts}
	if len(b.Errors) > 0 {
		meta.Errors = b.Errors
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal meta.json: %v", err)
	}
	if err := write("meta.json", append(metaJSON, '\n')); err != nil {
		return err
	}

	// <dir>/index.json — static-mode discovery (one entry per dumped
	// event). MERGED with any existing index so a re-dump of one event
	// keeps the other events the site shows; the current event's entry is
	// upserted (matched by code) and the list is sorted by start date.
	var ev struct {
		Title string `json:"title"`
		Start string `json:"start"`
		End   string `json:"end"`
	}
	_ = json.Unmarshal(b.Event, &ev) // lenient: missing fields → ""
	entries := []dumpIndexEntry{}
	if old, rerr := os.ReadFile(filepath.Join(dir, "index.json")); rerr == nil {
		_ = json.Unmarshal(old, &entries) // unparseable → start fresh
	}
	if entries == nil {
		entries = []dumpIndexEntry{}
	}
	entry := dumpIndexEntry{
		Code:     b.Code,
		Title:    ev.Title,
		Start:    ev.Start,
		End:      ev.End,
		PulledAt: b.PulledAt,
	}
	replaced := false
	for i, e := range entries {
		if e.Code == b.Code {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Start != entries[j].Start {
			return entries[i].Start < entries[j].Start
		}
		return entries[i].Title < entries[j].Title
	})
	idxJSON, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal index.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), append(idxJSON, '\n'), 0o644); err != nil {
		return fmt.Errorf("write index.json: %v", err)
	}
	return nil
}

func joinCountSummary(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
