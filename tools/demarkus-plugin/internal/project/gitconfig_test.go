package project

import "testing"

func TestParseGitConfig(t *testing.T) {
	cases := []struct {
		name, text       string
		origin, override string
		has              bool
	}{
		{"origin and override", "[remote \"origin\"]\n\turl = https://h/o/app.git\n[demarkus]\n\tproject = mine\n", "https://h/o/app.git", "mine", true},
		{"legacy subsection", "[remote.origin]\nurl = git@h:o/app\n", "git@h:o/app", "", false},
		{"names are case-insensitive", "[Remote \"origin\"]\n\tURL = x\n[DEMARKUS]\n\tProject = y\n", "x", "y", true},
		{"subsection is case-sensitive", "[remote \"Origin\"]\n\turl = x\n", "", "", false},
		{"another remote", "[remote \"upstream\"]\n\turl = x\n", "", "", false},
		{"a subsection is not the key", "[demarkus \"x\"]\n\tproject = y\n", "", "", false},
		{"unquoted comment", "[remote \"origin\"]\n\turl = abc   # note\n", "abc", "", false},
		{"quoted comment characters", "[remote \"origin\"]\n\turl = \"a#b;c\"  ; note\n", "a#b;c", "", false},
		{"quoted trailing space", "[demarkus]\n\tproject = \"a \"\n", "", "a ", true},
		{"escapes", "[demarkus]\n\tproject = a\\\\b\\\"c\\tz\n", "", "a\\b\"c\tz", true},
		{"continuation", "[remote \"origin\"]\n\turl = https://h/o/\\\napp.git\n\tfetch = x\n", "https://h/o/app.git", "", false},
		{"first url, last project", "[remote \"origin\"]\n\turl = one\n\turl = two\n[demarkus]\n\tproject = a\n\tproject = b\n", "one", "b", true},
		{"valueless key", "[demarkus]\n\tproject\n", "", "", true},
		{"empty value", "[demarkus]\n\tproject =\n", "", "", true},
		{"same line as the section", "[demarkus] project = x\n", "", "x", true},
		{"crlf", "[remote \"origin\"]\r\n\turl = x\r\n", "x", "", false},
		{"escaped subsection", "[remote \"or\\\"igin\"]\n\turl = x\n", "", "", false},
		{"comments and blanks", "# top\n\n; other\n[remote \"origin\"] # trailing\n\turl = x\n", "x", "", false},
		{"unterminated section", "[remote \"origin\n\turl = x\n", "", "", false},
		{"empty", "", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := parseGitConfig(c.text)
			if v.origin != c.origin || v.override != c.override || v.hasOverride != c.has {
				t.Fatalf("parseGitConfig = %+v, want origin %q override %q (%v)", v, c.origin, c.override, c.has)
			}
		})
	}
}

func TestRepoName(t *testing.T) {
	cases := map[string]string{
		"https://github.com/org/app.git":      "app",
		"https://github.com/org/app":          "app",
		"https://github.com/org/app/":         "app",
		"ssh://git@github.com:22/org/app.git": "app",
		"git@github.com:org/app.git":          "app",
		"host:app.git":                        "app",
		"file:///srv/git/app.git":             "app",
		"/srv/git/app.git":                    "app",
		"/home/me/src/app/.git":               "app",
		"../app":                              "app",
		"https://github.com/Org/My App.git":   "my-app",
		"  https://h/o/app.git\n":             "app",
		"":                                    "",
	}
	for url, want := range cases {
		if got := repoName(url); got != want {
			t.Errorf("repoName(%q) = %q, want %q", url, got, want)
		}
	}
}
