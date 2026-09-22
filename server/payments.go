package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// PaymentSummary is the "money view" of an event: order rows joined to
// attendee display names plus refund totals from transactions. Built purely
// from raw Cvent JSON (no network, no globals) so it is unit-testable.
type PaymentSummary struct {
	Totals struct {
		Ordered  float64 `json:"ordered"`
		Paid     float64 `json:"paid"`
		Due      float64 `json:"due"`
		Refunded float64 `json:"refunded"`
	} `json:"totals"`
	Orders    []OrderRow `json:"orders"`
	Cancelled []OrderRow `json:"cancelled"`
}

// OrderRow is one order joined to its attendee display name.
type OrderRow struct {
	Attendee      string  `json:"attendee"`
	Invoice       string  `json:"invoice,omitempty"`
	AmountOrdered float64 `json:"amountOrdered"`
	AmountPaid    float64 `json:"amountPaid"`
	AmountDue     float64 `json:"amountDue"`
	Status        string  `json:"status"` // paid | partial | unpaid (orders); "cancelled" in Cancelled rows
	Method        string  `json:"method,omitempty"`
	Type          string  `json:"type,omitempty"`
}

// buildOrder is the tolerant parse shape for one element of the orders array.
// Field paths mirror components.schemas.order-detail in openapi.json
// (line ~58668): attendee -> components.schemas.Attendee {id}, plus
// amountOrdered / amountPaid / amountDue / paymentMethod / invoiceNumber /
// cancelled / type. The attendee-id lookup is lenient because live payloads
// vary: order.attendee.id (the spec path) first, then order.contact.id,
// then a top-level order.attendeeId string; first non-empty wins.
type buildOrder struct {
	Attendee struct {
		ID string `json:"id"`
	} `json:"attendee"`
	Contact struct {
		ID string `json:"id"`
	} `json:"contact"`
	AttendeeId    string  `json:"attendeeId"`
	AmountOrdered float64 `json:"amountOrdered"`
	AmountPaid    float64 `json:"amountPaid"`
	AmountDue     float64 `json:"amountDue"`
	PaymentMethod string  `json:"paymentMethod"`
	InvoiceNumber string  `json:"invoiceNumber"`
	Cancelled     bool    `json:"cancelled"`
	Type          string  `json:"type"`
}

// buildTxn is the tolerant parse shape for one element of the transactions
// array. Field paths mirror components.schemas.transaction-detail-response
// in openapi.json (line ~70728): success (bool), paymentType, amount.
type buildTxn struct {
	Success     bool    `json:"success"`
	PaymentType string  `json:"paymentType"`
	Amount      float64 `json:"amount"`
}

// BuildPayments joins raw Cvent order + transaction JSON into a
// PaymentSummary.
//
// orders / transactions are the raw JSON arrays as stored by the event
// bundle (may be nil or empty). Each element is parsed tolerantly: unknown
// fields are ignored, missing/null amounts count as 0.
//
// attendeeName maps an attendee id to a display name; it is called for every
// non-empty id found on an order. Rows are NEVER dropped: if the id is
// missing, or the callback returns "", the row falls back to "#<id>" (or
// "#?" if there is no id at all).
//
// Status rules: cancelled orders go to Cancelled; otherwise
// amountDue <= 0 -> "paid", amountPaid > 0 -> "partial", else "unpaid".
// Totals sum NON-cancelled orders for ordered/paid/due. Refunded sums
// transaction amounts where success == true and paymentType contains
// "Refund"; failed transactions (success == false) never count.
// Order rows are sorted by amountDue descending (stable, so input order
// breaks ties) — the most actionable (unpaid) first.
func BuildPayments(orders, transactions []json.RawMessage, attendeeName func(id string) string) PaymentSummary {
	sum := PaymentSummary{Orders: []OrderRow{}, Cancelled: []OrderRow{}}

	for _, raw := range orders {
		var o buildOrder
		if err := json.Unmarshal(raw, &o); err != nil {
			continue // unparseable element: skip, never fatal
		}
		id := o.Attendee.ID
		if id == "" {
			id = o.Contact.ID
		}
		if id == "" {
			id = o.AttendeeId
		}
		name := ""
		if id != "" && attendeeName != nil {
			name = attendeeName(id)
		}
		if name == "" {
			if id != "" {
				name = "#" + id
			} else {
				name = "#?"
			}
		}
		row := OrderRow{
			Attendee:      name,
			Invoice:       o.InvoiceNumber,
			AmountOrdered: round2(o.AmountOrdered),
			AmountPaid:    round2(o.AmountPaid),
			AmountDue:     round2(o.AmountDue),
			Method:        o.PaymentMethod,
			Type:          o.Type,
		}
		if o.Cancelled {
			row.Status = "cancelled"
			sum.Cancelled = append(sum.Cancelled, row)
			continue
		}
		switch {
		case o.AmountDue <= 0:
			row.Status = "paid"
		case o.AmountPaid > 0:
			row.Status = "partial"
		default:
			row.Status = "unpaid"
		}
		sum.Orders = append(sum.Orders, row)
		sum.Totals.Ordered += o.AmountOrdered
		sum.Totals.Paid += o.AmountPaid
		sum.Totals.Due += o.AmountDue
	}

	for _, raw := range transactions {
		var tr buildTxn
		if err := json.Unmarshal(raw, &tr); err != nil {
			continue
		}
		// Failed transactions never count, toward paid or refunded.
		if !tr.Success {
			continue
		}
		if strings.Contains(tr.PaymentType, "Refund") {
			sum.Totals.Refunded += tr.Amount
		}
	}

	// Most actionable first: unpaid (highest due) at the top.
	sort.SliceStable(sum.Orders, func(i, j int) bool {
		return sum.Orders[i].AmountDue > sum.Orders[j].AmountDue
	})

	sum.Totals.Ordered = round2(sum.Totals.Ordered)
	sum.Totals.Paid = round2(sum.Totals.Paid)
	sum.Totals.Due = round2(sum.Totals.Due)
	sum.Totals.Refunded = round2(sum.Totals.Refunded)
	return sum
}

// round2 rounds to 2 decimal places, avoiding float artifacts like
// 649.9999999999999 in JSON output.
func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
