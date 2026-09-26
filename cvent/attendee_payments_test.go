package cvent

import (
	"encoding/json"
	"slices"
	"testing"
)

func raws(items ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(items))
	for i, s := range items {
		out[i] = json.RawMessage(s)
	}
	return out
}

func TestPaymentsByAttendee(t *testing.T) {
	orders := raws(
		`{"attendee":{"id":"paid"},"amountOrdered":650,"amountPaid":650,"amountDue":0,"paymentMethod":"CreditCard","invoiceNumber":"INV-1"}`,
		`{"attendee":{"id":"partial"},"amountOrdered":1000,"amountPaid":400,"amountDue":600}`,
		`{"attendee":{"id":"unpaid"},"amountOrdered":300,"amountPaid":0,"amountDue":300}`,
		`{"attendee":{"id":"waived"},"amountOrdered":0,"amountPaid":0,"amountDue":0,"discounts":[{"code":"TEST","name":"Test Code","type":"Percentage","value":100,"amount":1350}]}`,
		`{"attendee":{"id":"cancelled"},"amountOrdered":500,"amountPaid":0,"amountDue":500,"cancelled":true}`,
		// two orders for one attendee, one cancelled: only the active one counts
		`{"attendee":{"id":"two"},"amountOrdered":200,"amountPaid":200,"amountDue":0}`,
		`{"attendee":{"id":"two"},"amountOrdered":999,"amountPaid":0,"amountDue":999,"cancelled":true}`,
		`{"amountOrdered":1}`, // no attendee id: skipped
	)
	txns := raws(
		`{"attendee":{"id":"paid"},"success":true,"paymentType":"Online Charge","amount":650,"currency":"USD","paymentMethod":"Visa","date":"2026-08-01T00:00:00Z"}`,
		`{"attendee":{"id":"paid"},"success":true,"paymentType":"Online Charge","amount":1,"paymentMethod":"Old","date":"2026-07-01T00:00:00Z"}`,
		`{"attendee":{"id":"paid"},"success":true,"paymentType":"Online Refund","amount":50}`,
		`{"attendee":{"id":"paid"},"success":false,"paymentType":"Online Refund","amount":999}`,
		`{"attendee":{"id":"nobody"},"success":true,"paymentType":"Online Charge","amount":5}`,
	)
	got := PaymentsByAttendee(orders, txns)

	for id, want := range map[string]string{
		"paid": PaymentPaid, "partial": PaymentPartial, "unpaid": PaymentUnpaid,
		"waived": PaymentWaived, "cancelled": PaymentCancelled, "two": PaymentPaid,
	} {
		if p := got[id]; p == nil || p.Status != want {
			t.Errorf("%s: status = %+v, want %s", id, p, want)
		}
	}
	if len(got) != 6 {
		t.Errorf("got %d attendees, want 6 (no-id order and order-less txn skipped)", len(got))
	}
	p := got["paid"]
	if p.Refunded != 50 || p.Currency != "USD" || p.Method != "Visa" || p.LastPaymentDate != "2026-08-01T00:00:00Z" ||
		!slices.Equal(p.Invoices, []string{"INV-1"}) {
		t.Errorf("paid = %+v", p)
	}
	if two := got["two"]; two.Ordered != 200 || two.Due != 0 {
		t.Errorf("two = %+v, want only the active order", two)
	}
	w := got["waived"]
	wantD := []AppliedDiscount{{Code: "TEST", Name: "Test Code", Type: "Percentage", Value: 100, Amount: 1350}}
	if !slices.Equal(w.Discounts, wantD) || w.OriginalAmount != 1350 {
		t.Errorf("waived discounts = %+v original = %v, want %+v / 1350", w.Discounts, w.OriginalAmount, wantD)
	}
	if got["paid"].OriginalAmount != 0 {
		t.Errorf("paid (no discount) OriginalAmount = %v, want 0", got["paid"].OriginalAmount)
	}
}

func TestFetchRegistrantsWithPayments(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	m.setSingle("/attendees/filter", itemsPage(regAttRef, regAttNoRef))
	m.setSingle("/event-questions", itemsPage(regQRef))
	m.setSingle("/events/"+testUUID+"/orders", itemsPage(`{"attendee":{"id":"a1"},"amountOrdered":10,"amountPaid":10,"amountDue":0}`))
	m.setSingle("/events/"+testUUID+"/transactions", itemsPage())
	c := newCventClient(m.srv.URL, "cid", "secret")
	warmToken(c)

	rs, err := FetchRegistrants(t.Context(), c, testCode, RegistrantsOptions{WithPayments: true})
	if err != nil {
		t.Fatalf("FetchRegistrants: %v", err)
	}
	if rs[0].Payment == nil || rs[0].Payment.Status != PaymentPaid || rs[1].Payment != nil {
		t.Fatalf("payments = %+v / %+v, want a1 paid, a2 none", rs[0].Payment, rs[1].Payment)
	}
	if rs[0].Payment.Currency != "USD" { // no transaction: the event's currency
		t.Fatalf("currency = %q, want the event's USD", rs[0].Payment.Currency)
	}
}
