package protocol

import "testing"

const testHex = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

func TestIsContentHash(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"sha256-" + testHex, true},
		{"sha256-0000000000000000000000000000000000000000000000000000000000000000", true},
		{"/sha256-" + testHex, false},
		{"sha256-AAAA", false},
		{"sha256-a1b2c3", false},
		{"md5-" + testHex, false},
		{"sha256-g" + testHex[1:], false},
		{"sha256-" + testHex + "aa", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsContentHash(tt.input); got != tt.want {
			t.Errorf("IsContentHash(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsReservedHashPath(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"/sha256-" + testHex, true},
		{"sha256-" + testHex, true},
		{"/sha256-" + testHex + "/v1", false},
		{"/docs/sha256-" + testHex, false},
		{"/sha256-short", false},
		{"/doc.md", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsReservedHashPath(tt.input); got != tt.want {
			t.Errorf("IsReservedHashPath(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestVersionPath(t *testing.T) {
	if got := VersionPath("/docs/a.md", 3); got != "/docs/a.md/v3" {
		t.Errorf("VersionPath = %q", got)
	}
}
