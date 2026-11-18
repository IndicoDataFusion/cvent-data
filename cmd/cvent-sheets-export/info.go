package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/IndicoDataFusion/cvent-data/internal/gsheets"
)

// keyValues is a repeatable KEY=VALUE flag; order is kept.
type keyValues [][2]string

func (kv *keyValues) String() string {
	parts := make([]string, len(*kv))
	for i, p := range *kv {
		parts[i] = p[0] + "=" + p[1]
	}
	return strings.Join(parts, ",")
}

func (kv *keyValues) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if k = strings.TrimSpace(k); !ok || k == "" {
		return fmt.Errorf("want KEY=VALUE, got %q", s)
	}
	*kv = append(*kv, [2]string{k, v})
	return nil
}

// maxListed caps how many registrants one change cell names, keeping the
// cell well under the Sheets 50,000-character limit.
const maxListed = 100

// tableDiff is how a registrants table differs from its previous version.
// Rows are matched by the "id" column; cells are compared by column name,
// so added or reordered columns do not count as changed cells.
type tableDiff struct {
	added, removed []string // row labels
	changed        []string // row label + the changed columns
	colsAdded      []string
	colsRemoved    []string

	// Positions in the current table (row 0 = header), for highlighting.
	addedRows []int
	cells     []cellChange
}

// cellChange is a changed cell of the current table and its old value.
type cellChange struct {
	row, col int
	prev     string
}

// diffTables compares two tables (header row first). Rows read back from
// a sheet may be ragged: missing trailing cells are empty.
func diffTables(prev, cur [][]string) tableDiff {
	var d tableDiff
	if len(cur) == 0 {
		return d
	}
	if len(prev) == 0 { // an empty previous tab: every row is new
		prev = [][]string{cur[0]}
	}
	ph, ch := prev[0], cur[0]
	pIdx, cIdx := colIndex(ph), colIndex(ch)
	for _, name := range ch {
		if _, ok := pIdx[name]; !ok {
			d.colsAdded = append(d.colsAdded, name)
		}
	}
	for _, name := range ph {
		if _, ok := cIdx[name]; !ok {
			d.colsRemoved = append(d.colsRemoved, name)
		}
	}
	cell := func(row []string, idx map[string]int, col string) string {
		if i, ok := idx[col]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}
	// byID maps each id to its last row's index in rows (header = 0).
	byID := func(rows [][]string, idx map[string]int) (map[string]int, []string) {
		m := map[string]int{}
		var order []string
		for i := 1; i < len(rows); i++ {
			id := cell(rows[i], idx, "id")
			if id == "" {
				continue
			}
			if _, dup := m[id]; !dup {
				order = append(order, id)
			}
			m[id] = i
		}
		return m, order
	}
	pRows, pOrder := byID(prev, pIdx)
	cRows, cOrder := byID(cur, cIdx)

	// Columns present in both versions, in the current order.
	var shared []string
	for _, name := range ch {
		if _, ok := pIdx[name]; ok {
			shared = append(shared, name)
		}
	}
	for _, id := range cOrder {
		ci := cRows[id]
		cr := cur[ci]
		pi, ok := pRows[id]
		if !ok {
			d.added = append(d.added, rowLabel(cr, cIdx, id))
			d.addedRows = append(d.addedRows, ci)
			continue
		}
		pr := prev[pi]
		var cols []string
		for _, name := range shared {
			if old := cell(pr, pIdx, name); old != cell(cr, cIdx, name) {
				cols = append(cols, name)
				d.cells = append(d.cells, cellChange{ci, cIdx[name], old})
			}
		}
		if len(cols) > 0 {
			d.changed = append(d.changed, rowLabel(cr, cIdx, id)+": "+strings.Join(cols, ", "))
		}
	}
	for _, id := range pOrder {
		if _, ok := cRows[id]; !ok {
			d.removed = append(d.removed, rowLabel(prev[pRows[id]], pIdx, id))
		}
	}
	return d
}

func colIndex(header []string) map[string]int {
	m := make(map[string]int, len(header))
	for i, name := range header {
		if _, dup := m[name]; !dup {
			m[name] = i
		}
	}
	return m
}

