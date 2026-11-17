// Command cvent-sheets-export exports Cvent events' registrants as CSV, one file per
// event (cvent.WriteRegistrantsCSV columns, payments and billing addresses
// included). By default it exports every configured event (CVENT_CODE_<n>)
// whose title matches --match, so an event can be picked by a word in its
// title without naming its code.
//
// Usage:
//
//	cvent-sheets-export [flags] [event-code-or-uuid ...]
//
// Event codes/uuids given as arguments are exported as-is (no title match).
// Credentials come from the environment or .env, as for cvent-dump.
//
// With --sheet-id (or --sheet-id-env), each event's table is also written to
// a tab of that Google Sheet, replacing the tab's contents. Authentication
// is a service-account JSON key: the file --sheets-key, else the file
// GOOGLE_SHEETS_KEY_FILE, else the key JSON itself in GOOGLE_SHEETS_KEY_JSON
// (for CI secrets), each from the environment or .env. The spreadsheet must
// be shared with the service account as Editor. Values are written as plain
// text (no formula escaping needed).
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/IndicoDataFusion/cvent-data/cvent"
	"github.com/IndicoDataFusion/cvent-data/internal/gsheets"
)

// version is the release tag, stamped by `make dist` (-X main.version=…).
var version = "dev"

func main() {
	var (
		showVersion = flag.Bool("version", false, "print the version and exit")
		dir         = flag.String("dir", "exports", "output directory; each event is written to <dir>/<code>.csv (\"\" = no CSV files)")
		match       = flag.String("match", "", "with no event arguments: export only configured events whose title matches this case-insensitive regexp (empty = all)")
		envFile     = flag.String("env-file", "", "explicit .env file for credentials (the real environment still wins for any key it sets)")
		apiBase     = flag.String("api-base", "", "Cvent API base URL (overrides CVENT_API_BASE)")
		timeout     = flag.Duration("timeout", 5*time.Minute, "overall time limit")

		sheetID    = flag.String("sheet-id", "", "also write each event to this Google Sheet (the id in its URL)")
		sheetIDEnv = flag.String("sheet-id-env", "", "read the Google Sheet id from this variable (environment, else .env) instead of --sheet-id")
		sheetTab   = flag.String("sheet-tab", "", "tab to replace (default: the event code); only when one event is exported")
		sheetsKey  = flag.String("sheets-key", "", "service-account JSON key file (default: GOOGLE_SHEETS_KEY_FILE, else the key JSON in GOOGLE_SHEETS_KEY_JSON, from the environment or .env)")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] [event-code-or-uuid ...]\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "Flags:")
		flag.PrintDefaults()
	}
	// Codes and flags may interleave: resume parsing after each positional.
	var events []string
	for args := os.Args[1:]; ; {
		if err := flag.CommandLine.Parse(args); err != nil {
			os.Exit(2)
		}
		if flag.NArg() == 0 {
			break
		}
		events = append(events, flag.Arg(0))
		args = flag.Args()[1:]
	}

	if *showVersion {
		fmt.Println("cvent-sheets-export " + version)
		return
	}

	var titleRe *regexp.Regexp
	if *match != "" {
		re, err := regexp.Compile("(?i)" + *match)
		if err != nil {
			fail("--match: " + err.Error())
		}
		titleRe = re
	}

	if *envFile != "" {
		b, err := os.ReadFile(*envFile)
		if err != nil {
			fail(fmt.Sprintf("reading --env-file %s: %v", *envFile, err))
		}
		for k, v := range cvent.ParseDotEnv(b) {
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}
	// Sheet settings are checked before any Cvent request, so a typo fails
	// fast.
	var sheets *gsheets.Client
	if *sheetIDEnv != "" {
		if *sheetID != "" {
			fail("give --sheet-id or --sheet-id-env, not both")
		}
		if *sheetID = cvent.Getenv(*sheetIDEnv); *sheetID == "" {
			fail(*sheetIDEnv + " is not set (set it in the environment or in .env)")
		}
	}
	if *sheetID != "" {
		keyFile := *sheetsKey
		if keyFile == "" {
			keyFile = cvent.Getenv("GOOGLE_SHEETS_KEY_FILE")
		}
		var c *gsheets.Client
		var err error
		switch {
		case keyFile != "":
			if strings.HasPrefix(keyFile, "~/") {
				if home, herr := os.UserHomeDir(); herr == nil {
					keyFile = filepath.Join(home, keyFile[2:])
				}
			}
			c, err = gsheets.NewFromKeyFile(keyFile)
		case cvent.Getenv("GOOGLE_SHEETS_KEY_JSON") != "":
			c, err = gsheets.NewFromKeyJSON([]byte(cvent.Getenv("GOOGLE_SHEETS_KEY_JSON")))
		default:
			fail("writing a Google Sheet needs a service-account key: set GOOGLE_SHEETS_KEY_FILE (a path) or GOOGLE_SHEETS_KEY_JSON (the key itself), or pass --sheets-key")
		}
		if err != nil {
			fail(err.Error())
		}
		sheets = c
	} else if *sheetTab != "" || *sheetsKey != "" {
		fail("--sheet-tab and --sheets-key need --sheet-id or --sheet-id-env")
	}

	creds, err := cvent.FromEnvironment()
	if err != nil {
		fail(err.Error())
	}
	if *apiBase != "" {
		creds.BaseURL = *apiBase
	}
	client := cvent.NewWithCredentials(creds)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	explicit := len(events) > 0
	if !explicit {
		events = cvent.EventCodes()
		if len(events) == 0 {
			fail("no events: pass event codes/uuids or set CVENT_CODE_1 (…) in .env")
		}
	}

	type table struct {
		code, title string
		rows        [][]string
	}
	var tables []table
	var skipped []string
	for _, code := range events {
		info, err := cvent.FetchEvent(ctx, client, code)
		if err != nil {
			fail(err.Error())
		}
		if !explicit && titleRe != nil && !titleRe.MatchString(info.Title) {
			skipped = append(skipped, fmt.Sprintf("%s %q", info.Code, info.Title))
			continue
		}
		regs, err := cvent.FetchRegistrants(ctx, client, info.ID, cvent.RegistrantsOptions{
			WithPayments:       true,
			WithBillingAddress: true,
		})
		if err != nil {
			fail(info.Code + ": " + err.Error())
		}
		header, rows := cvent.RegistrantsTable(regs)
		name := info.Code
		if name == "" {
			name = code
		}
		if *dir != "" {
			var buf bytes.Buffer
			if err := cvent.WriteRegistrantsCSV(&buf, regs); err != nil {
				fail(info.Code + ": " + err.Error())
			}
			if err := os.MkdirAll(*dir, 0o755); err != nil {
				fail(err.Error())
			}
			p := filepath.Join(*dir, name+".csv")
			if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
				fail(err.Error())
			}
			fmt.Printf("csv: %s %q: %d registrants → %s\n", info.Code, info.Title, len(regs), p)
		} else {
			fmt.Printf("fetched: %s %q: %d registrants\n", info.Code, info.Title, len(regs))
		}
		tables = append(tables, table{name, info.Title, append([][]string{header}, rows...)})
	}
	if len(tables) == 0 {
		fail(fmt.Sprintf("no configured event title matches --match %q (checked: %s)", *match, strings.Join(skipped, ", ")))
	}

	if sheets == nil {
		return
	}
	if *sheetTab != "" && len(tables) > 1 {
		fail(fmt.Sprintf("--sheet-tab %q: %d events matched, so each needs its own tab; drop --sheet-tab to name tabs by event code", *sheetTab, len(tables)))
	}
	for _, t := range tables {
		tab := *sheetTab
		if tab == "" {
			tab = t.code
		}
		if err := sheets.ReplaceTab(ctx, *sheetID, tab, t.rows); err != nil {
			fail(t.code + ": " + err.Error())
		}
		fmt.Printf("sheet: %s → tab %q (%d rows) of https://docs.google.com/spreadsheets/d/%s\n", t.code, tab, len(t.rows)-1, *sheetID)
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "cvent-sheets-export: "+msg)
	os.Exit(1)
}
