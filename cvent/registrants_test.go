package cvent

import (
	"context"
	"encoding/json"
	"reflect"
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
	// A full registration form: contact details, custom fields (repeated
	// under attendee.questions, as Cvent does) and one guest.
	regAttFull = `{"id":"p1","status":"Accepted","confirmationNumber":"CP1","registrationType":{"name":"IEEE NPSS Member"},
		"admissionItem":{"name":"Conference Registration"},"registrationPath":{"name":"Member"},
		"registeredAt":"2026-10-15T12:00:00Z","note":" VIP ",
		"contact":{"prefix":"Dr.","firstName":"Ada","lastName":"Lovelace","email":"ada@example.com","ccEmail":"cc@example.com","company":"AE",
			"title":" Engineer ","mobilePhone":"+1 555 0100","workPhone":"+1 555 0101","pronouns":"she/her",
			"type":{"name":"IEEE Member"},"primaryAddressType":"Work",
			"workAddress":{"address1":"1 Main St","city":"Lansing","region":"Michigan","postalCode":"48824","country":"United States","countryCode":"US"},
			"homeAddress":{"address1":"2 Home Rd","city":"Okemos"},
			"customFields":[
				{"name":"Emergency Contact","value":["Charles Babbage"]},
				{"name":"Emergency Contact #","value":["+1 555 0199"]},
				{"name":"Do you require a visa letter?","value":["Yes"]}]},
		"questions":[{"name":"Emergency Contact","value":["Charles Babbage"]}],
		"answers":[{"question":{"id":"q-other"},"value":["Yes"]},{"question":{"id":"q-diet"},"value":["Vegan","Nut allergy"]},{"question":{"id":"q-blank"},"value":[]}]}`
	regAttGuest = `{"id":"g1","status":"Accepted","guest":true,"primaryId":"p1","confirmationNumber":"CG1","registrationType":{"name":"Companion/Guest"},
		"contact":{"firstName":"Byron","lastName":"Lovelace"}}`
	regQRef   = `{"id":"q-ref","text":"Please enter your Indico registration reference number (Ref #). If you …"}`
	regQOther = `{"id":"q-other","text":"First time attending"}`
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
		RegistrationType: "Member", ConfirmationNumber: "C1", CheckedIn: true,
		Answers: []Answer{{QuestionID: "q-other", Value: "x"}, {QuestionID: "q-ref", Value: "16"}}}
	if !reflect.DeepEqual(rs[0], want) {
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

func TestRegistrantsFromForm(t *testing.T) {
	rs := RegistrantsFrom(raws(regAttFull, regAttGuest, regAttNoRef,
		`{"id":"in","checkIn":"2027-05-23T09:00:00Z"}`), "")
	p, g := rs[0], rs[1]
	if in := rs[3]; !in.CheckedIn || in.CheckedInAt != "2027-05-23T09:00:00Z" || p.CheckedIn {
		t.Errorf("check-in: %+v / %v", in, p.CheckedIn)
	}
	if p.Title != "Engineer" || p.MobilePhone != "+1 555 0100" || p.RegistrationType != "IEEE NPSS Member" ||
		p.Prefix != "Dr." || p.CCEmail != "cc@example.com" || p.WorkPhone != "+1 555 0101" || p.Pronouns != "she/her" ||
		p.ContactType != "IEEE Member" || p.AdmissionItem != "Conference Registration" || p.RegistrationPath != "Member" ||
		p.RegisteredAt != "2026-10-15T12:00:00Z" || p.Note != "VIP" {
		t.Errorf("contact = %+v", p)
	}
	if p.EmergencyContactName != "Charles Babbage" || p.EmergencyContactPhone != "+1 555 0199" || p.VisaLetter != "Yes" {
		t.Errorf("custom fields: emergency=%q/%q visa=%q", p.EmergencyContactName, p.EmergencyContactPhone, p.VisaLetter)
	}
	if len(p.CustomFields) != 3 {
		t.Errorf("CustomFields = %v, want 3 (questions duplicate dropped)", p.CustomFields)
	}
	if got := p.Address.String(); got != "1 Main St, Lansing, Michigan, 48824, United States" || p.AddressType != "Work" {
		t.Errorf("Address = %q (%s)", got, p.AddressType)
	}
	if got := p.OtherAddress.String(); got != "2 Home Rd, Okemos" {
		t.Errorf("OtherAddress = %q", got)
	}
	if p.InvoiceAddress() != p.Address {
		t.Errorf("InvoiceAddress without billing = %v, want the contact address", p.InvoiceAddress())
	}
	p.BillingAddress = &Address{Address1: "2 Bank Rd", City: "Lansing"}
	if p.InvoiceAddress() != p.BillingAddress {
		t.Errorf("InvoiceAddress with billing = %v, want the billing address", p.InvoiceAddress())
	}
	wantAns := []Answer{{QuestionID: "q-other", Value: "Yes"}, {QuestionID: "q-diet", Value: "Vegan; Nut allergy"}}
	if !reflect.DeepEqual(p.Answers, wantAns) {
		t.Errorf("Answers = %+v, want %+v (blank answer dropped)", p.Answers, wantAns)
	}
	if !g.Guest || g.PrimaryID != "p1" || g.Address != nil || g.CustomFields != nil {
		t.Errorf("guest = %+v", g)
	}
	if want := []PersonRef{{ID: "g1", FullName: "Byron Lovelace", ConfirmationNumber: "CG1"}}; !reflect.DeepEqual(p.Guests, want) {
		t.Errorf("Guests = %+v, want %+v", p.Guests, want)
	}
	if want := (&PersonRef{ID: "p1", FullName: "Ada Lovelace", Email: "ada@example.com", ConfirmationNumber: "CP1"}); !reflect.DeepEqual(g.RegisteredBy, want) {
		t.Errorf("RegisteredBy = %+v, want %+v", g.RegisteredBy, want)
	}
	if rs[2].Guests != nil || rs[2].Address != nil || len(rs) != 4 {
		t.Errorf("registrant without guests/address = %+v", rs[2])
	}

	LabelAnswers(rs, raws(regQOther))
	if p.Answers[0].Question != "First time attending" || p.Answers[1].Question != "" {
		t.Errorf("labelled answers = %+v", p.Answers)
	}
}

func TestRegistrantNamePlacements(t *testing.T) {
	rs := RegistrantsFrom(raws(
		`{"id":"n","name":{"givenName":"Alan","middle":"M","familyName":"Turing"},"email":"alan@example.com"}`,
		`{"id":"t","firstName":"Bob","lastName":"Smith"}`,
		`{"id":"c","firstName":"Top","contact":{"firstName":"Contact","lastName":"Wins"}}`,
	), "")
	for i, want := range []string{"Alan M Turing", "Bob Smith", "Contact Wins"} {
		if rs[i].FullName != want {
			t.Errorf("%s: FullName = %q, want %q", rs[i].ID, rs[i].FullName, want)
		}
	}
	if rs[0].Email != "alan@example.com" {
		t.Errorf("top-level email = %q", rs[0].Email)
	}
}

func TestAddressOnlyHome(t *testing.T) {
	// Work is the primary type, but only a home address was entered.
	rs := RegistrantsFrom(raws(`{"id":"h","contact":{"primaryAddressType":"Work","workAddress":{"countryCode":"US"},"homeAddress":{"address1":"2 Home Rd"}}}`), "")
	if rs[0].AddressType != "Home" || rs[0].Address.Address1 != "2 Home Rd" || rs[0].OtherAddress != nil {
		t.Fatalf("got %+v (%s), other %+v", rs[0].Address, rs[0].AddressType, rs[0].OtherAddress)
	}
}

func TestRegistrantsFromBundle(t *testing.T) {
	b := &EventBundle{
		Event:          json.RawMessage(`{"currency":"USD"}`),
		Attendees:      json.RawMessage(`[` + regAttFull + `,` + regAttGuest + `]`),
		EventQuestions: json.RawMessage(`[` + regQOther + `]`),
		Orders:         json.RawMessage(`[{"id":"o1","attendee":{"id":"p1"},"amountOrdered":1350,"amountPaid":1350,"amountDue":0}]`),
		OrderItems: json.RawMessage(`[{"order":{"id":"o1"},"attendee":{"id":"p1"},"name":"Conference Registration","amountOrdered":990,"amountPaid":990},
			{"order":{"id":"o1"},"attendee":{"id":"p1"},"name":"Companion/Guest Registration","guest":true,"amountOrdered":360,"amountPaid":360}]`),
		Transactions:     json.RawMessage(`not json`), // unparseable resource → treated as empty
		BillingAddresses: map[string]*Address{"p1": {Address1: "2 Bank Rd"}},
	}
	rs := RegistrantsFromBundle(b, "")
	p, g := rs[0], rs[1]
	if p.Payment == nil || p.Payment.Status != PaymentPaid || p.Payment.Currency != "USD" || len(p.Payment.Items) != 2 ||
		!p.Payment.Items[1].ForGuest {
		t.Fatalf("payment = %+v", p.Payment)
	}
	if p.BillingAddress == nil || p.InvoiceAddress().Address1 != "2 Bank Rd" {
		t.Fatalf("billing = %+v", p.BillingAddress)
	}
	if p.Answers[0].Question != "First time attending" {
		t.Fatalf("answers not labelled: %+v", p.Answers)
	}
	if g.Payment != nil || g.RegisteredBy == nil || g.RegisteredBy.ID != "p1" {
		t.Fatalf("guest = %+v", g)
	}
	if RegistrantsFromBundle(nil, "") != nil {
		t.Fatal("nil bundle should give nil")
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

	if rs[0].Answers[1].Question == "" {
		t.Fatalf("answers not labelled: %+v", rs[0].Answers)
	}

	// A pinned question id overrides the auto-detected one.
	rs, err = FetchRegistrants(context.Background(), c, testCode, RegistrantsOptions{RefQuestionID: "q-other"})
	if err != nil {
		t.Fatalf("FetchRegistrants (pinned): %v", err)
	}
	if rs[0].Ref != "x" {
		t.Fatalf("pinned: Ref=%q, want x", rs[0].Ref)
	}
}
