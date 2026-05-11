package protocol

import "testing"

func TestHashToken(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "empty string",
			raw:  "",
			want: "sha256-e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name: "ascii token",
			raw:  "hello",
			want: "sha256-2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		},
		{
			name: "hex token of typical mint length",
			raw:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			want: "sha256-a8ae6e6ee929abea3afcfc5258c8ccd6f85273e0d4626d26c7279f3250f77c8e",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HashToken(tt.raw)
			if got != tt.want {
				t.Errorf("HashToken(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestHashTokenDeterministic(t *testing.T) {
	a := HashToken("the-same-input")
	b := HashToken("the-same-input")
	if a != b {
		t.Fatalf("HashToken non-deterministic: %q vs %q", a, b)
	}
}
