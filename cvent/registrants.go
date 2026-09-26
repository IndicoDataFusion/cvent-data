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
// needs to match it against its own registrations. Ref is the attendee's
// answer to the "Indico registration reference number (Ref #)" question,
// trimmed but otherwise as typed (it may be a non-numeric confirmation
// number, or empty).
type Registrant struct {
	ID                 string `json:"id"`
	FirstName          string `json:"firstName"`
	MiddleName         string `json:"middleName,omitempty"`
	LastName           string `json:"lastName"`
	FullName           string `json:"fullName"`
	Email              string `json:"email,omitempty"`
	Company            string `json:"company,omitempty"`
	Ref                string `json:"ref,omitempty"`
	Status             string `json:"status,omitempty"`
	RegistrationType   string `json:"registrationType,omitempty"`
	ConfirmationNumber string `json:"confirmationNumber,omitempty"`
	CheckedIn          bool   `json:"checkedIn"`
	// Payment is the attendee's order/transaction summary; nil when payments
	// were not requested or the attendee has no order.
	Payment *AttendeePayment `json:"payment,omitempty"`
}

// RegistrantsOptions tunes RegistrantsFrom / FetchRegistrants.
type RegistrantsOptions struct {
	// RefQuestionID pins the event question whose answer is the Ref #.
	// Empty = auto-detect by question text (see DefaultRefQuestionPattern).
	RefQuestionID string
	// WithPayments also fetches the event's orders and transactions and
	// fills Registrant.Payment (needs the orders/transactions read scopes).
	WithPayments bool
}

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
func RegistrantsFrom(attendees []json.RawMessage, refQuestionID string) []Registrant {
	out := make([]Registrant, 0, len(attendees))
	for _, raw := range attendees {
		var a struct {
			ID                 string `json:"id"`
			ConfirmationNumber string `json:"confirmationNumber"`
			Status             string `json:"status"`
			CheckedIn          bool   `json:"checkedIn"`
			Contact            struct {
				FirstName  string `json:"firstName"`
				MiddleName string `json:"middleName"`
				LastName   string `json:"lastName"`
				Email      string `json:"email"`
				Company    string `json:"company"`
			} `json:"contact"`
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
			Status:             a.Status,
			RegistrationType:   a.RegistrationType.Name,
			ConfirmationNumber: a.ConfirmationNumber,
			CheckedIn:          a.CheckedIn,
		}
		r.FullName = strings.Join(nonEmpty(r.FirstName, r.MiddleName, r.LastName), " ")
		if refQuestionID != "" {
			for _, ans := range a.Answers {
				if ans.Question.ID == refQuestionID {
					r.Ref = strings.TrimSpace(strings.Join(ans.Value, " "))
					break
				}
			}
		}
		out = append(out, r)
	}
	return out
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
// (plus orders and transactions with WithPayments), not the full bundle.
func FetchRegistrants(ctx context.Context, c *Client, code string, opts RegistrantsOptions) ([]Registrant, error) {
	eventRaw, ev, err := c.resolveEvent(ctx, code)
	if err != nil {
		return nil, err
	}
	filter := "event.id eq '" + ev.ID + "'"

	var attendees, questions, orders, transactions []json.RawMessage
	var aErr, qErr, oErr, tErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		attendees, aErr = c.filterAll(ctx, "/attendees/filter", filter)
	})
	if opts.RefQuestionID == "" {
		wg.Go(func() {
			questions, qErr = c.listAll(ctx, "/event-questions", url.Values{"filter": {filter}})
		})
	}
	if opts.WithPayments {
		wg.Go(func() {
			orders, oErr = c.listAll(ctx, "/events/"+ev.ID+"/orders", nil)
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
	if tErr != nil {
		return nil, fmt.Errorf("transactions: %w", tErr)
	}

	refID := opts.RefQuestionID
	if refID == "" {
		refID = RefQuestionID(questions)
	}
	regs := RegistrantsFrom(attendees, refID)
	if opts.WithPayments {
		pay := PaymentsByAttendee(orders, transactions)
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
			regs[i].Payment = p
		}
	}
	return regs, nil
}
