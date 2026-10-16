package cvent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

// Registrant is a Cvent attendee reduced to the fields an external system
// needs to match it against its own registrations, plus the registration
// form's contact details. Ref is the attendee's answer to the "Indico
// registration reference number (Ref #)" question, trimmed but otherwise as
// typed (it may be a non-numeric confirmation number, or empty).
//
// Guests (companions) are registrants of their own with Guest set,
// PrimaryID naming the attendee who registered them and RegisteredBy
// describing that attendee; the primary lists them in Guests. A guest's fee
// is an item on the primary's order (OrderItem.ForGuest), so a guest has no
// Payment of their own.
//
// Registrant is the one attendee record of this module: FetchRegistrants
// and RegistrantsFromBundle build it, the server's attendee search returns
// it, and the dump writes it to registrants.json.
type Registrant struct {
	ID                 string `json:"id"`
	Prefix             string `json:"prefix,omitempty"` // e.g. "Dr."
	FirstName          string `json:"firstName"`
	MiddleName         string `json:"middleName,omitempty"`
	LastName           string `json:"lastName"`
	FullName           string `json:"fullName"`
	Nickname           string `json:"nickname,omitempty"`
	Designation        string `json:"designation,omitempty"`
	Pronouns           string `json:"pronouns,omitempty"`
	Email              string `json:"email,omitempty"`
	CCEmail            string `json:"ccEmail,omitempty"`
	Company            string `json:"company,omitempty"`
	Title              string `json:"title,omitempty"` // job title
	MobilePhone        string `json:"mobilePhone,omitempty"`
	WorkPhone          string `json:"workPhone,omitempty"`
	HomePhone          string `json:"homePhone,omitempty"`
	Website            string `json:"website,omitempty"`
	ContactType        string `json:"contactType,omitempty"`
	Ref                string `json:"ref,omitempty"`
	Status             string `json:"status,omitempty"`
	RegistrationType   string `json:"registrationType,omitempty"` // the member type, e.g. "IEEE NPSS Member"
	AdmissionItem      string `json:"admissionItem,omitempty"`
	RegistrationPath   string `json:"registrationPath,omitempty"`
	RegisteredAt       string `json:"registeredAt,omitempty"` // RFC 3339
	CancelledAt        string `json:"cancelledAt,omitempty"`  // RFC 3339
	Note               string `json:"note,omitempty"`         // set by the planner, not the registrant
	ConfirmationNumber string `json:"confirmationNumber,omitempty"`
	CheckedIn          bool   `json:"checkedIn"`
	CheckedInAt        string `json:"checkedInAt,omitempty"` // RFC 3339
	// From the contact custom fields matched by name (see
	// EmergencyContactNamePattern and friends); the raw answers are in
	// CustomFields.
	EmergencyContactName  string `json:"emergencyContactName,omitempty"`
	EmergencyContactPhone string `json:"emergencyContactPhone,omitempty"`
	VisaLetter            string `json:"visaLetter,omitempty"` // answer as given: "Yes", "No" or empty
	// Address is the contact's primary address and AddressType its kind
	// ("Work" or "Home", per primaryAddressType); OtherAddress is the other
	// kind when that was filled in too. Nil when none was entered.
	Address      *Address `json:"address,omitempty"`
	AddressType  string   `json:"addressType,omitempty"`
	OtherAddress *Address `json:"otherAddress,omitempty"`
	// BillingAddress is the billing address of the attendee's latest card
	// payment; set with RegistrantsOptions.WithBillingAddress, and by
	// RegistrantsFromBundle from EventBundle.BillingAddresses. See
	// InvoiceAddress.
	BillingAddress *Address    `json:"billingAddress,omitempty"`
	Guest          bool        `json:"guest,omitempty"`
	PrimaryID      string      `json:"primaryId,omitempty"`
	RegisteredBy   *PersonRef  `json:"registeredBy,omitempty"` // a guest's primary
	Guests         []PersonRef `json:"guests,omitempty"`
	// CustomFields maps every answered contact custom field name to its
	// answer (multiple values joined with "; ").
	CustomFields map[string]string `json:"customFields,omitempty"`
	// Answers are the attendee's event-question answers, in Cvent's order.
	Answers []Answer `json:"answers,omitempty"`
	// Payment is the attendee's order/transaction summary; nil when payments
	// were not requested or the attendee has no order.
	Payment *AttendeePayment `json:"payment,omitempty"`
}

