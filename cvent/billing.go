package cvent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

// billingFetchConcurrency bounds the parallel GET /transactions/{id} calls.
const billingFetchConcurrency = 4

// BillingAddresses returns each attendee's billing address, taken from the
// latest successful charge that has one. The address is only on the
// transaction detail (GET /transactions/{id}), so this makes one request
// per successful non-refund transaction in transactions (raw
// /events/{id}/transactions items). Card payments carry an address; offline
// ones (e.g. bank transfers) come back empty, and attendees with no
// addressed charge are absent from the map. The first failed request is
// returned as the error.
func (c *Client) BillingAddresses(ctx context.Context, transactions []json.RawMessage) (map[string]*Address, error) {
	type txn struct {
		ID       string `json:"id"`
		Attendee struct {
			ID string `json:"id"`
		} `json:"attendee"`
		Success     bool   `json:"success"`
		PaymentType string `json:"paymentType"`
		Date        string `json:"date"`
	}
	var todo []txn
	for _, raw := range transactions {
		var t txn
		if json.Unmarshal(raw, &t) != nil || t.ID == "" || t.Attendee.ID == "" ||
			!t.Success || strings.Contains(t.PaymentType, "Refund") {
			continue
		}
		todo = append(todo, t)
	}

	addrs := make([]*Address, len(todo))
	errs := make([]error, len(todo))
	sem := make(chan struct{}, billingFetchConcurrency)
	var wg sync.WaitGroup
	for i, t := range todo {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			raw, err := c.getObject(ctx, "/transactions/"+t.ID)
			if err != nil {
				errs[i] = err
				return
			}
			var d struct {
				BillingAddress *Address `json:"billingAddress"`
			}
			if json.Unmarshal(raw, &d) == nil && !d.BillingAddress.IsZero() {
				addrs[i] = d.BillingAddress
			}
		})
	}
	wg.Wait()

	out := map[string]*Address{}
	latest := map[string]string{} // attendee id → date of the chosen charge
	for i, t := range todo {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if addrs[i] == nil {
			continue
		}
		if d, ok := latest[t.Attendee.ID]; !ok || t.Date >= d { // RFC 3339 strings order by time
			latest[t.Attendee.ID] = t.Date
			out[t.Attendee.ID] = addrs[i]
		}
	}
	return out, nil
}
