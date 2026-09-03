package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseMemoryJoinArgsAcceptsFlagsAfterHost(t *testing.T) {
	opts, err := parseMemoryJoinArgs([]string{"mark://example.com", "--bind", "/project", "--insecure"}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "mark://example.com" || opts.bind != "/project" || !opts.insecure {
		t.Fatalf("unexpected options: %+v", opts)
	}
}

func TestParseMemoryJoinArgsReadsTokenFromStdin(t *testing.T) {
	opts, err := parseMemoryJoinArgs([]string{"--token-stdin", "mark://example.com"}, strings.NewReader("secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.token != "secret" {
		t.Fatalf("token = %q, want secret", opts.token)
	}
}

func TestParseMemoryJoinArgsRejectsTwoTokenSources(t *testing.T) {
	_, err := parseMemoryJoinArgs([]string{"mark://example.com", "--token", "secret", "--token-stdin"}, strings.NewReader("other"))
	if err == nil {
		t.Fatal("parseMemoryJoinArgs accepted two token sources")
	}
}

func TestParseMemoryJoinArgsPreservesFlagPackageForms(t *testing.T) {
	opts, err := parseMemoryJoinArgs([]string{"-token=secret", "-bind", "/project", "--insecure=false", "--", "mark://example.com"}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "mark://example.com" || opts.token != "secret" || opts.bind != "/project" || opts.insecure {
		t.Fatalf("unexpected options: %+v", opts)
	}
}

func TestParseMemoryJoinArgsBoundsTokenStdin(t *testing.T) {
	token := strings.Repeat("x", maxMemoryJoinTokenInput)
	opts, err := parseMemoryJoinArgs([]string{"--token-stdin", "mark://example.com"}, strings.NewReader(token))
	if err != nil {
		t.Fatal(err)
	}
	if opts.token != token {
		t.Fatalf("token length = %d, want %d", len(opts.token), len(token))
	}
	_, err = parseMemoryJoinArgs([]string{"--token-stdin", "mark://example.com"}, strings.NewReader(token+"x"))
	if err == nil {
		t.Fatal("parseMemoryJoinArgs accepted token input over the limit")
	}
}

func TestParseMemoryJoinArgsRejectsEmptyTokenFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--token=", "mark://example.com"},
		{"--token", "", "mark://example.com"},
	} {
		if _, err := parseMemoryJoinArgs(args, strings.NewReader("")); err == nil {
			t.Fatalf("parseMemoryJoinArgs(%q) accepted an empty token", args)
		}
	}
}

func TestRegistryPromoteTargetReportsCanonicalPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-")
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = stdout
	registryPromoteTarget([]string{"add", "acme", "/shared//nested/", "Nested"})
	os.Stdout = originalStdout
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "OK: registered promote target acme /shared/nested\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
