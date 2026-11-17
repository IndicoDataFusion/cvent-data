package cvent

import (
	"encoding/csv"
	"io"
	"sort"
	"strconv"
	"strings"
)

// WriteRegistrantsCSV writes RegistrantsTable(regs) as CSV, for opening in
// a spreadsheet.
//
// The output starts with a UTF-8 byte-order mark so Excel reads non-ASCII
// names correctly. Text that a spreadsheet would run as a formula (leading
// =, +, -, @, tab or CR) is prefixed with an apostrophe.
func WriteRegistrantsCSV(w io.Writer, regs []Registrant) error {
	header, rows := RegistrantsTable(regs)
	if _, err := io.WriteString(w, "\xEF\xBB\xBF"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	for _, row := range append([][]string{header}, rows...) {
		safe := make([]string, len(row))
		for i, cell := range row {
			safe[i] = csvSafe(cell)
		}
		if err := cw.Write(safe); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// RegistrantsTable lays regs out as a table, one row per registrant, with
// the values as-is (no CSV escaping; see WriteRegistrantsCSV). The fixed
// columns are the scalar Registrant fields (the contact address split into
// parts; the other and billing addresses on one line each), guest links and
// the payment summary. They are followed by one "Q: <question>" column per
// event question and one "Field: <name>" column per contact custom field,
// in first-seen order and name order respectively, so every row has the
// same columns.
func RegistrantsTable(regs []Registrant) (header []string, rows [][]string) {
	// Dynamic columns: event questions (keyed by id, labelled by text) and
	// custom field names.
	type question struct{ id, label string }
	var questions []question
	seenQ := map[string]bool{}
	fieldSet := map[string]bool{}
	for i := range regs {
		for _, a := range regs[i].Answers {
			if seenQ[a.QuestionID] {
				continue
			}
			seenQ[a.QuestionID] = true
			label := a.Question
			if label == "" {
				label = a.QuestionID
			}
			questions = append(questions, question{a.QuestionID, label})
		}
		for name := range regs[i].CustomFields {
			fieldSet[name] = true
		}
	}
	fields := make([]string, 0, len(fieldSet))
	for name := range fieldSet {
		fields = append(fields, name)
	}
	sort.Strings(fields)

	header = append([]string(nil), registrantCSVColumns...)
	for _, q := range questions {
		header = append(header, "Q: "+q.label)
	}
	for _, f := range fields {
		header = append(header, "Field: "+f)
	}

	rows = make([][]string, 0, len(regs))
	for i := range regs {
		r := &regs[i]
		row := registrantCSVRow(r)
		answers := make(map[string]string, len(r.Answers))
		for _, a := range r.Answers {
			answers[a.QuestionID] = a.Value
		}
		for _, q := range questions {
			row = append(row, answers[q.id])
		}
		for _, f := range fields {
			row = append(row, r.CustomFields[f])
		}
		rows = append(rows, row)
	}
	return header, rows
}

// registrantCSVColumns are the fixed leading columns of WriteRegistrantsCSV,
// in the order registrantCSVRow fills them.
var registrantCSVColumns = []string{
	"id", "confirmationNumber", "status", "registrationType", "admissionItem",
	"registrationPath", "registeredAt", "cancelledAt", "checkedIn", "checkedInAt",
	"prefix", "firstName", "middleName", "lastName", "fullName", "nickname",
	"designation", "pronouns", "email", "ccEmail", "company", "title",
	"mobilePhone", "workPhone", "homePhone", "website", "contactType", "ref",
	"note", "emergencyContactName", "emergencyContactPhone", "visaLetter",
	"addressType", "address1", "address2", "address3", "city", "region",
	"postalCode", "country", "otherAddress", "billingAddress",
	"guest", "primaryId", "registeredBy", "registeredByConfirmation", "guests",
	"paymentStatus", "currency", "ordered", "paid", "due", "refunded",
	"originalAmount", "paymentMethod", "lastPaymentDate", "invoices",
	"discounts", "orderItems",
}

func registrantCSVRow(r *Registrant) []string {
	text := []string{
		r.ID, r.ConfirmationNumber, r.Status, r.RegistrationType, r.AdmissionItem,
		r.RegistrationPath, r.RegisteredAt, r.CancelledAt, "", r.CheckedInAt,
		r.Prefix, r.FirstName, r.MiddleName, r.LastName, r.FullName, r.Nickname,
		r.Designation, r.Pronouns, r.Email, r.CCEmail, r.Company, r.Title,
		r.MobilePhone, r.WorkPhone, r.HomePhone, r.Website, r.ContactType, r.Ref,
		r.Note, r.EmergencyContactName, r.EmergencyContactPhone, r.VisaLetter,
		r.AddressType,
	}
	ad := r.Address
	if ad == nil {
		ad = &Address{}
	}
	text = append(text, ad.Address1, ad.Address2, ad.Address3, ad.City,
		firstNonEmpty(ad.Region, ad.RegionCode), ad.PostalCode,
		firstNonEmpty(ad.Country, ad.CountryCode),
		r.OtherAddress.String(), r.BillingAddress.String())

	var byName, byConf string
	if r.RegisteredBy != nil {
		byName, byConf = r.RegisteredBy.FullName, r.RegisteredBy.ConfirmationNumber
	}
	guests := make([]string, 0, len(r.Guests))
	for _, g := range r.Guests {
		guests = append(guests, g.FullName)
	}
	text = append(text, strconv.FormatBool(r.Guest), r.PrimaryID, byName, byConf, strings.Join(guests, "; "))
	text[8] = strconv.FormatBool(r.CheckedIn)

	p := r.Payment
	if p == nil {
		return append(text, make([]string, 12)...)
	}
	money := func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }
	discounts := make([]string, 0, len(p.Discounts))
	for _, d := range p.Discounts {
		discounts = append(discounts, firstNonEmpty(d.Code, d.Name))
	}
	items := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		s := it.Name
		if it.ForGuest {
			s += " (guest)"
		}
		items = append(items, s)
	}
	original := ""
	if p.OriginalAmount != 0 {
		original = money(p.OriginalAmount)
	}
	return append(text,
		p.Status, p.Currency,
		money(p.Ordered), money(p.Paid), money(p.Due), money(p.Refunded), original,
		p.Method, p.LastPaymentDate,
		strings.Join(p.Invoices, "; "),
		strings.Join(discounts, "; "),
		strings.Join(items, "; "),
	)
}

// csvSafe defuses spreadsheet formula injection: a cell starting with
// =, +, -, @, tab or CR gets a leading apostrophe, which spreadsheets show
// as text and do not evaluate.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