// rowLabel names a registrant for the info tab: "Full Name (confirmation
// number)", falling back to the id.
func rowLabel(row []string, idx map[string]int, id string) string {
	get := func(col string) string {
		if i, ok := idx[col]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	ref := get("confirmationNumber")
	if ref == "" {
		ref = id
	}
	if name := get("fullName"); name != "" {
		return name + " (" + ref + ")"
	}
	return ref
}

// listCell is "N: a; b; …", capped at maxListed items.
func listCell(items []string) string {
	if len(items) == 0 {
		return "0"
	}
	shown := items
	more := ""
	if len(shown) > maxListed {
		shown = shown[:maxListed]
		more = fmt.Sprintf("; … and %d more", len(items)-maxListed)
	}
	return strconv.Itoa(len(items)) + ": " + strings.Join(shown, "; ") + more
}

// eventInfo is one exported table's entry on the info tab.
type eventInfo struct {
	tab, code, title string
	rows             int
	prev             [][]string // previous version, header first; nil = none
	prevSource       string     // where prev came from, e.g. "sheet tab"
	cur              [][]string
}

// infoRows builds the info tab: a key/value header, then the export
// metadata, the caller's pairs, and per table its row count and the
// changes since the previous version (keys prefixed "<tab>.").
func infoRows(exportedAt, exporter string, extra keyValues, events []eventInfo) [][]string {
	total := 0
	for _, e := range events {
		total += e.rows
	}
	rows := [][]string{
		{"key", "value"},
		{"exported_at", exportedAt},
		{"exporter", exporter},
		{"tables", strconv.Itoa(len(events))},
		{"rows_total", strconv.Itoa(total)},
	}
	for _, p := range extra {
		rows = append(rows, []string{p[0], p[1]})
	}
	for _, e := range events {
		k := func(s string) string { return e.tab + "." + s }
		rows = append(rows,
			[]string{k("event"), strings.TrimSpace(e.code + " " + e.title)},
			[]string{k("rows"), strconv.Itoa(e.rows)},
		)
		if e.prev == nil {
			rows = append(rows, []string{k("changes"), "no previous version"})
			continue
		}
		d := diffTables(e.prev, e.cur)
		prevRows := max(len(e.prev)-1, 0)
		rows = append(rows,
			[]string{k("previous_rows"), fmt.Sprintf("%d (%s)", prevRows, e.prevSource)},
			[]string{k("rows_delta"), signed(e.rows - prevRows)},
			[]string{k("added"), listCell(d.added)},
			[]string{k("removed"), listCell(d.removed)},
			[]string{k("changed"), listCell(d.changed)},
		)
		if len(d.colsAdded) > 0 {
			rows = append(rows, []string{k("columns_added"), listCell(d.colsAdded)})
		}
		if len(d.colsRemoved) > 0 {
			rows = append(rows, []string{k("columns_removed"), listCell(d.colsRemoved)})
		}
		if len(d.added)+len(d.removed)+len(d.changed)+len(d.colsAdded)+len(d.colsRemoved) == 0 {
			rows = append(rows, []string{k("changes"), "none"})
		}
	}
	return rows
}

func signed(n int) string {
	if n > 0 {
		return "+" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// Highlight colours: Google Sheets' "light green 3" and "light yellow 3".
var (
	addedColor   = gsheets.Color{Red: 0.851, Green: 0.918, Blue: 0.827}
	changedColor = gsheets.Color{Red: 1, Green: 0.949, Blue: 0.8}
)

// maxNote caps a cell note's previous value.
const maxNote = 1000

// highlights marks d on the current table: added rows green, changed
// cells yellow with the previous value as a note. Consecutive added rows
// share one range.
func highlights(d tableDiff) []gsheets.Highlight {
	var hs []gsheets.Highlight
	for _, r := range d.addedRows {
		if n := len(hs); n > 0 && hs[n-1].Note == "" && hs[n-1].Row+hs[n-1].Rows == r {
			hs[n-1].Rows++
			continue
		}
		hs = append(hs, gsheets.Highlight{Row: r, Rows: 1, Color: addedColor})
	}
	for _, c := range d.cells {
		prev := c.prev
		if prev == "" {
			prev = "(empty)"
		} else if r := []rune(prev); len(r) > maxNote {
			prev = string(r[:maxNote]) + "…"
		}
		hs = append(hs, gsheets.Highlight{Row: c.row, Col: c.col, Rows: 1, Cols: 1, Color: changedColor, Note: "Previous: " + prev})
	}
	return hs
}
