package retrievalbench

import (
	"strings"
	"testing"
)

func TestDefaultQuestionSet(t *testing.T) {
	set, err := DefaultQuestionSet()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(set.Questions); n < 30 || n > 50 {
		t.Fatalf("fixture has %d questions, want 30..50", n)
	}
	counts := map[Category]int{}
	for _, q := range set.Questions {
		counts[q.Category]++
	}
	for _, cat := range []Category{CategoryVocabulary, CategoryBody, CategoryDecision} {
		if counts[cat] < 5 {
			t.Errorf("category %s has %d questions, want >= 5", cat, counts[cat])
		}
	}
}

func TestValidateRejects(t *testing.T) {
	base := `{"scope":"/","questions":[{"id":"a","category":"body","query":"x","question":"y","expected_path":"/a.md","expected_anchor":"sec"}]}`
	tests := []struct {
		name string
		json string
		want string
	}{
		{"duplicate id", strings.Replace(base, `}]}`, `},{"id":"a","category":"body","query":"x","question":"y","expected_path":"/b.md"}]}`, 1), "duplicate id"},
		{"unknown category", strings.Replace(base, `"body"`, `"vibes"`, 1), "unknown category"},
		{"anchor not slug", strings.Replace(base, `"sec"`, `"#Sec"`, 1), "slug form"},
		{"path with anchor", strings.Replace(base, `"/a.md"`, `"/a.md#sec"`, 1), "bare absolute path"},
		{"relative scope", strings.Replace(base, `"scope":"/"`, `"scope":"docs"`, 1), "start and end with /"},
		{"missing query", strings.Replace(base, `"query":"x"`, `"query":""`, 1), "missing query"},
		{"missing id", strings.Replace(base, `"id":"a"`, `"id":""`, 1), "missing id"},
		{"missing question", strings.Replace(base, `"question":"y"`, `"question":""`, 1), "missing question"},
		{"no questions", `{"scope":"/","questions":[]}`, "no questions"},
		{"unknown field", strings.Replace(base, `"expected_anchor"`, `"expected_ancher"`, 1), "unknown field"},
		{"trailing value", base + ` {"scope":"/"}`, "trailing content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseQuestionSet([]byte(tt.json))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
	if _, err := ParseQuestionSet([]byte(base)); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
}
