package cvent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Credentials holds the resolved Cvent client credentials and API base. It is
// the unit the server and CLI pass around; building a Client from it is
// NewWithCredentials.
type Credentials struct {
	ClientID     string
	ClientSecret string
	BaseURL      string
}

// FromEnvironment resolves the Cvent credentials: real environment variables
// win, then the .env at the repo root (walked up from the working directory).
// BaseURL defaults to DefaultBaseURL when unset. Errors never contain secret
// values — they name the variable only.
func FromEnvironment() (Credentials, error) { return FromEnvironmentFrom("") }

// FromEnvironmentFrom is FromEnvironment with the starting directory made
// explicit ("" = os.Getwd()), so tests can point it at a temp tree.
func FromEnvironmentFrom(start string) (Credentials, error) {
	fileVals, err := dotEnvFrom(start)
	if err != nil {
		return Credentials{}, err
	}

	cid := envOr("CVENT_CLIENT_ID", fileVals)
	sec := envOr("CVENT_CLIENT_SECRET", fileVals)
	base := envOr("CVENT_API_BASE", fileVals)
	if base == "" {
		base = DefaultBaseURL
	}
	if cid == "" {
		return Credentials{}, fmt.Errorf("cvent env: CVENT_CLIENT_ID is not set (set it in the environment or in a .env at the repo root)")
	}
	if sec == "" {
		return Credentials{}, fmt.Errorf("cvent env: CVENT_CLIENT_SECRET is not set (set it in the environment or in a .env at the repo root)")
	}
	return Credentials{ClientID: cid, ClientSecret: sec, BaseURL: base}, nil
}

// dotEnvFrom walks up from start ("" = os.Getwd()) to the first .env that
// defines CVENT_CLIENT_ID and returns its parsed values (empty when none).
func dotEnvFrom(start string) (map[string]string, error) {
	dir := start
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil, fmt.Errorf("cvent env: %v", err)
		}
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
			vals := ParseDotEnv(b)
			if _, ok := vals["CVENT_CLIENT_ID"]; ok {
				return vals, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached filesystem root
			return map[string]string{}, nil
		}
		dir = parent
	}
}

// EventCodes returns the configured event codes: every CVENT_CODE_<n> in
// ascending n (the real environment wins per key over the repo-root .env),
// else a single legacy CVENT_EVENT. Empty means nothing is configured.
func EventCodes() []string { return EventCodesFrom("") }

// EventCodesFrom is EventCodes with the starting directory made explicit.
func EventCodesFrom(start string) []string {
	fileVals, _ := dotEnvFrom(start)
	byIndex := map[int]string{}
	collect := func(k, v string) {
		n, err := strconv.Atoi(strings.TrimPrefix(k, "CVENT_CODE_"))
		if !strings.HasPrefix(k, "CVENT_CODE_") || err != nil || v == "" {
			return
		}
		byIndex[n] = v
	}
	for k, v := range fileVals {
		collect(k, v)
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			collect(k, v)
		}
	}
	idx := make([]int, 0, len(byIndex))
	for n := range byIndex {
		idx = append(idx, n)
	}
	sort.Ints(idx)
	codes := make([]string, 0, len(idx))
	for _, n := range idx {
		codes = append(codes, byIndex[n])
	}
	if len(codes) == 0 {
		if v := envOr("CVENT_EVENT", fileVals); v != "" {
			codes = append(codes, v)
		}
	}
	return codes
}

// envOr returns the real environment value when set and non-empty, else the
// .env-file value.
func envOr(key string, file map[string]string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return file[key]
}

// ParseDotEnv parses KEY=VALUE lines: blank lines and # comments are skipped,
// surrounding single/double quotes are stripped from values.
func ParseDotEnv(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" {
			continue
		}
		out[k] = v
	}
	return out
}
