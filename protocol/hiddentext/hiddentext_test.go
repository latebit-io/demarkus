package hiddentext

import (
	"strings"
	"testing"
)

func TestFindFlagsHiddenText(t *testing.T) {
	tests := []struct {
		name, body string
		want       string
	}{
		{"escape", "# Doc\n\nok \x1b]52;c;ZXZpbA==\a\n", "U+001B (control) at line 3, column 4; U+0007 (control) at line 3, column 19"},
		{"lone carriage return", "line\rover", "U+000D (control) at line 1, column 5"},
		{"del", "a\x7fb", "U+007F (control) at line 1, column 2"},
		{"c1", "a\u0085b", "U+0085 (control) at line 1, column 2"},
		{"bidi override", "x = \U0000202Eevil", "U+202E (bidi) at line 1, column 5"},
		{"bidi isolate", "\U00002066x\U00002069", "U+2066 (bidi) at line 1, column 1; U+2069 (bidi) at line 1, column 3"},
		{"line separator", "a\U00002028b", "U+2028 (line-separator) at line 1, column 2"},
		{"annotation", "a\U0000FFF9b", "U+FFF9 (annotation) at line 1, column 2"},
		{"deprecated format", "a\U0000206Ab\U0000206F", "U+206A (invisible) at line 1, column 2; U+206F (invisible) at line 1, column 4"},
		{"tag smuggling", "hi \U000E0049\U000E0047\U000E004E", "U+E0049 (tag, 3 characters) at line 1, column 4"},
		{"stray cancel tag", "a\U000E007F", "U+E007F (tag) at line 1, column 2"},
		{"variation selector run", "\U0001F600\U0000FE00\U0000FE01\U0000FE02", "U+FE00 (invisible, 3 characters) at line 1, column 2"},
		{"supplement selectors", "x\U000E0100\U000E0101", "U+E0100 (invisible, 2 characters) at line 1, column 2"},
		{"zero-width pair", "a\U0000200B\U0000200Cb", "U+200B (invisible, 2 characters) at line 1, column 2"},
		{"columns count runes", "\U000000E9\U0000202E", "U+202E (bidi) at line 1, column 2"},
		{"second line", "ok\n\tx\U0000202E", "U+202E (bidi) at line 2, column 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Summary(Find(tt.body)); got != tt.want {
				t.Errorf("Find = %q, want %q", got, tt.want)
			}
			if !Has(tt.body) {
				t.Error("Has = false")
			}
		})
	}
}

func TestFindAllowsLegitimateText(t *testing.T) {
	for name, body := range map[string]string{
		"crlf":            "a\r\nb\r\n",
		"tab":             "a\tb",
		"zwj emoji":       "\U0001F469\U0000200D\U0001F4BB",
		"vs16 zwj emoji":  "\U0001F469\U0000200D\U00002764\U0000FE0F\U0000200D\U0001F468",
		"keycap":          "#\U0000FE0F\U000020E3",
		"zwnj persian":    "\U00000645\U000006CC\U0000200C\U0000062E\U00000648\U00000627\U00000647\U00000645",
		"bom":             "\U0000FEFF# Doc",
		"subdivision":     "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F",
		"black flag only": "\U0001F3F4 flag",
		"rtl marks":       "\U000005D0\U0000200F\U000005D1\U0000200E",
		"cjk":             "\U000065E5\U0000672C\U00008A9E",
	} {
		if hits := Find(body); len(hits) != 0 {
			t.Errorf("%s: Find = %s, want none", name, Summary(hits))
		}
	}
}

func TestFindCapsHits(t *testing.T) {
	if got := len(Find(strings.Repeat("\x1b", 50))); got != maxHits {
		t.Errorf("hits = %d, want %d", got, maxHits)
	}
}

func TestRevealMakesHiddenTextVisible(t *testing.T) {
	got := Reveal("a\x1b[2Jb\U000E0049\U000E0047")
	if want := "a<U+001B>[2Jb<U+E0049><U+E0047>"; got != want {
		t.Errorf("Reveal = %q, want %q", got, want)
	}
	clean := "# Doc\n\n\U0001F469\U0000200D\U0001F4BB\r\n"
	if got := Reveal(clean); got != clean {
		t.Errorf("Reveal changed clean text: %q", got)
	}
}

func FuzzReveal(f *testing.F) {
	f.Add("a\x1b[0mb")
	f.Add("\U0001F3F4\U000E0067\U000E007F\U000E0041")
	f.Add("\U0000FE0F\U0000200D\U0000200D")
	f.Fuzz(func(t *testing.T, s string) {
		if hits := Find(Reveal(s)); len(hits) != 0 {
			t.Fatalf("Reveal(%q) still hides %s", s, Summary(hits))
		}
		if !Has(s) && Reveal(s) != s {
			t.Fatalf("Reveal changed text without hidden spans: %q", s)
		}
	})
}
