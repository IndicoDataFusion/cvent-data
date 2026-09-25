package cvent

import (
	"fmt"
	"os"
	"path/filepath"
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
	dir := start
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return Credentials{}, fmt.Errorf("cvent env: %v", err)
		}
	}
	fileVals := map[string]string{}
	// Walk up until a .env that actually defines CVENT_CLIENT_ID.
	for {
		b, rerr := os.ReadFile(filepath.Join(dir, ".env"))
		if rerr == nil {
			fileVals = ParseDotEnv(b)
			if _, ok := fileVals["CVENT_CLIENT_ID"]; ok {
				break
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached filesystem root
			break
		}
		dir = parent
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
