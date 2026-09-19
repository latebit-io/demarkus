package mdoutline

import "testing"

// Section spans must stay inside the body and keep document order.
func FuzzHeadings(f *testing.F) {
	f.Add("# A\n\ntext\n\n## B\n\nSetext\n===\n")
	f.Add("```\n# not a heading\n```\n")
	f.Fuzz(func(t *testing.T, body string) {
		prev := -1
		for _, h := range Headings(body) {
			if h.Level < 1 || h.Level > 6 {
				t.Fatalf("level %d", h.Level)
			}
			if h.Start < 0 || h.Start > h.End || h.End > len(body) {
				t.Fatalf("span [%d,%d) outside body of %d bytes", h.Start, h.End, len(body))
			}
			if h.Start <= prev {
				t.Fatalf("headings out of order: %d after %d", h.Start, prev)
			}
			prev = h.Start
		}
	})
}
