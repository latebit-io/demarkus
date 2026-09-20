package lookuptable

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Row is one parsed LOOKUP match, cells unescaped. Tags stay the wire's
// comma-separated text; Anchor and Snippet are body mode only.
type Row struct {
	Path       string
	Anchor     string
	Importance float64
	Title      string
	Tags       string
	Snippet    string
}

// Table is a parsed LOOKUP result; Body reports the five column form.
type Table struct {
	Rows []Row
	Body bool
}

// ParseTable reads the result table strictly: a missing header, a row of the
// wrong width, a bad importance or a relative path is an error, not a skip.
func ParseTable(body string) (Table, error) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	header, columns := -1, 0
	for i, line := range lines {
		cells, ok := SplitRow(line)
		if ok && IsHeader(cells) {
			header, columns = i, len(cells)
			break
		}
	}
	if header < 0 {
		return Table{}, fmt.Errorf("malformed LOOKUP response: result table missing")
	}

	table := Table{Body: columns == BodyColumns}
	for _, line := range lines[header+1:] {
		if strings.TrimSpace(line) == "" {
			if len(table.Rows) > 0 {
				break
			}
			continue
		}
		cells, ok := SplitRow(line)
		if !ok {
			break
		}
		if IsSeparator(cells) {
			continue
		}
		row, err := parseRow(cells, columns)
		if err != nil {
			return Table{}, err
		}
		table.Rows = append(table.Rows, row)
	}
	return table, nil
}

func parseRow(cells []string, columns int) (Row, error) {
	if len(cells) != columns {
		return Row{}, fmt.Errorf("malformed LOOKUP response: row has %d columns, header %d", len(cells), columns)
	}
	importance, err := strconv.ParseFloat(Unescape(cells[1]), 64)
	if err != nil || math.IsNaN(importance) || math.IsInf(importance, 0) || importance < 0 || importance > 1 {
		return Row{}, fmt.Errorf("malformed LOOKUP response: invalid importance %q", cells[1])
	}
	docPath, anchor := SplitLocation(cells[0])
	if !strings.HasPrefix(docPath, "/") {
		return Row{}, fmt.Errorf("malformed LOOKUP response: invalid path %q", docPath)
	}
	row := Row{
		Path:       docPath,
		Anchor:     anchor,
		Importance: importance,
		Title:      Unescape(cells[2]),
		Tags:       Unescape(cells[3]),
	}
	if columns == BodyColumns {
		row.Snippet = Unescape(cells[4])
	}
	return row, nil
}
