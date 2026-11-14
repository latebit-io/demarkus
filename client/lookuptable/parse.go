package lookuptable

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/latebit-io/demarkus/protocol/render"
)

// Row is one parsed LOOKUP match, cells unescaped. Tags and Pins stay the
// wire's comma-separated text; Anchor and Snippet are body mode only.
type Row struct {
	Path       string
	Anchor     string
	Importance float64
	Title      string
	Tags       string
	Snippet    string
	Pins       string
}

// Table is a parsed LOOKUP result and the optional columns it carried: a
// server that ignored pins: true sends no Pins.
type Table struct {
	Rows []Row
	render.Columns
}

// ParseTable reads the result table strictly: a missing header, a row of the
// wrong width, a bad importance or a relative path is an error, not a skip.
func ParseTable(body string) (Table, error) {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	header, columns, width := findHeader(lines)
	if width == 0 {
		return Table{}, fmt.Errorf("malformed LOOKUP response: result table missing")
	}

	table := Table{Columns: columns}
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
		row, err := parseRow(cells, columns, width)
		if err != nil {
			return Table{}, err
		}
		table.Rows = append(table.Rows, row)
	}
	return table, nil
}

func parseRow(cells []string, columns render.Columns, width int) (Row, error) {
	if len(cells) != width {
		return Row{}, fmt.Errorf("malformed LOOKUP response: row has %d columns, header %d", len(cells), width)
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
	if columns.Snippet {
		row.Snippet = Unescape(cells[catalogColumns])
	}
	if columns.Pins {
		row.Pins = Unescape(cells[len(cells)-1])
	}
	return row, nil
}
