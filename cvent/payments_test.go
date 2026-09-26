package cvent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Attendee fixture shape: the plan spec prescribes a nested name object with
// first/middle/last, e.g. {"id":"A","name":{"first":"Alice","last":"S."},"email":"a@x.io"}.
//
// Real-spec note (checked in openapi.json): components.schemas.attendee
// (line ~36869) resolves its name via contact -> AttendeeContactInfo ->
// AttendeeContact, which carries FLAT firstName/middleName/lastName strings;
// the nested `Name` schema (givenName/familyName/middleName) is only used by
// User. No live sample disambiguates the two. testAttendeeName below therefore accepts
// both shapes (nested first, flat as fallback). BuildPayments itself never
// parses attendees — it only receives an attendeeName callback.
var testAttendees = map[string]string{
	"A": `{"id":"A","name":{"first":"Alice","last":"S."},"email":"a@x.io"}`,
	"B": `{"id":"B","name":{"first":"Bob","last":"M."},"email":"b@x.io"}`,
	"C": `{"id":"C","name":{"first":"Carol","last":"W."},"email":"c@x.io"}`,
}

func testAttendeeName(id string) string {
	raw, ok := testAttendees[id]
	if !ok {
		return "#" + id
	}
	var a struct {
		Name struct {
			First  string `json:"first"`
			Middle string `json:"middle"`
			Last   string `json:"last"`
		} `json:"name"`
		FirstName  string `json:"firstName"`
		MiddleName string `json:"middleName"`
		LastName   string `json:"lastName"`
	}
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return "#" + id
	}
	first, middle, last := a.Name.First, a.Name.Middle, a.Name.Last
	if first == "" {
		first, middle, last = a.FirstName, a.MiddleName, a.LastName
	}
	name := first
	if middle != "" {
		name += " " + middle + "."
	}
	if last != "" {
		name += " " + last
	}
	return name
}

// Order fixtures: raw Cvent JSON per components.schemas.order-detail
// (openapi.json line ~58668): attendee -> Attendee {id}, amountOrdered /
// amountPaid / amountDue, paymentMethod, invoiceNumber, cancelled, type.
var fullOrders = []json.RawMessage{
	json.RawMessage(`{"attendee":{"id":"A"},"amountOrdered":500,"amountPaid":500,"amountDue":0,"paymentMethod":"Visa","invoiceNumber":"INV-1","type":"Online Charge","cancelled":false}`),
	json.RawMessage(`{"attendee":{"id":"B"},"amountOrdered":400,"amountPaid":150,"amountDue":250,"paymentMethod":"Invoice","invoiceNumber":"INV-2","type":"Authorization","cancelled":false}`),
	json.RawMessage(`{"attendee":{"id":"C"},"amountOrdered":300,"amountPaid":0,"amountDue":300,"invoiceNumber":"INV-3","type":"Online Charge","cancelled":false}`),
	json.RawMessage(`{"attendee":{"id":"B"},"amountOrdered":100,"amountPaid":0,"amountDue":100,"invoiceNumber":"INV-4","type":"Online Charge","cancelled":true}`),
}

// Transaction fixtures: raw Cvent JSON per
// components.schemas.transaction-detail-response (openapi.json line ~70728):
// success (bool), paymentType, paymentMethod, amount.
var fullTxns = []json.RawMessage{
	json.RawMessage(`{"success":true,"paymentType":"Payment","paymentMethod":"Visa","amount":500}`),
	json.RawMessage(`{"success":true,"paymentType":"Payment","paymentMethod":"Invoice","amount":150}`),
	json.RawMessage(`{"success":true,"paymentType":"Refund","amount":80}`),
	json.RawMessage(`{"success":false,"paymentType":"Payment","amount":300}`),
}

// testTotals builds the anonymous Totals struct for test expectations.
func testTotals(ordered, paid, due, refunded float64) (t struct {
	Ordered  float64 `json:"ordered"`
	Paid     float64 `json:"paid"`
	Due      float64 `json:"due"`
	Refunded float64 `json:"refunded"`
}) {
	t.Ordered, t.Paid, t.Due, t.Refunded = ordered, paid, due, refunded
	return t
}

