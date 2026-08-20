package main

import (
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