// Address is a postal address from a contact or a payment.
type Address struct {
	Address1    string `json:"address1,omitempty"`
	Address2    string `json:"address2,omitempty"`
	Address3    string `json:"address3,omitempty"`
	City        string `json:"city,omitempty"`
	Region      string `json:"region,omitempty"`
	RegionCode  string `json:"regionCode,omitempty"`
	PostalCode  string `json:"postalCode,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"countryCode,omitempty"`
}

// IsZero reports whether no street, city or postal code is set.
func (a *Address) IsZero() bool {
	return a == nil || a.Address1 == "" && a.Address2 == "" && a.City == "" && a.PostalCode == ""
}

// String formats the address on one line, comma-separated.
func (a *Address) String() string {
	if a == nil {
		return ""
	}
	country := a.Country
	if country == "" {
		country = a.CountryCode
	}
	return strings.Join(nonEmpty(a.Address1, a.Address2, a.Address3, a.City,
		firstNonEmpty(a.Region, a.RegionCode), a.PostalCode, country), ", ")
}

// PersonRef names a linked registrant: a primary's guest, or a guest's
// primary. ConfirmationNumber finds them in an attendee search.
type PersonRef struct {
	ID                 string `json:"id"`
	FullName           string `json:"fullName"`
	Email              string `json:"email,omitempty"`
	ConfirmationNumber string `json:"confirmationNumber,omitempty"`
}

func personRef(r *Registrant) PersonRef {
	return PersonRef{ID: r.ID, FullName: r.FullName, Email: r.Email, ConfirmationNumber: r.ConfirmationNumber}
}

// Answer is one event-question answer. Question is the question text; it is
// empty when the event's questions were not available (RegistrantsFrom).
type Answer struct {
	QuestionID string `json:"questionId"`
	Question   string `json:"question,omitempty"`
	Value      string `json:"value"` // multiple values joined with "; "
}

// InvoiceAddress is the address to invoice: the card billing address when
// known, else the contact's address. Nil when neither is set.
func (r *Registrant) InvoiceAddress() *Address {
	if !r.BillingAddress.IsZero() {
		return r.BillingAddress
	}
	return r.Address
}

// RegistrantsOptions tunes RegistrantsFrom / FetchRegistrants.
type RegistrantsOptions struct {
	// RefQuestionID pins the event question whose answer is the Ref #.
	// Empty = auto-detect by question text (see DefaultRefQuestionPattern).
	RefQuestionID string
	// WithPayments also fetches the event's orders, order items and
	// transactions and fills Registrant.Payment (needs the
	// orders/transactions read scopes).
	WithPayments bool
	// WithBillingAddress fills Registrant.BillingAddress. Billing addresses
	// are only on the transaction detail, so this costs one GET
	// /transactions/{id} per successful charge.
	WithBillingAddress bool
}

// Custom field names matched (case-insensitively) for the dedicated
// Registrant fields. The emergency-contact name pattern only matches the
// whole field name so it does not also catch the phone field.
var (
	EmergencyContactNamePattern  = regexp.MustCompile(`(?i)^\s*emergency contact( name)?\s*$`)
	EmergencyContactPhonePattern = regexp.MustCompile(`(?i)^\s*emergency contact\s*(#|no\.?|number|phone)`)
	VisaLetterPattern            = regexp.MustCompile(`(?i)\bvisa\b.*\bletter\b`)
)

// DefaultRefQuestionPattern matches the text of the question asking for the
// Indico registration reference number, e.g. "Please enter your Indico
// registration reference number (Ref #). …".
var DefaultRefQuestionPattern = regexp.MustCompile(`(?i)\bindico\b.*\bref(erence)?\b`)

// RefQuestionID returns the id of the Ref # question among the event's
// questions (raw /event-questions items), or "" when there is none. When
// several match, the first in list order wins.
func RefQuestionID(eventQuestions []json.RawMessage) string {
	for _, raw := range eventQuestions {
		var q struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &q) == nil && q.ID != "" && DefaultRefQuestionPattern.MatchString(q.Text) {
			return q.ID
		}
	}
	return ""
}

