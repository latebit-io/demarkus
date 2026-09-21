package protocol

import "testing"

func TestIsHashPath(t *testing.T) {
	tests := []struct {
		input    string
		wantHash string
		wantOK   bool
	}{
		{"sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", "sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true},
		{"/sha256-0000000000000000000000000000000000000000000000000000000000000000", "sha256-0000000000000000000000000000000000000000000000000000000000000000", true},
		{"sha256-AAAA", "", false},
		{"sha256-a1b2c3", "", false},
		{"md5-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", "", false},
		{"sha256-g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", "", false},
		{"sha256-a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2aa", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		hash, ok := IsHashPath(tt.input)
		if ok != tt.wantOK || hash != tt.wantHash {
			t.Errorf("IsHashPath(%q) = (%q, %v), want (%q, %v)", tt.input, hash, ok, tt.wantHash, tt.wantOK)
		}
	}
}

func TestVersionPath(t *testing.T) {
	if got := VersionPath("/docs/a.md", 3); got != "/docs/a.md/v3" {
		t.Errorf("VersionPath = %q", got)
	}
}
