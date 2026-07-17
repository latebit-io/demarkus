package joinurl

import (
	"testing"
)

func TestBuildParseRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		join Join
	}{
		{"host only", Join{Host: "kb.example.com"}},
		{"host with port", Join{Host: "kb.example.com:6310"}},
		{"token", Join{Host: "kb.example.com", Token: "abc123"}},
		{"token needing escaping", Join{Host: "kb.example.com:6309", Token: "tok+/=x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Build(tt.join)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			got, err := Parse(s)
			if err != nil {
				t.Fatalf("Parse(%q): %v", s, err)
			}
			if got != tt.join {
				t.Errorf("round trip: got %+v, want %+v", got, tt.join)
			}
		})
	}
}

func TestBuildRejects(t *testing.T) {
	tests := []struct {
		name string
		join Join
	}{
		{"empty host", Join{}},
		{"host with path", Join{Host: "h/x"}},
		{"host with fragment", Join{Host: "h#f"}},
		{"host with userinfo", Join{Host: "u@h"}},
		{"ipv6 literal host", Join{Host: "[::1]:6309"}},
		{"port-only host", Join{Host: ":6309"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Build(tt.join); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestParseForms(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Join
		wantErr bool
	}{
		{"bare host", "kb.example.com", Join{Host: "kb.example.com"}, false},
		{"mark scheme", "mark://kb.example.com", Join{Host: "kb.example.com"}, false},
		{"trailing slash", "mark://kb.example.com/", Join{Host: "kb.example.com"}, false},
		{"fragment", "kb.example.com#token=t", Join{Host: "kb.example.com", Token: "t"}, false},
		{"whitespace", "  kb.example.com  ", Join{Host: "kb.example.com"}, false},
		{"empty", "", Join{}, true},
		{"https scheme", "https://kb.example.com", Join{}, true},
		{"with path", "mark://h/docs/x.md", Join{}, true},
		{"unknown fragment key", "h#token=t&extra=1", Join{}, true},
		{"future fp key fails loudly", "h#token=t&fp=sha256:ab", Join{}, true},
		{"userinfo rejected", "mark://user@h#token=t", Join{}, true},
		{"query rejected", "mark://h?x=1#token=t", Join{}, true},
		{"duplicate token rejected", "h#token=a&token=b", Join{}, true},
		{"ipv6 literal rejected", "mark://[2001:db8::1]:6309#token=t", Join{}, true},
		{"port-only authority rejected", "mark://:6309#token=t", Join{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHasCredentials(t *testing.T) {
	if HasCredentials("kb.example.com") {
		t.Error("bare host should not have credentials")
	}
	if !HasCredentials("kb.example.com#token=t") {
		t.Error("token fragment should have credentials")
	}
	if HasCredentials("h#bogus=1") {
		t.Error("invalid join URL should not report credentials")
	}
}