// RegistrantsFrom converts raw attendee items into Registrants, reading the
// Ref # from the answer to the question refQuestionID ("" = no Ref #).
// Guests are linked to their primary attendee (Registrant.Guests). Answer
// question texts are left empty; LabelAnswers fills them.
func RegistrantsFrom(attendees []json.RawMessage, refQuestionID string) []Registrant {
	type customField struct {
		Name  string   `json:"name"`
		Value []string `json:"value"`
	}
	type lookup struct {
		Name string `json:"name"`
		Code string `json:"code"`
	}
	// Name parts outside contact (seen in some payloads; contact wins).
	type nameParts struct {
		FirstName  string `json:"firstName"`
		GivenName  string `json:"givenName"`
		First      string `json:"first"`
		MiddleName string `json:"middleName"`
		Middle     string `json:"middle"`
		LastName   string `json:"lastName"`
		FamilyName string `json:"familyName"`
		Last       string `json:"last"`
	}
	out := make([]Registrant, 0, len(attendees))
	for _, raw := range attendees {
		var a struct {
			ID   string    `json:"id"`
			Name nameParts `json:"name"`
			nameParts
			Email                   string `json:"email"`
			ConfirmationNumber      string `json:"confirmationNumber"`
			Status                  string `json:"status"`
			CheckedIn               bool   `json:"checkedIn"`
			CheckIn                 string `json:"checkIn"`
			Guest                   bool   `json:"guest"`
			PrimaryID               string `json:"primaryId"`
			RegisteredAt            string `json:"registeredAt"`
			RegistrationCancelledAt string `json:"registrationCancelledAt"`
			Note                    string `json:"note"`
			Contact                 struct {
				Prefix             string        `json:"prefix"`
				FirstName          string        `json:"firstName"`
				MiddleName         string        `json:"middleName"`
				LastName           string        `json:"lastName"`
				Nickname           string        `json:"nickname"`
				Designation        string        `json:"designation"`
				Pronouns           string        `json:"pronouns"`
				Pronoun            string        `json:"pronoun"` // deprecated spelling
				Email              string        `json:"email"`
				CCEmail            string        `json:"ccEmail"`
				Company            string        `json:"company"`
				Title              string        `json:"title"`
				MobilePhone        string        `json:"mobilePhone"`
				WorkPhone          string        `json:"workPhone"`
				HomePhone          string        `json:"homePhone"`
				PersonalWebsite    string        `json:"personalWebsite"`
				Type               lookup        `json:"type"`
				PrimaryAddressType string        `json:"primaryAddressType"`
				WorkAddress        *Address      `json:"workAddress"`
				HomeAddress        *Address      `json:"homeAddress"`
				CustomFields       []customField `json:"customFields"`
			} `json:"contact"`
			// questions repeats the contact custom fields on the attendee.
			Questions        []customField `json:"questions"`
			RegistrationType lookup        `json:"registrationType"`
			AdmissionItem    lookup        `json:"admissionItem"`
			RegistrationPath lookup        `json:"registrationPath"`
			Answers          []struct {
				Question struct {
					ID string `json:"id"`
				} `json:"question"`
				Value []string `json:"value"`
			} `json:"answers"`
		}
		if json.Unmarshal(raw, &a) != nil {
			continue
		}
		c := a.Contact
		t := strings.TrimSpace
		r := Registrant{
			ID:                 a.ID,
			Prefix:             t(c.Prefix),
			FirstName:          t(c.FirstName),
			MiddleName:         t(c.MiddleName),
			LastName:           t(c.LastName),
			Nickname:           t(c.Nickname),
			Designation:        t(c.Designation),
			Pronouns:           t(firstNonEmpty(c.Pronouns, c.Pronoun)),
			Email:              t(c.Email),
			CCEmail:            t(c.CCEmail),
			Company:            t(c.Company),
			Title:              t(c.Title),
			MobilePhone:        t(c.MobilePhone),
			WorkPhone:          t(c.WorkPhone),
			HomePhone:          t(c.HomePhone),
			Website:            t(c.PersonalWebsite),
			ContactType:        firstNonEmpty(c.Type.Name, c.Type.Code),
			Status:             a.Status,
			RegistrationType:   firstNonEmpty(a.RegistrationType.Name, a.RegistrationType.Code),
			AdmissionItem:      firstNonEmpty(a.AdmissionItem.Name, a.AdmissionItem.Code),
			RegistrationPath:   firstNonEmpty(a.RegistrationPath.Name, a.RegistrationPath.Code),
			RegisteredAt:       a.RegisteredAt,
			CancelledAt:        a.RegistrationCancelledAt,
			Note:               t(a.Note),
			ConfirmationNumber: a.ConfirmationNumber,
			CheckedIn:          a.CheckedIn || a.CheckIn != "",
			CheckedInAt:        a.CheckIn,
			Guest:              a.Guest,
			PrimaryID:          a.PrimaryID,
		}
		if r.FirstName == "" && r.MiddleName == "" && r.LastName == "" {
			for _, n := range []nameParts{a.Name, a.nameParts} {
				first := t(firstNonEmpty(n.FirstName, n.GivenName, n.First))
				middle := t(firstNonEmpty(n.MiddleName, n.Middle))
				last := t(firstNonEmpty(n.LastName, n.FamilyName, n.Last))
				if first != "" || middle != "" || last != "" {
					r.FirstName, r.MiddleName, r.LastName = first, middle, last
					break
				}
			}
		}
		if r.Email == "" {
			r.Email = t(a.Email)
		}
		r.FullName = strings.Join(nonEmpty(r.FirstName, r.MiddleName, r.LastName), " ")

		primary, other := "Work", "Home"
		addrs := map[string]*Address{"Work": c.WorkAddress, "Home": c.HomeAddress}
		if strings.EqualFold(c.PrimaryAddressType, "Home") {
			primary, other = other, primary
		}
		if addrs[primary].IsZero() { // only the other kind was entered
			primary, other = other, primary
		}
		if !addrs[primary].IsZero() {
			r.Address, r.AddressType = addrs[primary], primary
			if !addrs[other].IsZero() {
				r.OtherAddress = addrs[other]
			}
		}

		for _, f := range append(c.CustomFields, a.Questions...) {
			v := strings.TrimSpace(strings.Join(f.Value, "; "))
			if f.Name == "" || v == "" {
				continue
			}
			if _, dup := r.CustomFields[f.Name]; dup {
				continue
			}
			if r.CustomFields == nil {
				r.CustomFields = map[string]string{}
			}
			r.CustomFields[f.Name] = v
			switch {
			case EmergencyContactNamePattern.MatchString(f.Name):
				r.EmergencyContactName = v
			case EmergencyContactPhonePattern.MatchString(f.Name):
				r.EmergencyContactPhone = v
			case VisaLetterPattern.MatchString(f.Name):
				r.VisaLetter = v
			}
		}

		for _, ans := range a.Answers {
			v := strings.TrimSpace(strings.Join(ans.Value, "; "))
			if refQuestionID != "" && ans.Question.ID == refQuestionID && r.Ref == "" {
				r.Ref = strings.TrimSpace(strings.Join(ans.Value, " "))
			}
			if v != "" {
				r.Answers = append(r.Answers, Answer{QuestionID: ans.Question.ID, Value: v})
			}
		}
		out = append(out, r)
	}

	byID := make(map[string]int, len(out))
	for i := range out {
		byID[out[i].ID] = i
	}
	for gi := range out {
		g := &out[gi]
		if !g.Guest || g.PrimaryID == "" {
			continue
		}
		if i, ok := byID[g.PrimaryID]; ok {
			p := personRef(&out[i])
			g.RegisteredBy = &p
			out[i].Guests = append(out[i].Guests, personRef(g))
		}
	}
	return out
}

