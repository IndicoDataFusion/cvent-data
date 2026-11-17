package cvent

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
)

func TestWriteRegistrantsCSV(t *testing.T) {
	regs := []Registrant{
		{
			ID: "a1", FirstName: "Zoë", LastName: "Lovelace", FullName: "Zoë Lovelace",
			MobilePhone: "+1 555 0100", Company: "=HYPERLINK(\"x\")", CheckedIn: true,
			Address:      &Address{Address1: "1 Main St", City: "Lansing", RegionCode: "MI", CountryCode: "US"},
			Guests:       []PersonRef{{ID: "g1", FullName: "Byron Lovelace"}},
			Answers:      []Answer{{QuestionID: "q1", Question: "Dietary needs", Value: "Vegan"}},
			CustomFields: map[string]string{"Visa letter": "No"},
			Payment: &AttendeePayment{Status: "paid", Currency: "USD", Ordered: 360, Paid: 360,
				Discounts: []AppliedDiscount{{Code: "EARLY", Amount: 40}}, OriginalAmount: 400,
				Items: []OrderItem{{Name: "Registration"}, {Name: "Companion", ForGuest: true}}},
		},
		{
			ID: "g1", FullName: "Byron Lovelace", Guest: true, PrimaryID: "a1",
			RegisteredBy: &PersonRef{ID: "a1", FullName: "Zoë Lovelace", ConfirmationNumber: "C1"},
			Answers:      []Answer{{QuestionID: "q2", Value: "x"}},
			CustomFields: map[string]string{"Emergency contact": "Ann"},
		},
	}
	var buf bytes.Buffer
	if err := WriteRegistrantsCSV(&buf, regs); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("\xEF\xBB\xBF")) {
		t.Fatal("missing UTF-8 BOM")
	}
	rows, err := csv.NewReader(bytes.NewReader(buf.Bytes()[3:])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want header + 2", len(rows))
	}
	header := rows[0]
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	for _, r := range rows[1:] {
		if len(r) != len(header) {
			t.Fatalf("row has %d cells, header %d", len(r), len(header))
		}
	}
	want := map[string][2]string{ // column → (row 1, row 2)
		"id":                       {"a1", "g1"},
		"firstName":                {"Zoë", ""},
		"checkedIn":                {"true", "false"},
		"guest":                    {"false", "true"},
		"mobilePhone":              {"'+1 555 0100", ""},
		"company":                  {"'=HYPERLINK(\"x\")", ""},
		"city":                     {"Lansing", ""},
		"region":                   {"MI", ""},
		"country":                  {"US", ""},
		"guests":                   {"Byron Lovelace", ""},
		"registeredBy":             {"", "Zoë Lovelace"},
		"registeredByConfirmation": {"", "C1"},
		"primaryId":                {"", "a1"},
		"paymentStatus":            {"paid", ""},
		"ordered":                  {"360.00", ""},
		"due":                      {"0.00", ""},
		"originalAmount":           {"400.00", ""},
		"discounts":                {"EARLY", ""},
		"orderItems":               {"Registration; Companion (guest)", ""},
		"Q: Dietary needs":         {"Vegan", ""},
		"Q: q2":                    {"", "x"},
		"Field: Emergency contact": {"", "Ann"},
		"Field: Visa letter":       {"No", ""},
	}
	for name, w := range want {
		i, ok := col[name]
		if !ok {
			t.Errorf("no column %q in %s", name, strings.Join(header, ","))
			continue
		}
		if rows[1][i] != w[0] || rows[2][i] != w[1] {
			t.Errorf("%s = %q, %q; want %q, %q", name, rows[1][i], rows[2][i], w[0], w[1])
		}
	}
}

func TestWriteRegistrantsCSVEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRegistrantsCSV(&buf, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(buf.Bytes()[3:])).ReadAll()
	if err != nil || len(rows) != 1 || len(rows[0]) != len(registrantCSVColumns) {
		t.Fatalf("rows = %v, err %v; want only the fixed header", rows, err)
	}
}
