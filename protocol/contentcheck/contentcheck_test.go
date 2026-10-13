package contentcheck

import (
	"reflect"
	"strings"
	"testing"
)

// Every fixture is synthetic: documented example values or repeated
// characters shaped like a credential. None is live.
func TestScanSecrets(t *testing.T) {
	cases := []struct {
		name string
		line string
		rule string
	}{
		{"pem", "-----BEGIN RSA PRIVATE KEY-----", "pem-private-key"},
		{"pem openssh", "    -----BEGIN OPENSSH PRIVATE KEY-----", "pem-private-key"},
		{"aws", "key: AKIAIOSFODNN7EXAMPLE", "aws-access-key"},
		{"github", "token=ghp_" + strings.Repeat("A", 36), "github-token"},
		{"github pat", "github_pat_" + strings.Repeat("a", 30), "github-token"},
		{"slack", "xoxb-" + strings.Repeat("1", 12) + "-abc", "slack-token"},
		{"google", "AIza" + strings.Repeat("A", 35), "google-api-key"},
		{"anthropic", "sk-ant-api03-" + strings.Repeat("a", 40), "anthropic-api-key"},
		{"openai", "sk-" + strings.Repeat("A", 40), "openai-api-key"},
		{"jwt", "Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJlLXNpZ25hdHVyZQ", "jwt"},
		{"connection string", "postgres://app:s3cr3tpass@db.internal:5432/app", "connection-string-password"},
		{"connection string with a slash in the password", "postgres://app:ab/cd9x@db.internal/app", "connection-string-password"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scan("# Doc\n\n```text\n"+c.line+"\n```\n", Options{})
			want := []Finding{{Rule: c.rule, Kind: Secret, Line: 4}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Scan = %v, want %v", got, want)
			}
		})
	}
}

func TestScanSecretsIgnoresPlaceholdersAndProse(t *testing.T) {
	clean := []string{
		"postgres://user:password@host/db",
		"postgres://user:${DB_PASSWORD}@host/db",
		"postgres://user:<password>@host/db",
		"mysql://root:xxxxxxxx@localhost/app",
		"https://example.com/path:with:colons",
		"The key id starts with AKIA and is sixteen characters long.",
		"sk-ant is the Anthropic prefix.",
		"an anthropic key reads sk-ant-... in the console",
	}
	for _, line := range clean {
		if got := Scan(line, Options{PII: Block}); len(got) != 0 {
			t.Errorf("%q flagged: %v", line, got)
		}
	}
}

func TestScanPII(t *testing.T) {
	opts := Options{PII: Block, WorkDomains: []string{"acme.example"}}
	cases := []struct {
		name string
		line string
		want []Finding
	}{
		{"sin with spaces", "SIN 046 454 286 on file", []Finding{{Rule: "canadian-sin", Kind: PII, Line: 1}}},
		{"sin with hyphens", "046-454-286", []Finding{{Rule: "canadian-sin", Kind: PII, Line: 1}}},
		{"sin bad checksum", "046 454 287", nil},
		{"sin bare digits not flagged", "order 046454286", nil},
		{"visa test number", "card 4111 1111 1111 1111 exp 12/30", []Finding{{Rule: "card-number", Kind: PII, Line: 1}}},
		{"card bad checksum", "4111 1111 1111 1112", nil},
		{"card wrong leading digit", "9111 1111 1111 1111", nil},
		{"outside email", "contact pat@gmail.com", []Finding{{Rule: "email-outside-work-domains", Kind: PII, Line: 1}}},
		{"work email", "contact pat@acme.example and ops@mail.acme.example", nil},
		{"reserved email", "alice@example.com, bob@example.org", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Scan(c.line, opts); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Scan = %v, want %v", got, c.want)
			}
		})
	}
	if got := Scan("pat@gmail.com", Options{PII: Block}); len(got) != 0 {
		t.Errorf("email rule ran without work domains: %v", got)
	}
	if got := Scan("046 454 286", Options{PII: Off}); len(got) != 0 {
		t.Errorf("PII rule ran with PII off: %v", got)
	}
	if got := Scan("046 454 286", Options{}); len(got) != 1 {
		t.Errorf("zero Options should run the PII rules: %v", got)
	}
}

func TestScanOrderAndSummary(t *testing.T) {
	body := "# Doc\n\n-----BEGIN PRIVATE KEY-----\n\nSIN 046 454 286 and pat@gmail.com\n"
	got := Scan(body, Options{PII: Block, WorkDomains: []string{"acme.example"}})
	want := []Finding{
		{Rule: "pem-private-key", Kind: Secret, Line: 3},
		{Rule: "canadian-sin", Kind: PII, Line: 5},
		{Rule: "email-outside-work-domains", Kind: PII, Line: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Scan = %v, want %v", got, want)
	}
	if !Secrets(got) || Secrets(got[1:]) {
		t.Error("Secrets misreports")
	}
	summary := Summary(got)
	if summary != "secret/pem-private-key at line 3; pii/canadian-sin at line 5; pii/email-outside-work-domains at line 5" {
		t.Errorf("Summary = %q", summary)
	}
	for _, leaked := range []string{"046", "gmail", "BEGIN"} {
		if strings.Contains(summary, leaked) {
			t.Errorf("summary echoes %q", leaked)
		}
	}
}

func TestRemedies(t *testing.T) {
	secret := []Finding{{Rule: "pem-private-key", Kind: Secret, Line: 1}}
	pii := []Finding{{Rule: "canadian-sin", Kind: PII, Line: 1}}
	if !strings.Contains(Remedy(secret), "cannot be overridden") || !strings.Contains(Remedy(pii), OverrideKey) {
		t.Errorf("Remedy = %q / %q", Remedy(secret), Remedy(pii))
	}
	if !strings.Contains(StoredRemedy(secret), "rotate or revoke") || StoredRemedy(pii) != Remedy(pii) {
		t.Errorf("StoredRemedy = %q / %q", StoredRemedy(secret), StoredRemedy(pii))
	}
}

func TestLuhn(t *testing.T) {
	for digits, want := range map[string]bool{"4111111111111111": true, "4111111111111112": false, "046454286": true, "0": false, "12a4": false} {
		if got := luhn(digits); got != want {
			t.Errorf("luhn(%q) = %v, want %v", digits, got, want)
		}
	}
}
