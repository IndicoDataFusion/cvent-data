package cvent

import (
	"context"
	"encoding/json"
	"testing"
)

// itemsPage wraps raw JSON items in a one-page list response.
func itemsPage(items ...string) []byte {
	b := `{"items":[`
	for i, it := range items {
		if i > 0 {
			b += ","
		}
		b += it
	}
	return []byte(b + `],"paging":{"limit":200,"totalCount":` + itoa(len(items)) + `}}`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

const (
	regAttRef = `{"id":"a1","status":"Accepted","confirmationNumber":"C1","checkedIn":true,
		"contact":{"firstName":" Ada ","lastName":"Lovelace","email":"ada@example.com","company":"AE"},
		"registrationType":{"name":"Member"},
		"answers":[{"question":{"id":"q-other"},"value":["x"]},{"question":{"id":"q-ref"},"value":[" 16 "]}]}`
	regAttNoRef = `{"id":"a2","status":"Cancelled","contact":{"firstName":"Grace","middleName":"B.","lastName":"Hopper"}}`
	regQRef     = `{"id":"q-ref","text":"Please enter your Indico registration reference number (Ref #). If you …"}`
	regQOther   = `{"id":"q-other","text":"First time attending"}`
)

func TestRefQuestionID(t *testing.T) {
	qs := []json.RawMessage{json.RawMessage(regQOther), json.RawMessage(regQRef)}
	if got := RefQuestionID(qs); got != "q-ref" {
		t.Fatalf("RefQuestionID = %q, want q-ref", got)
	}
	if got := RefQuestionID(qs[:1]); got != "" {
		t.Fatalf("RefQuestionID without a ref question = %q, want empty", got)
	}
}

func TestRegistrantsFrom(t *testing.T) {
	rs := RegistrantsFrom([]json.RawMessage{json.RawMessage(regAttRef), json.RawMessage(regAttNoRef)}, "q-ref")
	if len(rs) != 2 {
		t.Fatalf("got %d registrants, want 2", len(rs))
	}
	want := Registrant{ID: "a1", FirstName: "Ada", LastName: "Lovelace", FullName: "Ada Lovelace",
		Email: "ada@example.com", Company: "AE", Ref: "16", Status: "Accepted",
		RegistrationType: "Member", ConfirmationNumber: "C1", CheckedIn: true}
	if rs[0] != want {
		t.Fatalf("registrant[0] = %+v\nwant %+v", rs[0], want)
	}
	if rs[1].FullName != "Grace B. Hopper" || rs[1].Ref != "" || rs[1].Status != "Cancelled" {
		t.Fatalf("registrant[1] = %+v", rs[1])
	}
	// No ref question: Ref stays empty.
	if rs := RegistrantsFrom([]json.RawMessage{json.RawMessage(regAttRef)}, ""); rs[0].Ref != "" {
		t.Fatalf("Ref without a ref question = %q, want empty", rs[0].Ref)
	}
}

func TestFetchRegistrants(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	m.setSingle("/attendees/filter", itemsPage(regAttRef, regAttNoRef))
	m.setSingle("/event-questions", itemsPage(regQOther, regQRef))
	c := newCventClient(m.srv.URL, "cid", "secret")
	warmToken(c)

	rs, err := FetchRegistrants(context.Background(), c, testCode, RegistrantsOptions{})
	if err != nil {
		t.Fatalf("FetchRegistrants: %v", err)
	}
	if len(rs) != 2 || rs[0].Ref != "16" {
		t.Fatalf("got %+v, want 2 registrants with a1.Ref=16", rs)
	}
	// Only attendees + questions: no bundle fan-out.
	if n := m.count("/events/" + testUUID + "/orders"); n != 0 {
		t.Fatalf("orders fetched %d times, want 0", n)
	}

	// A pinned question id skips the questions fetch.
	before := m.count("/event-questions")
	rs, err = FetchRegistrants(context.Background(), c, testCode, RegistrantsOptions{RefQuestionID: "q-other"})
	if err != nil {
		t.Fatalf("FetchRegistrants (pinned): %v", err)
	}
	if rs[0].Ref != "x" || m.count("/event-questions") != before {
		t.Fatalf("pinned: Ref=%q, questions fetched again=%v", rs[0].Ref, m.count("/event-questions") != before)
	}
}
