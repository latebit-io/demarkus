package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/storage"
)

func TestInvalidArgumentsDoNotCreateGCSClient(t *testing.T) {
	policyFile := filepath.Join(t.TempDir(), "policy.md")
	if err := os.WriteFile(policyFile, []byte("strictness: block\n"), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	invalidPolicyFile := filepath.Join(t.TempDir(), "invalid-policy.md")
	if err := os.WriteFile(invalidPolicyFile, []byte("strictness: invalid\n"), 0o600); err != nil {
		t.Fatalf("write invalid policy: %v", err)
	}
	tests := []struct {
		name      string
		arguments []string
	}{
		{name: "missing bucket", arguments: []string{"-world-id", testWorldID, "-policy-file", policyFile}},
		{name: "bucket path", arguments: []string{"-bucket", "gs://valid-bucket/path", "-world-id", testWorldID, "-policy-file", policyFile}},
		{name: "noncanonical world", arguments: []string{"-bucket", "gs://valid-bucket", "-world-id", "52B471F7-8D38-4C89-B44A-6F4F8B1A4F48", "-policy-file", policyFile}},
		{name: "missing policy", arguments: []string{"-bucket", "gs://valid-bucket", "-world-id", testWorldID}},
		{name: "invalid policy", arguments: []string{"-bucket", "gs://valid-bucket", "-world-id", testWorldID, "-policy-file", invalidPolicyFile}},
		{name: "positional argument", arguments: []string{"-bucket", "gs://valid-bucket", "-world-id", testWorldID, "-policy-file", policyFile, "extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			factory := func(context.Context) (*storage.Client, error) {
				called = true
				return nil, errors.New("unexpected GCS client creation")
			}
			if err := run(context.Background(), test.arguments, io.Discard, factory); err == nil {
				t.Fatal("run accepted invalid arguments")
			}
			if called {
				t.Fatal("GCS client created for invalid arguments")
			}
		})
	}
}

func TestVersionDoesNotCreateGCSClient(t *testing.T) {
	t.Run("prints version", func(t *testing.T) {
		called := false
		factory := func(context.Context) (*storage.Client, error) {
			called = true
			return nil, errors.New("unexpected GCS client creation")
		}
		var output bytes.Buffer
		if err := run(context.Background(), []string{"-version"}, &output, factory); err != nil {
			t.Fatalf("run: %v", err)
		}
		if called {
			t.Fatal("GCS client created for -version")
		}
		if output.String() != version+"\n" {
			t.Errorf("version output = %q, want %q", output.String(), version+"\n")
		}
	})
}
