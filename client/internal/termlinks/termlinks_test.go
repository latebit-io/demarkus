package termlinks

import (
	"reflect"
	"strings"
	"testing"
)

// osc8 wraps text in an OSC 8 hyperlink, as Glamour v2 renders a link part.
func osc8(url, text string) string {
	return "\x1b]8;id=1;" + url + "\x07" + text + "\x1b]8;;\x07"
}

// rendered is a link as Glamour prints it: the label, a space, the URL.
func renderedLink(url, text string) string { return osc8(url, text) + " " + osc8(url, url) }

func none(int) bool { return false }

func only(index int) func(int) bool { return func(i int) bool { return i == index } }

// visible is the text a terminal shows: escapes dropped.
func visible(s string) string {
	var b strings.Builder
	scan([]rune(s), func(_ int, r rune, shown bool) {
		if shown {
			b.WriteRune(r)
		}
	})
	return b.String()
}

func TestLocate(t *testing.T) {
	tests := []struct {
		name     string
		rendered string
		links    []Link
		want     []string // the visible text of each span, in rendered order
	}{
		{"no links", "Just text.", nil, nil},
		{"label and its URL", "see " + renderedLink("url.md", "hello") + " rest", []Link{{"hello", "url.md"}}, []string{"hello url.md"}},
		{"label alone when no URL follows", "see " + osc8("url.md", "hello") + " rest", []Link{{"hello", "url.md"}}, []string{"hello"}},
		{"two links", renderedLink("a.md", "first") + " and " + renderedLink("b.md", "second"), []Link{{"first", "a.md"}, {"second", "b.md"}}, []string{"first a.md", "second b.md"}},
		{"plain text with the label's word is not the link", "hello then " + osc8("url.md", "hello"), []Link{{"hello", "url.md"}}, []string{"hello"}},
		{"a link with no label has no span", osc8("a.md", "first"), []Link{{"", "skip.md"}, {"first", "a.md"}}, []string{"first"}},
		{"a label wrapped over two lines", "see " + osc8("url.md", "a long\n  label") + " " + osc8("url.md", "url.md"), []Link{{"a long label", "url.md"}}, []string{"a long\n  label url.md"}},
		{"a label the renderer reopens on each wrapped line", "see " + osc8("url.md", "a long") + " \n  " + osc8("url.md", "label") + " " + osc8("url.md", "url.md") + " done", []Link{{"a long label", "url.md"}}, []string{"a long \n  label url.md"}},
		{"a label styled in parts", osc8("url.md", "bold") + osc8("url.md", " tail") + " " + osc8("url.md", "url.md"), []Link{{"bold tail", "url.md"}}, []string{"bold tail url.md"}},
		{"two links to one destination", renderedLink("x.md", "a") + " " + renderedLink("x.md", "b"), []Link{{"a", "x.md"}, {"b", "x.md"}}, []string{"a x.md", "b x.md"}},
		{"the renderer keeps the fragment", osc8("doc.md#sec", "sec"), []Link{{"sec", "doc.md"}}, []string{"sec"}},
		{"ST-terminated hyperlink", "\x1b]8;;url.md\x1b\\hello\x1b]8;;\x1b\\", []Link{{"hello", "url.md"}}, []string{"hello"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runes := []rune(tt.rendered)
			var got []string
			for _, s := range Locate(tt.rendered, tt.links) {
				got = append(got, visible(string(runes[s.Start:s.End])))
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("spans = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLocateKeepsTheLinkIndex(t *testing.T) {
	rendered := osc8("b.md", "second") + " " + osc8("a.md", "first")
	spans := Locate(rendered, []Link{{"first", "a.md"}, {"second", "b.md"}})
	if len(spans) != 2 || spans[0].Index != 1 || spans[1].Index != 0 {
		t.Errorf("spans = %+v, want rendered order with source indexes 1 then 0", spans)
	}
}

func TestPaint(t *testing.T) {
	rendered := "see " + renderedLink("/p.md", "hello") + " rest"
	spans := Locate(rendered, []Link{{"hello", "/p.md"}})

	plain, regions := Paint(rendered, spans, none)
	if plain != rendered {
		t.Errorf("unlit output changed: %q", plain)
	}
	// "see " is 4 cells; "hello /p.md" is 11.
	if want := []Region{{Index: 0, Line: 0, StartCol: 4, EndCol: 15}}; !reflect.DeepEqual(regions, want) {
		t.Errorf("regions = %+v, want %+v", regions, want)
	}

	lit, _ := Paint(rendered, spans, only(0))
	if visible(lit) != "see hello /p.md rest" {
		t.Errorf("highlight changed the visible text: %q", visible(lit))
	}
	on, off := strings.Index(lit, reverseOn), strings.LastIndex(lit, reverseOff)
	if on < 0 || off < on || !strings.HasPrefix(lit[on+len(reverseOn):], "hello") || !strings.HasSuffix(lit[:off], "/p.md") {
		t.Errorf("highlight does not wrap label through URL: %q", lit)
	}
}

func TestPaintLightsOnlyTheChosenLink(t *testing.T) {
	rendered := renderedLink("/a.md", "a") + " " + renderedLink("/b.md", "b") + " end"
	spans := Locate(rendered, []Link{{"a", "/a.md"}, {"b", "/b.md"}})
	lit, regions := Paint(rendered, spans, only(1))
	if len(regions) != 2 || strings.Count(lit, reverseOn) != 1 {
		t.Fatalf("regions = %+v, lit = %q", regions, lit)
	}
	if _, after, _ := strings.Cut(lit, reverseOn); !strings.HasPrefix(after, "b") {
		t.Errorf("the second link is not the one lit: %q", lit)
	}
}

// Hit-testing is in cells: a wide rune before or inside a link counts two.
func TestPaintCountsCells(t *testing.T) {
	rendered := "你好" + renderedLink("/文.md", "link") + " end"
	_, regions := Paint(rendered, Locate(rendered, []Link{{"link", "/文.md"}}), none)
	// "link" (4) + " " (1) + "/文.md" (6) from cell 4.
	if want := []Region{{Index: 0, Line: 0, StartCol: 4, EndCol: 15}}; !reflect.DeepEqual(regions, want) {
		t.Errorf("regions = %+v, want %+v", regions, want)
	}
}

func TestPaintSkipsStyleSequences(t *testing.T) {
	rendered := "line0\n\x1b[1mpre \x1b[0m" + osc8("/u.md", "\x1b[4mhi\x1b[0m") + " tail"
	_, regions := Paint(rendered, Locate(rendered, []Link{{"hi", "/u.md"}}), none)
	if want := []Region{{Index: 0, Line: 1, StartCol: 4, EndCol: 6}}; !reflect.DeepEqual(regions, want) {
		t.Errorf("regions = %+v, want %+v", regions, want)
	}
}

// A wrapped link is clickable on every line it occupies, and the indent the
// wrap adds is neither clickable nor highlighted.
func TestPaintWrappedLinkHasARegionPerLine(t *testing.T) {
	rendered := "  see " + osc8("/u.md", "a long\n  label") + " after"
	spans := Locate(rendered, []Link{{"a long label", "/u.md"}})
	lit, regions := Paint(rendered, spans, only(0))
	want := []Region{{Index: 0, Line: 0, StartCol: 6, EndCol: 12}, {Index: 0, Line: 1, StartCol: 2, EndCol: 7}}
	if !reflect.DeepEqual(regions, want) {
		t.Fatalf("regions = %+v, want %+v", regions, want)
	}
	if !strings.Contains(lit, reverseOn+"a long"+reverseOff+"\n  "+reverseOn+"label"+reverseOff) {
		t.Errorf("highlight = %q, want each line's text lit and the indent plain", lit)
	}
}

// A style reset inside a lit link would end the highlight; it is asserted again.
func TestPaintKeepsTheHighlightAcrossAStyleReset(t *testing.T) {
	rendered := osc8("/u.md", "\x1b[4mhi\x1b[0m") + " " + osc8("/u.md", "\x1b[34m/u.md\x1b[0m")
	lit, _ := Paint(rendered, Locate(rendered, []Link{{"hi", "/u.md"}}), only(0))
	if !strings.Contains(lit, "hi\x1b[0m"+reverseOn) {
		t.Errorf("highlight lost at the reset between label and URL: %q", lit)
	}
	if !strings.HasSuffix(strings.TrimSuffix(lit, "\x1b]8;;\x07"), "/u.md"+reverseOff+"\x1b[0m") {
		t.Errorf("highlight not closed after the URL: %q", lit)
	}
}

// Link boundaries are kept beside the text. Private-use runes a document
// carries (the in-band markers of an earlier design) are text like any other.
func TestDocumentRunesCannotForgeALink(t *testing.T) {
	forged := string(rune(0xF0000)) + "click here" + string(rune(0xF1000))
	rendered := forged + " then " + osc8("/real.md", "real")
	spans := Locate(rendered, []Link{{"real", "/real.md"}})
	lit, regions := Paint(rendered, spans, only(0))
	if len(regions) != 1 || regions[0].Index != 0 || regions[0].StartCol < len("click here then ") {
		t.Fatalf("regions = %+v, want the real link alone", regions)
	}
	if !strings.Contains(lit, forged) {
		t.Errorf("document runes were consumed: %q", lit)
	}
}
