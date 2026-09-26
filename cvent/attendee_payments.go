package cvent

import (
	"encoding/json"
	"strings"
)

// Attendee payment statuses (AttendeePayment.Status).
const (
	PaymentPaid      = "paid"      // something was paid and nothing is due
	PaymentPartial   = "partial"   // paid in part, a balance is due
	PaymentUnpaid    = "unpaid"    // nothing paid, a balance is due
	PaymentWaived    = "waived"    // nothing was charged (e.g. a 100% discount)
	PaymentCancelled = "cancelled" // every order is cancelled
)

// AttendeePayment is one attendee's money position, summed over their
// non-cancelled orders, plus their successful transactions.
type AttendeePayment struct {
	Status   string  `json:"status"`
	Ordered  float64 `json:"ordered"`
	Paid     float64 `json:"paid"`
	Due      float64 `json:"due"`
	Refunded float64 `json:"refunded,omitempty"`
	Currency string  `json:"currency,omitempty"`
	// Method is the payment method of the latest successful charge, else of
	// the latest order.
	Method string `json:"method,omitempty"`
	// LastPaymentDate is the date of the latest successful charge.
	LastPaymentDate string            `json:"lastPaymentDate,omitempty"`
	Invoices        []string          `json:"invoices,omitempty"`
	Discounts       []AppliedDiscount `json:"discounts,omitempty"`
	// OriginalAmount is the amount before discounts (Ordered + the discount
	// amounts); 0 when no discount applies.
	OriginalAmount float64 `json:"originalAmount,omitempty"`
}

// AppliedDiscount is a discount on one of the attendee's orders.
type AppliedDiscount struct {
	Code   string  `json:"code,omitempty"`
	Name   string  `json:"name,omitempty"`
	Type   string  `json:"type,omitempty"`  // e.g. "Percentage"
	Value  float64 `json:"value,omitempty"` // percent for Percentage, else the configured amount
	Amount float64 `json:"amount"`          // amount taken off this order
}

// PaymentsByAttendee sums raw order and transaction items per attendee id.
// Attendees without orders are absent from the map. Status rules: all orders
// cancelled → cancelled; nothing charged → waived; nothing due → paid; some
// paid → partial; else unpaid. Refunds are successful transactions whose
// paymentType contains "Refund"; failed transactions never count.
func PaymentsByAttendee(orders, transactions []json.RawMessage) map[string]*AttendeePayment {
	out := map[string]*AttendeePayment{}
	active := map[string]int{} // non-cancelled orders per attendee
	get := func(id string) *AttendeePayment {
		p := out[id]
		if p == nil {
			p = &AttendeePayment{}
			out[id] = p
		}
		return p
	}

	for _, raw := range orders {
		var o struct {
			buildOrder
			Discounts []AppliedDiscount `json:"discounts"`
		}
		if json.Unmarshal(raw, &o) != nil {
			continue
		}
		id := firstNonEmpty(o.Attendee.ID, o.Contact.ID, o.AttendeeId)
		if id == "" {
			continue
		}
		p := get(id)
		if o.Cancelled {
			continue
		}
		active[id]++
		p.Ordered += o.AmountOrdered
		p.Paid += o.AmountPaid
		p.Due += o.AmountDue
		if o.PaymentMethod != "" {
			p.Method = o.PaymentMethod
		}
		if o.InvoiceNumber != "" {
			p.Invoices = append(p.Invoices, o.InvoiceNumber)
		}
		for _, d := range o.Discounts {
			d.Amount = round2(d.Amount)
			p.Discounts = append(p.Discounts, d)
			p.OriginalAmount += d.Amount
		}
	}

	for _, raw := range transactions {
		var t struct {
			buildTxn
			Attendee struct {
				ID string `json:"id"`
			} `json:"attendee"`
			Currency      string `json:"currency"`
			PaymentMethod string `json:"paymentMethod"`
			Date          string `json:"date"`
		}
		if json.Unmarshal(raw, &t) != nil || !t.Success {
			continue
		}
		p := out[t.Attendee.ID]
		if p == nil {
			continue // a transaction without an order of this attendee
		}
		if t.Currency != "" {
			p.Currency = t.Currency
		}
		if strings.Contains(t.PaymentType, "Refund") {
			p.Refunded += t.Amount
			continue
		}
		if t.Date >= p.LastPaymentDate { // RFC 3339 strings order by time
			p.LastPaymentDate = t.Date
			if t.PaymentMethod != "" {
				p.Method = t.PaymentMethod
			}
		}
	}

	for id, p := range out {
		p.Ordered, p.Paid, p.Due, p.Refunded = round2(p.Ordered), round2(p.Paid), round2(p.Due), round2(p.Refunded)
		if len(p.Discounts) > 0 {
			p.OriginalAmount = round2(p.Ordered + p.OriginalAmount)
		}
		switch {
		case active[id] == 0:
			p.Status = PaymentCancelled
		case p.Ordered <= 0 && p.Paid <= 0:
			p.Status = PaymentWaived
		case p.Due <= 0:
			p.Status = PaymentPaid
		case p.Paid > 0:
			p.Status = PaymentPartial
		default:
			p.Status = PaymentUnpaid
		}
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
