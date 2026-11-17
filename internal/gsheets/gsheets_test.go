package gsheets

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGoogle serves the token endpoint (checking the RS256 assertion) and
// the Sheets endpoints ReplaceTab calls, recording each Sheets request.
type fakeGoogle struct {
	srv  *httptest.Server
	pub  *rsa.PublicKey
	tabs []string // existing tab titles

	mu    sync.Mutex
	calls []string          // "METHOD path?query"
	put   map[string]any    // last values PUT body
	batch []json.RawMessage // batchUpdate bodies
}

func newFakeGoogle(t *testing.T, tabs ...string) (*fakeGoogle, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	f := &fakeGoogle{pub: &key.PublicKey, tabs: tabs}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve(t)))
	t.Cleanup(f.srv.Close)
	keyJSON, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"client_email":   "exporter@example.iam.gserviceaccount.com",
		"private_key_id": "kid1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"token_uri":      f.srv.URL + "/token",
	})
	return f, keyJSON
}

func (f *fakeGoogle) serve(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			parts := strings.Split(r.PostForm.Get("assertion"), ".")
			if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || len(parts) != 3 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
			sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
			if rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig) != nil {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"invalid_grant","error_description":"bad signature"}`))
				return
			}
			claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var c map[string]any
			_ = json.Unmarshal(claims, &c)
			if c["iss"] != "exporter@example.iam.gserviceaccount.com" || c["scope"] != Scope || c["aud"] != f.srv.URL+"/token" {
				t.Errorf("claims = %v", c)
			}
			w.Write([]byte(`{"access_token":"tok","expires_in":3600}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			call += "?" + r.URL.RawQuery
		}
		f.calls = append(f.calls, call)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sheets/SID":
			var sheets []map[string]any
			for i, title := range f.tabs {
				sheets = append(sheets, map[string]any{"properties": map[string]any{"sheetId": i + 10, "title": title}})
			}
			json.NewEncoder(w).Encode(map[string]any{"sheets": sheets})
		case r.URL.Path == "/sheets/SID:batchUpdate":
			f.batch = append(f.batch, body)
			w.Write([]byte(`{"replies":[{"addSheet":{"properties":{"sheetId":99}}}]}`))
		case strings.HasSuffix(r.URL.Path, ":clear"):
			w.Write([]byte(`{}`))
		case r.Method == http.MethodPut:
			_ = json.Unmarshal(body, &f.put)
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"Requested entity was not found."}}`))
		}
	}
}

func TestReplaceTabExisting(t *testing.T) {
	f, keyJSON := newFakeGoogle(t, "Sheet1", "Registrant's list")
	c, err := NewFromKeyJSON(keyJSON)
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = f.srv.URL + "/sheets"
	rows := [][]string{{"name", "phone"}, {"Ada", "+1 555 0100"}}
	if err := c.ReplaceTab(context.Background(), "SID", "Registrant's list", rows); err != nil {
		t.Fatalf("ReplaceTab: %v", err)
	}
	want := []string{
		"GET /sheets/SID?fields=sheets.properties%28sheetId%2Ctitle%29",
		"POST /sheets/SID/values/%27Registrant%27%27s%20list%27:clear",
		"PUT /sheets/SID/values/%27Registrant%27%27s%20list%27%21A1?valueInputOption=RAW",
		"POST /sheets/SID:batchUpdate",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	vals, _ := json.Marshal(f.put["values"])
	if string(vals) != `[["name","phone"],["Ada","+1 555 0100"]]` {
		t.Fatalf("values = %s", vals)
	}
	if !strings.Contains(string(f.batch[0]), `"sheetId":11`) || !strings.Contains(string(f.batch[0]), `"frozenRowCount":1`) {
		t.Fatalf("freeze request = %s", f.batch[0])
	}
}

func TestReplaceTabAddsMissingTab(t *testing.T) {
	f, keyJSON := newFakeGoogle(t, "Sheet1")
	c, _ := NewFromKeyJSON(keyJSON)
	c.BaseURL = f.srv.URL + "/sheets"
	if err := c.ReplaceTab(context.Background(), "SID", "Registrants", [][]string{{"a"}}); err != nil {
		t.Fatalf("ReplaceTab: %v", err)
	}
	if len(f.batch) != 2 || !strings.Contains(string(f.batch[0]), `"addSheet"`) || !strings.Contains(string(f.batch[1]), `"sheetId":99`) {
		t.Fatalf("batchUpdates = %s", f.batch)
	}
}

func TestReplaceTabNotShared(t *testing.T) {
	f, keyJSON := newFakeGoogle(t)
	c, _ := NewFromKeyJSON(keyJSON)
	c.BaseURL = f.srv.URL + "/sheets"
	err := c.ReplaceTab(context.Background(), "OTHER", "Registrants", nil)
	if err == nil || !strings.Contains(err.Error(), "shared with exporter@example.iam.gserviceaccount.com") {
		t.Fatalf("err = %v, want a sharing hint", err)
	}
}

func TestNewFromKeyJSONRejects(t *testing.T) {
	for _, in := range []string{`nope`, `{"type":"authorized_user"}`, `{"type":"service_account","client_email":"x","private_key":"not pem"}`} {
		if _, err := NewFromKeyJSON([]byte(in)); err == nil {
			t.Errorf("%s: want an error", in)
		}
	}
}
