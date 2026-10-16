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
// Guests (companions) are registrants of their own with Guest set and
// PrimaryID naming the attendee who registered them; that attendee lists
// them in Guests. A guest's fee is an item on the primary's order
// (OrderItem.ForGuest), so a guest has no Payment of their own.
type Registrant struct {
	ID                 string `json:"id"`
	FirstName          string `json:"firstName"`
	MiddleName         string `json:"middleName,omitempty"`
	LastName           string `json:"lastName"`
	FullName           string `json:"fullName"`
	Email              string `json:"email,omitempty"`
	Company            string `json:"company,omitempty"`
	Title              string `json:"title,omitempty"` // job title
	MobilePhone        string `json:"mobilePhone,omitempty"`
	Ref                string `json:"ref,omitempty"`
	Status             string `json:"status,omitempty"`
	RegistrationType   string `json:"registrationType,omitempty"` // the member type, e.g. "IEEE NPSS Member"
	ConfirmationNumber string `json:"confirmationNumber,omitempty"`
	CheckedIn          bool   `json:"checkedIn"`
	// From the contact custom fields matched by name (see
	// EmergencyContactNamePattern and friends); the raw answers are in
	// CustomFields.
	EmergencyContactName  string `json:"emergencyContactName,omitempty"`
	EmergencyContactPhone string `json:"emergencyContactPhone,omitempty"`
	VisaLetter            string `json:"visaLetter,omitempty"` // answer as given: "Yes", "No" or empty
	// Address is the contact's primary address (work or home, per
	// primaryAddressType); nil when none was entered.
	Address   *Address   `json:"address,omitempty"`
	Guest     bool       `json:"guest,omitempty"`
	PrimaryID string     `json:"primaryId,omitempty"`
	Guests    []GuestRef `json:"guests,omitempty"`
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

// GuestRef names a guest registered by a primary attendee.
type GuestRef struct {
	ID       string `json:"id"`
	FullName string `json:"fullName"`
	Email    string `json:"email,omitempty"`
}

// Answer is one event-question answer. Question is the question text; it is
// empty when the event's questions were not available (RegistrantsFrom).
type Answer struct {
	QuestionID string `json:"questionId"`
	Question   string `json:"question,omitempty"`
	Value      string `json:"value"` // multiple values joined with "; "
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
	out := make([]Registrant, 0, len(attendees))
	for _, raw := range attendees {
		var a struct {
			ID                 string `json:"id"`
			ConfirmationNumber string `json:"confirmationNumber"`
			Status             string `json:"status"`
			CheckedIn          bool   `json:"checkedIn"`
			Guest              bool   `json:"guest"`
			PrimaryID          string `json:"primaryId"`
			Contact            struct {
				FirstName          string        `json:"firstName"`
				MiddleName         string        `json:"middleName"`
				LastName           string        `json:"lastName"`
				Email              string        `json:"email"`
				Company            string        `json:"company"`
				Title              string        `json:"title"`
				MobilePhone        string        `json:"mobilePhone"`
				PrimaryAddressType string        `json:"primaryAddressType"`
				WorkAddress        *Address      `json:"workAddress"`
				HomeAddress        *Address      `json:"homeAddress"`
				CustomFields       []customField `json:"customFields"`
			} `json:"contact"`
			// questions repeats the contact custom fields on the attendee.
			Questions        []customField `json:"questions"`
			RegistrationType struct {
				Name string `json:"name"`
			} `json:"registrationType"`
			Answers []struct {
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
		r := Registrant{
			ID:                 a.ID,
			FirstName:          strings.TrimSpace(c.FirstName),
			MiddleName:         strings.TrimSpace(c.MiddleName),
			LastName:           strings.TrimSpace(c.LastName),
			Email:              strings.TrimSpace(c.Email),
			Company:            strings.TrimSpace(c.Company),
			Title:              strings.TrimSpace(c.Title),
			MobilePhone:        strings.TrimSpace(c.MobilePhone),
			Status:             a.Status,
			RegistrationType:   a.RegistrationType.Name,
			ConfirmationNumber: a.ConfirmationNumber,
			CheckedIn:          a.CheckedIn,
			Guest:              a.Guest,
			PrimaryID:          a.PrimaryID,
		}
		r.FullName = strings.Join(nonEmpty(r.FirstName, r.MiddleName, r.LastName), " ")

		primary, other := c.WorkAddress, c.HomeAddress
		if strings.EqualFold(c.PrimaryAddressType, "Home") {
			primary, other = other, primary
		}
		if !primary.IsZero() {
			r.Address = primary
		} else if !other.IsZero() {
			r.Address = other
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
	for _, g := range out {
		if !g.Guest || g.PrimaryID == "" {
			continue
		}
		if i, ok := byID[g.PrimaryID]; ok {
			out[i].Guests = append(out[i].Guests, GuestRef{ID: g.ID, FullName: g.FullName, Email: g.Email})
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

// FetchRegistrants resolves the event (short code or uuid) and returns its
// attendees as Registrants. It fetches only attendees and event questions
// (plus orders, order items and transactions with WithPayments), not the
// full bundle.
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

	refID := opts.RefQuestionID
	if refID == "" {
		refID = RefQuestionID(questions)
	}
	regs := RegistrantsFrom(attendees, refID)
	LabelAnswers(regs, questions)
	if opts.WithPayments {
		pay := PaymentsByAttendee(orders, transactions)
		items := OrderItemsByAttendee(orders, orderItems)
		// Orders carry no currency; attendees without a transaction (e.g. a
		// fully discounted order) get the event's.
		var evCur struct {
			Currency string `json:"currency"`
		}
		_ = json.Unmarshal(eventRaw, &evCur)
		for i := range regs {
			p := pay[regs[i].ID]
			if p != nil && p.Currency == "" {
				p.Currency = evCur.Currency
			}
			if p != nil {
				p.Items = items[regs[i].ID]
			}
			regs[i].Payment = p
		}
	}
	return regs, nil
}
