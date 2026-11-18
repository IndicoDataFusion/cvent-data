package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IndicoDataFusion/cvent-data/internal/gsheets"
)

func TestKeyValuesFlag(t *testing.T) {
	var kv keyValues
	for _, s := range []string{"source=cron", "note=a=b", "empty="} {
		if err := kv.Set(s); err != nil {
			t.Fatalf("Set(%q): %v", s, err)
		}
	}
	if got := kv.String(); got != "source=cron,note=a=b,empty=" {
		t.Fatalf("String = %q", got)
	}
	for _, bad := range []string{"novalue", "=x", " =x"} {
		if err := kv.Set(bad); err == nil {
			t.Errorf("Set(%q): want an error", bad)
		}
	}
}

func TestDiffTables(t *testing.T) {
	prev := [][]string{
		{"id", "confirmationNumber", "fullName", "status", "Q: Diet"},
		{"1", "C1", "Ada Lovelace", "Accepted", "veg"},
		{"2", "C2", "Bob", "Accepted"}, // ragged: trailing cell dropped by the API
		{"3", "C3", "Cy", "Accepted", ""},
	}
	cur := [][]string{
		{"id", "confirmationNumber", "fullName", "status", "Field: Org"},
		{"1", "C1", "Ada Lovelace", "Cancelled", "MSU"},
		{"2", "C2", "Bob", "Accepted", ""},
		{"4", "", "", "Accepted", ""},
	}
	d := diffTables(prev, cur)
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	check("added", d.added, "4")
	check("removed", d.removed, "Cy (C3)")
	check("changed", d.changed, "Ada Lovelace (C1): status")
	check("colsAdded", d.colsAdded, "Field: Org")
	check("colsRemoved", d.colsRemoved, "Q: Diet")

	d = diffTables([][]string{}, cur)
	check("added from empty tab", d.added, "Ada Lovelace (C1)", "Bob (C2)", "4")
	check("colsAdded from empty tab", d.colsAdded)
}

func TestListCellCaps(t *testing.T) {
	if got := listCell(nil); got != "0" {
		t.Fatalf("empty = %q", got)
	}
	items := make([]string, maxListed+5)
	for i := range items {
		items[i] = "x"
	}
	got := listCell(items)
	if !strings.HasPrefix(got, "105: x; x") || !strings.HasSuffix(got, "; … and 5 more") {
		t.Fatalf("listCell = %q", got)
	}
}

func TestInfoRows(t *testing.T) {
	cur := [][]string{{"id", "fullName"}, {"1", "Ada"}, {"2", "Bob"}}
	rows := infoRows("2026-11-18T10:00:00Z", "cvent-sheets-export v1", keyValues{{"run", "nightly"}}, []eventInfo{
		{tab: "EV1", code: "EV1", title: "Conf", rows: 2, cur: cur, prev: [][]string{{"id", "fullName"}, {"1", "Ada"}}, prevSource: "sheet tab"},
		{tab: "EV2", code: "EV2", title: "Workshop", rows: 2, cur: cur},
		{tab: "EV3", code: "EV3", rows: 2, cur: cur, prev: cur, prevSource: "csv file"},
	})
	got := map[string]string{}
	var keys []string
	for _, r := range rows {
		got[r[0]] = r[1]
		keys = append(keys, r[0])
	}
	want := map[string]string{
		"key":               "value",
		"exported_at":       "2026-11-18T10:00:00Z",
		"exporter":          "cvent-sheets-export v1",
		"tables":            "3",
		"rows_total":        "6",
		"run":               "nightly",
		"EV1.event":         "EV1 Conf",
		"EV1.previous_rows": "1 (sheet tab)",
		"EV1.rows_delta":    "+1",
		"EV1.added":         "1: Bob (2)",
		"EV1.removed":       "0",
		"EV2.changes":       "no previous version",
		"EV3.event":         "EV3",
		"EV3.changes":       "none",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if keys[0] != "key" || keys[5] != "run" {
		t.Errorf("key order = %q", keys)
	}
}

func TestCSVRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "info.csv")
	if rows, err := readCSV(p); rows != nil || err != nil {
		t.Fatalf("missing file: %v %v", rows, err)
	}
	in := [][]string{{"key", "value"}, {"rows_delta", "+3"}, {"note", "a,\"b\""}}
	if err := writeCSV(p, in); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "\xEF\xBB\xBF") {
		t.Fatal("no BOM")
	}
	rows, err := readCSV(p)
	if err != nil {
		t.Fatal(err)
	}
	if rows[1][1] != "'+3" || rows[2][1] != `a,"b"` || rows[0][0] != "key" {
		t.Fatalf("rows = %q", rows)
	}
}

func TestHighlights(t *testing.T) {
	prev := [][]string{
		{"id", "status", "note"},
		{"1", "Accepted", "a"},
		{"9", "Accepted", ""},
	}
	cur := [][]string{
		{"id", "note", "status"}, // reordered columns: positions follow cur
		{"1", "b", "Cancelled"},
		{"2", "", "Accepted"},
		{"3", "", "Accepted"},
		{"9", "new", "Accepted"},
		{"4", "", "Accepted"},
	}
	d := diffTables(prev, cur)
	if fmt.Sprint(d.addedRows) != "[2 3 5]" {
		t.Fatalf("addedRows = %v", d.addedRows)
	}
	if fmt.Sprint(d.cells) != "[{1 1 a} {1 2 Accepted} {4 1 }]" {
		t.Fatalf("cells = %v", d.cells)
	}
	hs := highlights(d)
	want := []gsheets.Highlight{
		{Row: 2, Rows: 2, Color: addedColor},
		{Row: 5, Rows: 1, Color: addedColor},
		{Row: 1, Col: 1, Rows: 1, Cols: 1, Color: changedColor, Note: "Previous: a"},
		{Row: 1, Col: 2, Rows: 1, Cols: 1, Color: changedColor, Note: "Previous: Accepted"},
		{Row: 4, Col: 1, Rows: 1, Cols: 1, Color: changedColor, Note: "Previous: (empty)"},
	}
	if fmt.Sprint(hs) != fmt.Sprint(want) {
		t.Fatalf("highlights =\n%v\nwant\n%v", hs, want)
	}

	long := highlights(tableDiff{cells: []cellChange{{1, 0, strings.Repeat("é", maxNote+10)}}})
	if n := len([]rune(long[0].Note)); n != len("Previous: ")+maxNote+1 {
		t.Fatalf("long note has %d runes", n)
	}
}
