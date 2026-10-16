package cvent

import (
	"encoding/json"
	"reflect"
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
	m.setSingle("/events/"+testUUID+"/orders", itemsPage(`{"id":"o1","attendee":{"id":"a1"},"amountOrdered":10,"amountPaid":10,"amountDue":0}`))
	m.setSingle("/events/"+testUUID+"/orders/items", itemsPage(`{"order":{"id":"o1"},"attendee":{"id":"a1"},"name":"Conference Registration","active":true,"amountOrdered":10,"amountPaid":10}`))
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
	if items := rs[0].Payment.Items; len(items) != 1 || items[0].Name != "Conference Registration" {
		t.Fatalf("items = %+v, want the conference registration", items)
	}
	if rs[0].BillingAddress != nil {
		t.Fatalf("BillingAddress without WithBillingAddress = %+v", rs[0].BillingAddress)
	}
}

func TestOrderItemsByAttendee(t *testing.T) {
	orders := raws(`{"id":"o1"}`, `{"id":"o2","cancelled":true}`)
	items := raws(
		`{"order":{"id":"o1"},"attendee":{"id":"p1"},"product":{"type":"AdmissionItem"},"name":"Conference Registration","active":true,"quantity":1,"amountOrdered":990,"amountPaid":990,"amountDue":0}`,
		`{"order":{"id":"o1"},"attendee":{"id":"p1"},"product":{"type":"AdmissionItem"},"name":"Companion/Guest Registration","guest":true,"active":true,"quantity":1,"amountOrdered":360.004,"amountPaid":0,"amountDue":360.004}`,
		`{"order":{"id":"o1"},"attendee":{"id":"p1"},"name":"Dropped","active":false,"amountOrdered":5}`,
		`{"order":{"id":"o1"},"attendee":{"id":"p1"},"name":"Deleted","deleted":true,"amountOrdered":5}`,
		`{"order":{"id":"o2"},"attendee":{"id":"p1"},"name":"Cancelled order","amountOrdered":5}`,
		`{"order":{"id":"o1"},"name":"No attendee"}`,
	)
	got := OrderItemsByAttendee(orders, items)
	want := map[string][]OrderItem{"p1": {
		{Name: "Conference Registration", ProductType: "AdmissionItem", Quantity: 1, Ordered: 990, Paid: 990},
		{Name: "Companion/Guest Registration", ProductType: "AdmissionItem", ForGuest: true, Quantity: 1, Ordered: 360, Due: 360},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("OrderItemsByAttendee = %+v\nwant %+v", got, want)
	}
}

func TestFetchRegistrantsWithBillingAddress(t *testing.T) {
	m := newCventMock(t)
	m.setEventEndpoints()
	m.setSingle("/attendees/filter", itemsPage(regAttFull, regAttRef))
	m.setSingle("/event-questions", itemsPage(regQRef))
	m.setSingle("/events/"+testUUID+"/transactions", itemsPage(
		`{"id":"t-old","attendee":{"id":"p1"},"success":true,"paymentType":"Online Charge","date":"2026-07-01T00:00:00Z"}`,
		`{"id":"t-new","attendee":{"id":"p1"},"success":true,"paymentType":"Online Charge","date":"2026-08-01T00:00:00Z"}`,
		`{"id":"t-bank","attendee":{"id":"p1"},"success":true,"paymentType":"Offline Charge","date":"2026-09-01T00:00:00Z"}`,
		`{"id":"t-refund","attendee":{"id":"p1"},"success":true,"paymentType":"Online Refund","date":"2026-10-01T00:00:00Z"}`,
		`{"id":"t-failed","attendee":{"id":"a1"},"success":false,"paymentType":"Online Charge"}`,
	))
	m.setSingle("/transactions/t-old", []byte(`{"id":"t-old","billingAddress":{"address1":"Old Rd","city":"X"}}`))
	m.setSingle("/transactions/t-new", []byte(`{"id":"t-new","billingAddress":{"address1":"2 Bank Rd","city":"Lansing","countryCode":"US"}}`))
	m.setSingle("/transactions/t-bank", []byte(`{"id":"t-bank","billingAddress":{}}`))
	c := newCventClient(m.srv.URL, "cid", "secret")
	warmToken(c)

	rs, err := FetchRegistrants(t.Context(), c, testCode, RegistrantsOptions{WithBillingAddress: true})
	if err != nil {
		t.Fatalf("FetchRegistrants: %v", err)
	}
	if b := rs[0].BillingAddress; b == nil || b.Address1 != "2 Bank Rd" {
		t.Fatalf("BillingAddress = %+v, want the latest addressed charge (2 Bank Rd)", b)
	}
	if rs[0].InvoiceAddress() != rs[0].BillingAddress || rs[1].BillingAddress != nil {
		t.Fatalf("invoice = %+v, a1 billing = %+v", rs[0].InvoiceAddress(), rs[1].BillingAddress)
	}
	if m.count("/transactions/t-refund") != 0 || m.count("/transactions/t-failed") != 0 {
		t.Fatal("refund/failed transaction details fetched")
	}
	if m.count("/events/"+testUUID+"/orders") != 0 {
		t.Fatal("orders fetched without WithPayments")
	}

	// A failing detail request fails the fetch.
	m.setStatus("/transactions/t-old", 403, []byte(`{}`))
	if _, err := FetchRegistrants(t.Context(), c, testCode, RegistrantsOptions{WithBillingAddress: true}); err == nil {
		t.Fatal("want an error when a transaction detail fails")
	}
}
