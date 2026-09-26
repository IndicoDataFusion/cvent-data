package cvent

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
// layout order. name is the bundle key (and the Errors-map key); file is the
// on-disk name under <dir>/<code>/ — the same names tests/make_fixture.py
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
	{"eventQuestions", "event-questions.json"},
}

// ResourceFiles returns the dump layout: the bundle-key → on-disk filename
// pairs, in write order. Exposed so tooling can reproduce the layout.
func ResourceFiles() []struct {
	Name string
	File string
} {
	out := make([]struct{ Name, File string }, len(dumpResources))
	for i, r := range dumpResources {
		out[i] = struct{ Name, File string }{r.name, r.file}
	}
	return out
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
	case "eventQuestions":
		return b.EventQuestions
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

// DumpMeta is <dir>/<code>/meta.json. Errors is omitted when the bundle had
// no per-resource failures.
type DumpMeta struct {
	Code     string            `json:"code"`
	PulledAt string            `json:"pulledAt"`
	Counts   map[string]int    `json:"counts"`
	Errors   map[string]string `json:"errors,omitempty"`
}

// DumpIndexEntry is one element of <dir>/index.json — the static-mode
// discovery file (?static=1): the SPA reads it to find the events without a
// live API. One entry per dumped event; WriteDump upserts by code. ShortName
// is an optional compact label for the topbar event selector (e.g. "CONF27");
// the frontend falls back to Title when absent. A re-dump preserves a
// previously stored ShortName (it is not derivable from the Cvent event
// object).
type DumpIndexEntry struct {
	Code      string `json:"code"`
	Title     string `json:"title"`
	ShortName string `json:"shortName,omitempty"`
	Start     string `json:"start"`
	End       string `json:"end"`
	PulledAt  string `json:"pulledAt"`
}

// RunDump fetches the event's bundle once and writes the snapshot to dir. It
// never discovers or fans out across multiple events. It returns a short
// human summary line (for a CLI) and an error on any failure — including
// missing credentials.
func RunDump(creds Credentials, eventID, dir string) (string, error) {
	client := NewWithCredentials(creds)
	cache := NewEventCache(client)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := cache.Bundle(ctx, eventID)
	if err != nil {
		return "", err // Resolution failed: nothing is written.
	}
	if err := WriteDump(dir, b); err != nil {
		return "", err
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
	return fmt.Sprintf("dump: %s %q — %s → %s", b.Code, title, joinCountSummary(counts), filepath.Join(dir, b.Code)), nil
}

// WriteDump writes the snapshot: <dir>/<code>/{event,meta}.json plus one file
// per resource, and <dir>/index.json. Parent dirs are created and existing
// files are overwritten, so a re-dump is a plain replacement.
func WriteDump(dir string, b *EventBundle) error {
	codeDir := filepath.Join(dir, b.Code)
	if err := os.MkdirAll(codeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %v", codeDir, err)
	}
	write := func(name string, data []byte) error {
		p := filepath.Join(codeDir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %v", p, err)
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

	meta := DumpMeta{Code: b.Code, PulledAt: b.PulledAt, Counts: b.Counts}
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

	// <dir>/index.json — static-mode discovery (one entry per dumped event).
	// MERGED with any existing index so a re-dump of one event keeps the other
	// events the site shows; the current event's entry is upserted (matched by
	// code) and the list is sorted by start date.
	var ev struct {
		Title string `json:"title"`
		Start string `json:"start"`
		End   string `json:"end"`
	}
	_ = json.Unmarshal(b.Event, &ev) // lenient: missing fields → ""
	entries := []DumpIndexEntry{}
	if old, rerr := os.ReadFile(filepath.Join(dir, "index.json")); rerr == nil {
		_ = json.Unmarshal(old, &entries) // unparseable → start fresh
	}
	if entries == nil {
		entries = []DumpIndexEntry{}
	}
	entry := DumpIndexEntry{
		Code:     b.Code,
		Title:    ev.Title,
		Start:    ev.Start,
		End:      ev.End,
		PulledAt: b.PulledAt,
	}
	replaced := false
	for i, e := range entries {
		if e.Code == b.Code {
			if entry.ShortName == "" {
				entry.ShortName = e.ShortName // preserve the stored label
			}
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