// LabelAnswers fills Answer.Question from the event's questions (raw
// /event-questions items).
func LabelAnswers(regs []Registrant, eventQuestions []json.RawMessage) {
	text := make(map[string]string, len(eventQuestions))
	for _, raw := range eventQuestions {
		var q struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &q) == nil && q.ID != "" {
			text[q.ID] = strings.TrimSpace(q.Text)
		}
	}
	for i := range regs {
		for j := range regs[i].Answers {
			regs[i].Answers[j].Question = text[regs[i].Answers[j].QuestionID]
		}
	}
}

func nonEmpty(ss ...string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// RegistrantSource is the raw Cvent data (list items as returned by the
// API) that one event's Registrants are built from. Nil resources are
// fine: without Orders no Payment is set, without BillingAddresses no
// BillingAddress.
type RegistrantSource struct {
	Event            json.RawMessage // the event object (for its currency)
	Attendees        []json.RawMessage
	EventQuestions   []json.RawMessage
	Orders           []json.RawMessage
	OrderItems       []json.RawMessage
	Transactions     []json.RawMessage
	BillingAddresses map[string]*Address // attendee id → address (Client.BillingAddresses)
}

// BuildRegistrants turns src into Registrants: contact and form fields,
// guest links, labelled answers, the Ref # (from refQuestionID, else the
// auto-detected question), payments with their order lines, and billing
// addresses. FetchRegistrants and RegistrantsFromBundle both build with it.
func BuildRegistrants(src RegistrantSource, refQuestionID string) []Registrant {
	if refQuestionID == "" {
		refQuestionID = RefQuestionID(src.EventQuestions)
	}
	regs := RegistrantsFrom(src.Attendees, refQuestionID)
	LabelAnswers(regs, src.EventQuestions)
	pay := PaymentsByAttendee(src.Orders, src.Transactions)
	items := OrderItemsByAttendee(src.Orders, src.OrderItems)
	// Orders carry no currency; attendees without a transaction (e.g. a
	// fully discounted order) get the event's.
	var ev struct {
		Currency string `json:"currency"`
	}
	_ = json.Unmarshal(src.Event, &ev)
	for i := range regs {
		if p := pay[regs[i].ID]; p != nil {
			if p.Currency == "" {
				p.Currency = ev.Currency
			}
			p.Items = items[regs[i].ID]
			regs[i].Payment = p
		}
		regs[i].BillingAddress = src.BillingAddresses[regs[i].ID]
	}
	return regs
}

// RegistrantsFromBundle builds the Registrants of a cached event bundle
// (EventCache.Bundle, or a dump), payments and billing addresses included.
// Resources that failed to fetch are treated as empty.
func RegistrantsFromBundle(b *EventBundle, refQuestionID string) []Registrant {
	if b == nil {
		return nil
	}
	items := func(raw json.RawMessage) []json.RawMessage {
		var out []json.RawMessage
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &out) // unparseable → no rows
		}
		return out
	}
	return BuildRegistrants(RegistrantSource{
		Event:            b.Event,
		Attendees:        items(b.Attendees),
		EventQuestions:   items(b.EventQuestions),
		Orders:           items(b.Orders),
		OrderItems:       items(b.OrderItems),
		Transactions:     items(b.Transactions),
		BillingAddresses: b.BillingAddresses,
	}, refQuestionID)
}

