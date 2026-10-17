package cvent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Discount methods (Discount.Method).
const (
	DiscountByAmount     = "BY_AMOUNT"
	DiscountByPercentage = "BY_PERCENTAGE"
	DiscountFlatPrice    = "FLAT_PRICE"
)

// Discount is one of an event's discounts (GET /events/{id}/discounts): a
// code with its value, limits and redemption count. Orders reference it by
// code (AppliedDiscount.Code).
type Discount struct {
	ID        string `json:"id"`
	Code      string `json:"code,omitempty"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type,omitempty"` // e.g. "DISCOUNT_CODE"
	Active    bool   `json:"active"`
	Stackable bool   `json:"stackable,omitempty"`
	AutoApply bool   `json:"autoApply,omitempty"`
	// Method is how Value applies: BY_PERCENTAGE (percent), BY_AMOUNT (amount
	// off) or FLAT_PRICE (the price charged).
	Method string  `json:"method,omitempty"`
	Value  float64 `json:"value"`
	// Level is what the discount applies to, e.g. "EVENT" or "ADMISSION";
	// AudienceType is PRIMARY, GUEST or ALL.
	Level                        string `json:"level,omitempty"`
	AudienceType                 string `json:"audienceType,omitempty"`
	ApplyToAllAgendaItems        bool   `json:"applyToAllAgendaItems,omitempty"`
	IncludeGuestsTowardsCapacity bool   `json:"includeGuestsTowardsCapacity,omitempty"`
	// CapacityTotal is how many times the discount may be redeemed event
	// wide, -1 when unlimited; CapacityUsed is how many times it was.
	CapacityTotal int    `json:"capacityTotal"`
	CapacityUsed  int    `json:"capacityUsed"`
	EffectiveFrom string `json:"effectiveFrom,omitempty"` // ISO 8601
	EffectiveTo   string `json:"effectiveTo,omitempty"`   // ISO 8601
	Note          string `json:"note,omitempty"`
	Created       string `json:"created,omitempty"`
	LastModified  string `json:"lastModified,omitempty"`
}

// Unlimited reports whether the discount has no redemption limit.
func (d *Discount) Unlimited() bool { return d.CapacityTotal < 0 }

// Remaining is the number of redemptions left, -1 when unlimited.
func (d *Discount) Remaining() int {
	if d.Unlimited() {
		return -1
	}
	return max(d.CapacityTotal-d.CapacityUsed, 0)
}

// DiscountsFrom converts raw /events/{id}/discounts items into Discounts.
// Items that do not parse are skipped.
func DiscountsFrom(raw []json.RawMessage) []Discount {
	out := make([]Discount, 0, len(raw))
	for _, r := range raw {
		var d struct {
			ID                           string `json:"id"`
			Code                         string `json:"code"`
			Name                         string `json:"name"`
			Type                         string `json:"type"`
			Active                       bool   `json:"active"`
			Stackable                    bool   `json:"stackable"`
			AutoApply                    bool   `json:"autoApply"`
			Level                        string `json:"level"`
			AudienceType                 string `json:"audienceType"`
			ApplyToAllAgendaItems        bool   `json:"applyToAllAgendaItems"`
			IncludeGuestsTowardsCapacity bool   `json:"includeGuestsTowardsCapacity"`
			Method                       struct {
				Type  string  `json:"type"`
				Value float64 `json:"value"`
			} `json:"method"`
			Capacity *struct {
				Total *int `json:"total"`
				Used  int  `json:"used"`
			} `json:"capacity"`
			EffectiveFrom string `json:"effectiveFrom"`
			EffectiveTo   string `json:"effectiveTo"`
			Note          string `json:"note"`
			Created       string `json:"created"`
			LastModified  string `json:"lastModified"`
		}
		if json.Unmarshal(r, &d) != nil {
			continue
		}
		disc := Discount{
			ID:                           d.ID,
			Code:                         strings.TrimSpace(d.Code),
			Name:                         strings.TrimSpace(d.Name),
			Type:                         d.Type,
			Active:                       d.Active,
			Stackable:                    d.Stackable,
			AutoApply:                    d.AutoApply,
			Method:                       d.Method.Type,
			Value:                        d.Method.Value,
			Level:                        d.Level,
			AudienceType:                 d.AudienceType,
			ApplyToAllAgendaItems:        d.ApplyToAllAgendaItems,
			IncludeGuestsTowardsCapacity: d.IncludeGuestsTowardsCapacity,
			CapacityTotal:                -1, // no capacity = unlimited
			EffectiveFrom:                d.EffectiveFrom,
			EffectiveTo:                  d.EffectiveTo,
			Note:                         strings.TrimSpace(d.Note),
			Created:                      d.Created,
			LastModified:                 d.LastModified,
		}
		if c := d.Capacity; c != nil {
			disc.CapacityUsed = c.Used
			if c.Total != nil {
				disc.CapacityTotal = *c.Total
			}
		}
		out = append(out, disc)
	}
	return out
}

// FetchDiscounts resolves the event (short code or uuid) and returns its
// discounts: one paged GET /events/{id}/discounts.
func FetchDiscounts(ctx context.Context, c *Client, code string) ([]Discount, error) {
	_, ev, err := c.resolveEvent(ctx, code)
	if err != nil {
		return nil, err
	}
	raw, err := c.listAll(ctx, "/events/"+ev.ID+"/discounts", nil)
	if err != nil {
		return nil, fmt.Errorf("discounts: %w", err)
	}
	return DiscountsFrom(raw), nil
}
