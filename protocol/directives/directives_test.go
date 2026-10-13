package directives

import (
	"fmt"
	"testing"
)

func TestFirstKeepsTheFirstValuePerKey(t *testing.T) {
	body := "# Doc\n\nstrictness: warn\n  journal: log/ \nstrictness: block\nNote: prose with a colon\nno key here\n:empty\n\vvt: ignored\nspaced key : ignored\n"
	got := First(body)
	want := map[string]string{"strictness": "warn", "journal": "log/", "Note": "prose with a colon"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("First = %v, want %v", got, want)
	}
}