// FetchRegistrants resolves the event (short code or uuid) and returns its
// attendees as Registrants. It fetches only attendees and event questions
// (plus orders, order items and transactions with WithPayments, and the
// transaction details with WithBillingAddress), not the full bundle.
func FetchRegistrants(ctx context.Context, c *Client, code string, opts RegistrantsOptions) ([]Registrant, error) {
	eventRaw, ev, err := c.resolveEvent(ctx, code)
	if err != nil {
		return nil, err
	}
	filter := "event.id eq '" + ev.ID + "'"

	var attendees, questions, orders, orderItems, transactions []json.RawMessage
	var aErr, qErr, oErr, iErr, tErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		attendees, aErr = c.filterAll(ctx, "/attendees/filter", filter)
	})
	// Always fetched: the answers are labelled with the question texts.
	wg.Go(func() {
		questions, qErr = c.listAll(ctx, "/event-questions", url.Values{"filter": {filter}})
	})
	if opts.WithPayments {
		wg.Go(func() {
			orders, oErr = c.listAll(ctx, "/events/"+ev.ID+"/orders", nil)
		})
		wg.Go(func() {
			orderItems, iErr = c.listAll(ctx, "/events/"+ev.ID+"/orders/items", nil)
		})
	}
	if opts.WithPayments || opts.WithBillingAddress {
		wg.Go(func() {
			transactions, tErr = c.listAll(ctx, "/events/"+ev.ID+"/transactions", nil)
		})
	}
	wg.Wait()
	if aErr != nil {
		return nil, fmt.Errorf("attendees: %w", aErr)
	}
	if qErr != nil {
		return nil, fmt.Errorf("event questions: %w", qErr)
	}
	if oErr != nil {
		return nil, fmt.Errorf("orders: %w", oErr)
	}
	if iErr != nil {
		return nil, fmt.Errorf("order items: %w", iErr)
	}
	if tErr != nil {
		return nil, fmt.Errorf("transactions: %w", tErr)
	}

	src := RegistrantSource{
		Event:          eventRaw,
		Attendees:      attendees,
		EventQuestions: questions,
	}
	if opts.WithPayments {
		src.Orders, src.OrderItems, src.Transactions = orders, orderItems, transactions
	}
	if opts.WithBillingAddress {
		if src.BillingAddresses, err = c.BillingAddresses(ctx, transactions); err != nil {
			return nil, fmt.Errorf("billing addresses: %w", err)
		}
	}
	regs := BuildRegistrants(src, opts.RefQuestionID)
	return regs, nil
}
