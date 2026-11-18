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
//
// With --info-tab (or any --info KEY=VALUE, which implies --info-tab info),
// a key/value summary is also written: to that tab of the sheet, and to
// <dir>/<info-tab>.csv when CSV files are written. It holds the export time,
// the row count per table and in total, the --info pairs, and per table the
// registrants added, removed and changed (with the changed columns) since
// the previous version: the tab being replaced, else the CSV file being
// overwritten.
//
// With --highlight, each event tab also marks the changes since the tab's
// previous contents: added registrant rows green, changed cells yellow with
// the previous value as a cell note. Earlier colours and notes in the tab
// are cleared first.
package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
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

		infoTab = flag.String("info-tab", "", "also write a key/value summary (export time, row counts, changes since the previous version) to this tab, and to <dir>/<info-tab>.csv (\"\" = none)")
		extra   keyValues

		highlight = flag.Bool("highlight", false, "in each event's sheet tab, colour rows added since the previous version green and changed cells yellow, with the previous value as a cell note (clears the tab's earlier colours and notes)")
	)
	flag.Var(&extra, "info", "extra KEY=VALUE row for the info tab (repeatable; implies --info-tab info)")
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
	if len(extra) > 0 && *infoTab == "" {
		*infoTab = "info"
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
	if *highlight && sheets == nil {
		fail("--highlight needs --sheet-id or --sheet-id-env")
	}
	if *infoTab != "" && sheets == nil && *dir == "" {
		fail("--info-tab and --info need --sheet-id, --sheet-id-env or --dir")
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
		rows        [][]string // header first, values as-is
		// For the info tab with no sheet: the CSV file this export
		// replaced (nil = none) and the new CSV, both parsed, so cells
		// compare with the same formula escaping.
		prevCSV, curCSV [][]string
	}
	exportedAt := time.Now().UTC().Format(time.RFC3339)
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
		t := table{code: name, title: info.Title, rows: append([][]string{header}, rows...)}
		if *dir != "" {
			p := filepath.Join(*dir, name+".csv")
			var buf bytes.Buffer
			if err := cvent.WriteRegistrantsCSV(&buf, regs); err != nil {
				fail(info.Code + ": " + err.Error())
			}
			if *infoTab != "" && sheets == nil {
				if t.prevCSV, err = readCSV(p); err != nil {
					fail(err.Error())
				}
				if t.curCSV, err = parseCSV(buf.Bytes()); err != nil {
					fail(info.Code + ": " + err.Error())
				}
			}
			if err := os.MkdirAll(*dir, 0o755); err != nil {
				fail(err.Error())
			}
			if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
				fail(err.Error())
			}
			fmt.Printf("csv: %s %q: %d registrants → %s\n", info.Code, info.Title, len(regs), p)
		} else {
			fmt.Printf("fetched: %s %q: %d registrants\n", info.Code, info.Title, len(regs))
		}
		tables = append(tables, t)
	}
	if len(tables) == 0 {
		fail(fmt.Sprintf("no configured event title matches --match %q (checked: %s)", *match, strings.Join(skipped, ", ")))
	}
	if *sheetTab != "" && len(tables) > 1 {
		fail(fmt.Sprintf("--sheet-tab %q: %d events matched, so each needs its own tab; drop --sheet-tab to name tabs by event code", *sheetTab, len(tables)))
	}
	tabOf := func(t table) string {
		if *sheetTab != "" {
			return *sheetTab
		}
		return t.code
	}
	if *infoTab != "" {
		for _, t := range tables {
			if tabOf(t) == *infoTab || (*dir != "" && t.code == *infoTab) {
				fail(fmt.Sprintf("--info-tab %q is also an event's tab or CSV name; pick another", *infoTab))
			}
		}
	}

	var infos []eventInfo
	for _, t := range tables {
		e := eventInfo{tab: tabOf(t), code: t.code, title: t.title, rows: len(t.rows) - 1, cur: t.rows}
		if sheets != nil {
			var opt gsheets.ReplaceOptions
			if *infoTab != "" || *highlight {
				prev, found, err := sheets.ReadTab(ctx, *sheetID, e.tab)
				if err != nil {
					fail(t.code + ": reading the previous tab: " + err.Error())
				}
				if found {
					e.prev, e.prevSource = prev, "sheet tab"
					if e.prev == nil {
						e.prev = [][]string{}
					}
				}
			}
			if *highlight {
				opt.ResetHighlights = true
				if e.prev != nil {
					opt.Highlights = highlights(diffTables(e.prev, t.rows))
				}
			}
			if err := sheets.ReplaceTabWith(ctx, *sheetID, e.tab, t.rows, opt); err != nil {
				fail(t.code + ": " + err.Error())
			}
			marked := ""
			if *highlight {
				marked = fmt.Sprintf(", %d highlights", len(opt.Highlights))
			}
			fmt.Printf("sheet: %s → tab %q (%d rows%s) of https://docs.google.com/spreadsheets/d/%s\n", t.code, e.tab, len(t.rows)-1, marked, *sheetID)
		} else if t.prevCSV != nil {
			e.prev, e.prevSource, e.cur = t.prevCSV, "csv file", t.curCSV
		}
		infos = append(infos, e)
	}

	if *infoTab == "" {
		return
	}
	infoTable := infoRows(exportedAt, "cvent-sheets-export "+version, extra, infos)
	if *highlight {
		legend := []string{"highlights", "event tabs: added rows green, changed cells yellow (note = previous value)"}
		infoTable = append(infoTable[:5], append([][]string{legend}, infoTable[5:]...)...)
	}
	if *dir != "" {
		p := filepath.Join(*dir, *infoTab+".csv")
		if err := writeCSV(p, infoTable); err != nil {
			fail(err.Error())
		}
		fmt.Printf("info: %s\n", p)
	}
	if sheets != nil {
		if err := sheets.ReplaceTab(ctx, *sheetID, *infoTab, infoTable); err != nil {
			fail("info tab: " + err.Error())
		}
		fmt.Printf("info: tab %q of https://docs.google.com/spreadsheets/d/%s\n", *infoTab, *sheetID)
	}
}

// readCSV parses a CSV file this tool wrote earlier; a missing file is nil
// with no error.
func readCSV(path string) ([][]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := parseCSV(b)
	if err != nil {
		return nil, fmt.Errorf("reading the previous %s: %v", path, err)
	}
	return rows, nil
}

// parseCSV parses CSV bytes, dropping a leading UTF-8 BOM. An empty file
// is an empty (non-nil) table.
func parseCSV(b []byte) ([][]string, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if rows == nil {
		rows = [][]string{}
	}
	return rows, err
}

// writeCSV writes rows as a UTF-8 CSV with a BOM, prefixing formula-like
// cells (e.g. "+3") with an apostrophe, as cvent.WriteRegistrantsCSV does.
func writeCSV(path string, rows [][]string) error {
	var buf bytes.Buffer
	buf.WriteString("\xEF\xBB\xBF")
	w := csv.NewWriter(&buf)
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, c := range row {
			if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
				c = "'" + c
			}
			safe[i] = c
		}
		w.Write(safe)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "cvent-sheets-export: "+msg)
	os.Exit(1)
}
