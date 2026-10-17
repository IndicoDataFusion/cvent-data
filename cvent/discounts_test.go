package cvent

import (
	"encoding/json"
	"testing"
)

func TestDiscountsFrom(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"id":"d1","code":" EXHIB ","name":"Exhibitor","type":"DISCOUNT_CODE",
			"active":true,"stackable":true,"method":{"type":"BY_PERCENTAGE","value":100},
			"level":"EVENT","audienceType":"ALL","includeGuestsTowardsCapacity":true,
			"capacity":{"total":3,"used":1},"effectiveTo":"2027-05-01T00:00:00Z"}`),
		json.RawMessage(`{"id":"d2","code":"OPEN","active":false,
			"method":{"type":"BY_AMOUNT","value":50},"capacity":{"total":-1,"used":7}}`),
		json.RawMessage(`{"id":"d3","code":"NOCAP","method":{"type":"FLAT_PRICE","value":200}}`),
		json.RawMessage(`not json`),
	}
	ds := DiscountsFrom(raw)
	if len(ds) != 3 {
		t.Fatalf("got %d discounts, want 3 (bad item skipped)", len(ds))
	}

	d := ds[0]
	if d.Code != "EXHIB" || d.Method != DiscountByPercentage || d.Value != 100 || !d.Active ||
		d.CapacityTotal != 3 || d.CapacityUsed != 1 || d.Remaining() != 2 || d.Unlimited() ||
		!d.IncludeGuestsTowardsCapacity || d.EffectiveTo == "" {
		t.Errorf("d1 = %+v", d)
	}
	if d := ds[1]; !d.Unlimited() || d.CapacityUsed != 7 || d.Remaining() != -1 || d.Active {
		t.Errorf("d2 = %+v, want unlimited, inactive, used 7", d)
	}
	if d := ds[2]; !d.Unlimited() || d.CapacityUsed != 0 || d.Method != DiscountFlatPrice {
		t.Errorf("d3 = %+v, want unlimited without a capacity", d)
	}

	full := Discount{CapacityTotal: 2, CapacityUsed: 5}
	if full.Remaining() != 0 {
		t.Errorf("over-redeemed Remaining = %d, want 0", full.Remaining())
	}
}
