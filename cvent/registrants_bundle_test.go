package cvent

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The bundle reads each successful charge's detail for its billing address
// after the fan-out; a failing detail is recorded, not fatal.
func TestFetchEventBillingAddresses(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	setAllResources200(m)
	m.setSingle("/events/"+testUUID+"/transactions", itemsPage(
		`{"id":"t1","attendee":{"id":"p1"},"success":true,"paymentType":"Online Charge","date":"2026-08-01T00:00:00Z"}`))
	m.setSingle("/transactions/t1", []byte(`{"id":"t1","billingAddress":{"address1":"2 Bank Rd","city":"Lansing"}}`))

	ec := newTestEventCache(m)
	b, err := ec.Bundle(t.Context(), testCode)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if a := b.BillingAddresses["p1"]; a == nil || a.Address1 != "2 Bank Rd" {
		t.Fatalf("BillingAddresses = %+v", b.BillingAddresses)
	}
	// Callers get their own copy: a mutation must not reach the cache.
	b.BillingAddresses["p1"].Address1 = "tampered"
	b2, _ := ec.Bundle(t.Context(), testCode)
	if b2.BillingAddresses["p1"].Address1 != "2 Bank Rd" {
		t.Fatalf("cache poisoned: %+v", b2.BillingAddresses["p1"])
	}

	m.setStatus("/transactions/t1", http.StatusForbidden, []byte(`{}`))
	ec.Invalidate(testCode)
	b3, err := ec.Bundle(t.Context(), testCode)
	if err != nil {
		t.Fatalf("Bundle with a failing detail: %v", err)
	}
	if !strings.Contains(b3.Errors["billingAddresses"], "403") || b3.BillingAddresses != nil || b3.Transactions == nil {
		t.Fatalf("errors = %v, addresses = %v", b3.Errors, b3.BillingAddresses)
	}
}

func TestWriteDumpRegistrants(t *testing.T) {
	dir := t.TempDir()
	b := &EventBundle{
		Code:             testCode,
		PulledAt:         "2026-10-15T00:00:00Z",
		Event:            json.RawMessage(`{"title":"T","currency":"USD"}`),
		Counts:           map[string]int{},
		Attendees:        json.RawMessage(`[` + regAttFull + `,` + regAttGuest + `]`),
		BillingAddresses: map[string]*Address{"p1": {Address1: "2 Bank Rd"}},
	}
	if err := WriteDump(dir, b); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, testCode, "registrants.json"))
	if err != nil {
		t.Fatalf("registrants.json: %v", err)
	}
	var rs []Registrant
	if err := json.Unmarshal(raw, &rs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rs) != 2 || rs[0].BillingAddress == nil || len(rs[0].Guests) != 1 || rs[1].RegisteredBy == nil {
		t.Fatalf("registrants.json = %s", raw)
	}

	// No attendees: an empty array, not null.
	b.Code, b.Attendees = "EMPTY", nil
	if err := WriteDump(dir, b); err != nil {
		t.Fatalf("WriteDump: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "EMPTY", "registrants.json")); strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("empty registrants.json = %q", raw)
	}
}
