package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseSoulJoinArgsAcceptsFlagsAfterHost(t *testing.T) {
	opts, err := parseSoulJoinArgs([]string{"mark://example.com", "--bind", "/project", "--insecure"}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "mark://example.com" || opts.bind != "/project" || !opts.insecure {
		t.Fatalf("unexpected options: %+v", opts)
	}
}

func TestParseSoulJoinArgsReadsTokenFromStdin(t *testing.T) {
	opts, err := parseSoulJoinArgs([]string{"--token-stdin", "mark://example.com"}, strings.NewReader("secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	if opts.token != "secret" {
		t.Fatalf("token = %q, want secret", opts.token)
	}
}

func TestParseSoulJoinArgsRejectsTwoTokenSources(t *testing.T) {
	_, err := parseSoulJoinArgs([]string{"mark://example.com", "--token", "secret", "--token-stdin"}, strings.NewReader("other"))
	if err == nil {
		t.Fatal("parseSoulJoinArgs accepted two token sources")
	}
}

func TestParseSoulJoinArgsPreservesFlagPackageForms(t *testing.T) {
	opts, err := parseSoulJoinArgs([]string{"-token=secret", "-bind", "/project", "--insecure=false", "--", "mark://example.com"}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "mark://example.com" || opts.token != "secret" || opts.bind != "/project" || opts.insecure {
		t.Fatalf("unexpected options: %+v", opts)
	}
}

func TestParseSoulJoinArgsBoundsTokenStdin(t *testing.T) {
	token := strings.Repeat("x", maxSoulJoinTokenInput)
	opts, err := parseSoulJoinArgs([]string{"--token-stdin", "mark://example.com"}, strings.NewReader(token))
	if err != nil {
		t.Fatal(err)
	}
	if opts.token != token {
		t.Fatalf("token length = %d, want %d", len(opts.token), len(token))
	}
	_, err = parseSoulJoinArgs([]string{"--token-stdin", "mark://example.com"}, strings.NewReader(token+"x"))
	if err == nil {
		t.Fatal("parseSoulJoinArgs accepted token input over the limit")
	}
}

func TestParseSoulJoinArgsRejectsEmptyTokenFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--token=", "mark://example.com"},
		{"--token", "", "mark://example.com"},
	} {
		if _, err := parseSoulJoinArgs(args, strings.NewReader("")); err == nil {
			t.Fatalf("parseSoulJoinArgs(%q) accepted an empty token", args)
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