func TestBuildPayments(t *testing.T) {
	cases := []struct {
		name          string
		orders        []json.RawMessage
		txn           []json.RawMessage
		nameFn        func(id string) string
		want          PaymentSummary
		wantJSONSubst []string // substrings the marshaled summary must contain
	}{
		{
			name:   "full join: totals, statuses, sort by due desc, cancelled excluded",
			orders: fullOrders,
			txn:    fullTxns,
			nameFn: testAttendeeName,
			want: PaymentSummary{
				Totals: testTotals(1200, 650, 550, 80),
				Orders: []OrderRow{
					{Attendee: "Carol W.", Invoice: "INV-3", AmountOrdered: 300, AmountPaid: 0, AmountDue: 300, Status: "unpaid", Type: "Online Charge"},
					{Attendee: "Bob M.", Invoice: "INV-2", AmountOrdered: 400, AmountPaid: 150, AmountDue: 250, Status: "partial", Method: "Invoice", Type: "Authorization"},
					{Attendee: "Alice S.", Invoice: "INV-1", AmountOrdered: 500, AmountPaid: 500, AmountDue: 0, Status: "paid", Method: "Visa", Type: "Online Charge"},
				},
				Cancelled: []OrderRow{
					{Attendee: "Bob M.", Invoice: "INV-4", AmountOrdered: 100, AmountPaid: 0, AmountDue: 100, Status: "cancelled", Type: "Online Charge"},
				},
			},
		},
		{
			name:   "unknown attendee id: row kept with #<id> name from callback",
			orders: []json.RawMessage{json.RawMessage(`{"attendee":{"id":"X1"},"amountOrdered":10,"amountPaid":0,"amountDue":10}`)},
			txn:    nil,
			nameFn: testAttendeeName,
			want: PaymentSummary{
				Totals: testTotals(10, 0, 10, 0),
				Orders: []OrderRow{
					{Attendee: "#X1", AmountOrdered: 10, AmountDue: 10, Status: "unpaid"},
				},
				Cancelled: []OrderRow{},
			},
		},
		{
			name:   "callback returns empty: BuildPayments substitutes #<id> and keeps row",
			orders: []json.RawMessage{json.RawMessage(`{"attendee":{"id":"X2"},"amountOrdered":5,"amountPaid":5,"amountDue":0}`)},
			txn:    nil,
			nameFn: func(id string) string { return "" },
			want: PaymentSummary{
				Totals: testTotals(5, 5, 0, 0),
				Orders: []OrderRow{
					{Attendee: "#X2", AmountOrdered: 5, AmountPaid: 5, AmountDue: 0, Status: "paid"},
				},
				Cancelled: []OrderRow{},
			},
		},
		{
			name: "lenient attendee id paths: contact.id, attendeeId, attendee.id wins",
			orders: []json.RawMessage{
				json.RawMessage(`{"contact":{"id":"A"},"amountOrdered":1,"amountPaid":1,"amountDue":0}`),
				json.RawMessage(`{"attendeeId":"B","amountOrdered":1,"amountPaid":1,"amountDue":0}`),
				json.RawMessage(`{"attendee":{"id":"A"},"contact":{"id":"B"},"attendeeId":"C","amountOrdered":1,"amountPaid":1,"amountDue":0}`),
			},
			txn:    nil,
			nameFn: testAttendeeName,
			want: PaymentSummary{
				Totals: testTotals(3, 3, 0, 0),
				Orders: []OrderRow{
					{Attendee: "Alice S.", AmountOrdered: 1, AmountPaid: 1, AmountDue: 0, Status: "paid"},
					{Attendee: "Bob M.", AmountOrdered: 1, AmountPaid: 1, AmountDue: 0, Status: "paid"},
					{Attendee: "Alice S.", AmountOrdered: 1, AmountPaid: 1, AmountDue: 0, Status: "paid"},
				},
				Cancelled: []OrderRow{},
			},
		},
		{
			name:   "null amounts treated as zero",
			orders: []json.RawMessage{json.RawMessage(`{"attendee":{"id":"A"},"amountOrdered":null,"amountPaid":null,"amountDue":null}`)},
			txn:    nil,
			nameFn: testAttendeeName,
			want: PaymentSummary{
				Totals: testTotals(0, 0, 0, 0),
				Orders: []OrderRow{
					{Attendee: "Alice S.", AmountOrdered: 0, AmountPaid: 0, AmountDue: 0, Status: "paid"},
				},
				Cancelled: []OrderRow{},
			},
		},
		{
			name:          "empty inputs: zero totals, empty non-nil slices render as []",
			orders:        nil,
			txn:           nil,
			nameFn:        testAttendeeName,
			want:          PaymentSummary{Orders: []OrderRow{}, Cancelled: []OrderRow{}},
			wantJSONSubst: []string{`"orders":[]`, `"cancelled":[]`},
		},
		{
			name:   "failed transactions never count toward paid or refunded totals",
			orders: nil,
			txn: []json.RawMessage{
				json.RawMessage(`{"success":false,"paymentType":"Refund","amount":50}`),
				json.RawMessage(`{"success":false,"paymentType":"Payment","amount":200}`),
			},
			nameFn: testAttendeeName,
			want:   PaymentSummary{Orders: []OrderRow{}, Cancelled: []OrderRow{}},
		},
		{
			name: "floats rounded to 2 decimals in output",
			orders: []json.RawMessage{
				json.RawMessage(`{"attendee":{"id":"A"},"amountOrdered":0.1,"amountPaid":0,"amountDue":0.1}`),
				json.RawMessage(`{"attendee":{"id":"B"},"amountOrdered":0.2,"amountPaid":0,"amountDue":0.2}`),
			},
			txn:    nil,
			nameFn: testAttendeeName,
			want: PaymentSummary{
				Totals: testTotals(0.3, 0, 0.3, 0),
				Orders: []OrderRow{
					{Attendee: "Bob M.", AmountOrdered: 0.2, AmountDue: 0.2, Status: "unpaid"},
					{Attendee: "Alice S.", AmountOrdered: 0.1, AmountDue: 0.1, Status: "unpaid"},
				},
				Cancelled: []OrderRow{},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildPayments(tc.orders, tc.txn, tc.nameFn)
			// Fill expected totals from the want struct's totals (set below per case).
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
			for _, sub := range tc.wantJSONSubst {
				b, err := json.Marshal(got)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if !strings.Contains(string(b), sub) {
					t.Errorf("marshaled JSON %s does not contain %q", b, sub)
				}
			}
		})
	}
}

// Exact totals for the full-join scenario (asserted separately so the
// expected numbers are impossible to miss in the diff).
func TestBuildPaymentsFullTotals(t *testing.T) {
	got := BuildPayments(fullOrders, fullTxns, testAttendeeName)
	want := struct {
		Ordered, Paid, Due, Refunded float64
	}{Ordered: 1200, Paid: 650, Due: 550, Refunded: 80}
	if got.Totals.Ordered != want.Ordered || got.Totals.Paid != want.Paid ||
		got.Totals.Due != want.Due || got.Totals.Refunded != want.Refunded {
		t.Errorf("totals = {ordered:%v paid:%v due:%v refunded:%v}, want %+v",
			got.Totals.Ordered, got.Totals.Paid, got.Totals.Due, got.Totals.Refunded, want)
	}
	if len(got.Orders) != 3 {
		t.Errorf("orders = %d rows, want 3", len(got.Orders))
	}
	if len(got.Cancelled) != 1 {
		t.Errorf("cancelled = %d rows, want 1", len(got.Cancelled))
	}
	if got.Cancelled[0].Invoice != "INV-4" {
		t.Errorf("cancelled row = %+v, want INV-4", got.Cancelled[0])
	}
}
