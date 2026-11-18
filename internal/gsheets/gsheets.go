// Package gsheets writes tables to Google Sheets with a service-account
// key, using only the standard library: the OAuth2 JWT-bearer grant
// (RS256-signed assertion) and the Sheets API v4 REST endpoints.
package gsheets

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Scope is the OAuth2 scope for reading and writing spreadsheets.
const Scope = "https://www.googleapis.com/auth/spreadsheets"

// DefaultBaseURL is the Sheets API v4 spreadsheets collection.
const DefaultBaseURL = "https://sheets.googleapis.com/v4/spreadsheets"

// Client is a Sheets API client authenticated as a service account.
type Client struct {
	BaseURL string // Sheets API base; DefaultBaseURL unless overridden (tests)
	HTTP    *http.Client

	email    string // the service account; the sheet must be shared with it
	keyID    string
	key      *rsa.PrivateKey
	tokenURL string

	token  string
	expiry time.Time
}

// serviceAccountKey is the JSON key file Google Cloud issues for a service
// account (only the fields used here).
type serviceAccountKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
}

// NewFromKeyFile reads a service-account JSON key file. Errors name the
// file, never the key material.
func NewFromKeyFile(path string) (*Client, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gsheets: reading key file: %v", err)
	}
	return NewFromKeyJSON(b)
}

// NewFromKeyJSON parses a service-account JSON key.
func NewFromKeyJSON(b []byte) (*Client, error) {
	var k serviceAccountKey
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, errors.New("gsheets: key file is not valid JSON")
	}
	if k.Type != "service_account" || k.ClientEmail == "" || k.PrivateKey == "" {
		return nil, errors.New("gsheets: key file is not a service-account key (want type service_account with client_email and private_key)")
	}
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, errors.New("gsheets: key file private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if rk, err1 := x509.ParsePKCS1PrivateKey(block.Bytes); err1 == nil {
			parsed = rk
		} else {
			return nil, errors.New("gsheets: key file private_key is not an RSA key")
		}
	}
	rk, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("gsheets: key file private_key is not an RSA key")
	}
	if k.TokenURI == "" {
		k.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &Client{
		BaseURL:  DefaultBaseURL,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
		email:    k.ClientEmail,
		keyID:    k.PrivateKeyID,
		key:      rk,
		tokenURL: k.TokenURI,
	}, nil
}

// Email is the service account's address: share the spreadsheet with it as
// Editor.
func (c *Client) Email() string { return c.email }

// accessToken returns a cached token, or exchanges a fresh signed JWT
// assertion for one.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	if c.token != "" && time.Now().Before(c.expiry) {
		return c.token, nil
	}
	now := time.Now()
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": c.keyID})
	claims, _ := json.Marshal(map[string]any{
		"iss":   c.email,
		"scope": Scope,
		"aud":   c.tokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("gsheets: signing token request: %v", err)
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signing + "." + enc.EncodeToString(sig)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gsheets: token request: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &e)
		return "", fmt.Errorf("gsheets: token request: status %d: %s %s", res.StatusCode, e.Error, e.Description)
	}
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", errors.New("gsheets: token response has no access_token")
	}
	if t.ExpiresIn <= 0 {
		t.ExpiresIn = 3600
	}
	c.token = t.AccessToken
	c.expiry = now.Add(time.Duration(t.ExpiresIn)*time.Second - time.Minute)
	return c.token, nil
}

// do sends one Sheets API request with a JSON body (nil = none) and decodes
// a JSON response into out (nil = discard).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("gsheets: %s: %v", method, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = http.StatusText(res.StatusCode)
		}
		if res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusNotFound {
			msg += " (is the spreadsheet id right, and is the sheet shared with " + c.email + " as Editor?)"
		}
		return fmt.Errorf("gsheets: status %d: %s", res.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("gsheets: decoding response: %v", err)
		}
	}
	return nil
}

// tabIDs returns the spreadsheet's tab titles mapped to their sheet ids.
func (c *Client) tabIDs(ctx context.Context, base string) (map[string]int64, error) {
	var meta struct {
		Sheets []struct {
			Properties struct {
				SheetID int64  `json:"sheetId"`
				Title   string `json:"title"`
			} `json:"properties"`
		} `json:"sheets"`
	}
	if err := c.do(ctx, http.MethodGet, base, url.Values{"fields": {"sheets.properties(sheetId,title)"}}, nil, &meta); err != nil {
		return nil, err
	}
	ids := make(map[string]int64, len(meta.Sheets))
	for _, s := range meta.Sheets {
		ids[s.Properties.Title] = s.Properties.SheetID
	}
	return ids, nil
}

// quoteTab quotes a tab title for an A1 range.
func quoteTab(tab string) string { return "'" + strings.ReplaceAll(tab, "'", "''") + "'" }

// ReadTab returns the values of the tab named tab as formatted text, row
// by row (the API drops trailing empty cells and rows, so rows may be
// ragged). found is false, with no error, when the tab does not exist.
func (c *Client) ReadTab(ctx context.Context, spreadsheetID, tab string) (rows [][]string, found bool, err error) {
	base := "/" + url.PathEscape(spreadsheetID)
	ids, err := c.tabIDs(ctx, base)
	if err != nil {
		return nil, false, err
	}
	if _, ok := ids[tab]; !ok {
		return nil, false, nil
	}
	var res struct {
		Values [][]string `json:"values"`
	}
	if err := c.do(ctx, http.MethodGet, base+"/values/"+url.PathEscape(quoteTab(tab)),
		url.Values{"valueRenderOption": {"FORMATTED_VALUE"}, "majorDimension": {"ROWS"}}, nil, &res); err != nil {
		return nil, true, err
	}
	return res.Values, true, nil
}

// Color is an RGB background colour, each channel 0–1.
type Color struct{ Red, Green, Blue float64 }

// Highlight colours a block of cells: Rows rows from Row and Cols columns
// from Col (0-based, row 0 = the header; a Cols of 0 means to the end of
// the row). A non-empty Note is attached to every cell of the block.
type Highlight struct {
	Row, Col   int
	Rows, Cols int
	Color      Color
	Note       string
}

// ReplaceOptions are ReplaceTabWith's formatting extras.
type ReplaceOptions struct {
	// ResetHighlights clears every cell's background colour and note
	// before Highlights are applied, so the previous run's marks go.
	ResetHighlights bool
	Highlights      []Highlight
}

// ReplaceTab makes the tab named tab in the spreadsheet hold exactly rows:
// it adds the tab when missing, clears it, writes rows from A1 as plain
// values (valueInputOption RAW: nothing is parsed as a formula, number or
// date) and freezes the first row as the header.
func (c *Client) ReplaceTab(ctx context.Context, spreadsheetID, tab string, rows [][]string) error {
	return c.ReplaceTabWith(ctx, spreadsheetID, tab, rows, ReplaceOptions{})
}

// ReplaceTabWith is ReplaceTab, then applies opt in the same batchUpdate
// as the header freeze.
func (c *Client) ReplaceTabWith(ctx context.Context, spreadsheetID, tab string, rows [][]string, opt ReplaceOptions) error {
	base := "/" + url.PathEscape(spreadsheetID)

	ids, err := c.tabIDs(ctx, base)
	if err != nil {
		return err
	}
	sheetID, found := ids[tab]
	if !found {
		var added struct {
			Replies []struct {
				AddSheet struct {
					Properties struct {
						SheetID int64 `json:"sheetId"`
					} `json:"properties"`
				} `json:"addSheet"`
			} `json:"replies"`
		}
		req := map[string]any{"requests": []any{
			map[string]any{"addSheet": map[string]any{"properties": map[string]any{"title": tab}}},
		}}
		if err := c.do(ctx, http.MethodPost, base+":batchUpdate", nil, req, &added); err != nil {
			return err
		}
		if len(added.Replies) > 0 {
			sheetID = added.Replies[0].AddSheet.Properties.SheetID
		}
	}

	quoted := quoteTab(tab)
	if err := c.do(ctx, http.MethodPost, base+"/values/"+url.PathEscape(quoted)+":clear", nil, map[string]any{}, nil); err != nil {
		return err
	}
	if rows == nil {
		rows = [][]string{}
	}
	a1 := quoted + "!A1"
	if err := c.do(ctx, http.MethodPut, base+"/values/"+url.PathEscape(a1),
		url.Values{"valueInputOption": {"RAW"}},
		map[string]any{"range": a1, "majorDimension": "ROWS", "values": rows}, nil); err != nil {
		return err
	}
	requests := []any{
		map[string]any{"updateSheetProperties": map[string]any{
			"properties": map[string]any{"sheetId": sheetID, "gridProperties": map[string]any{"frozenRowCount": 1}},
			"fields":     "gridProperties.frozenRowCount",
		}},
	}
	if opt.ResetHighlights {
		requests = append(requests, map[string]any{"repeatCell": map[string]any{
			"range":  map[string]any{"sheetId": sheetID},
			"cell":   map[string]any{},
			"fields": "userEnteredFormat.backgroundColor,note",
		}})
	}
	for _, h := range opt.Highlights {
		rng := map[string]any{"sheetId": sheetID, "startRowIndex": h.Row, "endRowIndex": h.Row + max(h.Rows, 1), "startColumnIndex": h.Col}
		if h.Cols > 0 {
			rng["endColumnIndex"] = h.Col + h.Cols
		}
		cell := map[string]any{"userEnteredFormat": map[string]any{"backgroundColor": map[string]any{
			"red": h.Color.Red, "green": h.Color.Green, "blue": h.Color.Blue,
		}}}
		fields := "userEnteredFormat.backgroundColor"
		if h.Note != "" {
			cell["note"] = h.Note
			fields += ",note"
		}
		requests = append(requests, map[string]any{"repeatCell": map[string]any{"range": rng, "cell": cell, "fields": fields}})
	}
	return c.do(ctx, http.MethodPost, base+":batchUpdate", nil, map[string]any{"requests": requests}, nil)
}
