package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestSource(t *testing.T) {
	t.Run("empty source", func(t *testing.T) {
		source, err := OpenSource(SourceConfig{})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}
		if source.Path() != "" {
			t.Errorf("Path: got %q, want empty", source.Path())
		}
		if source.Current() != nil {
			t.Fatal("Current: got store, want nil")
		}
		if err := source.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if source.Current() != nil {
			t.Fatal("Current after reload: got store, want nil")
		}
	})

	t.Run("initial load", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "tokens.toml")
		writeTokenFile(t, filePath, "sha256-b", "sha256-a")

		source, err := OpenSource(SourceConfig{TokensFile: filePath})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}
		if source.Path() != filePath {
			t.Errorf("Path: got %q, want %q", source.Path(), filePath)
		}
		if got, want := source.Current().Hashes(), []string{"sha256-a", "sha256-b"}; !slices.Equal(got, want) {
			t.Errorf("Hashes: got %v, want %v", got, want)
		}
	})

	t.Run("invalid initial load", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "tokens.toml")
		if err := os.WriteFile(filePath, []byte("invalid {{{"), 0o600); err != nil {
			t.Fatal(err)
		}

		source, err := OpenSource(SourceConfig{TokensFile: filePath})
		if err == nil {
			t.Fatal("OpenSource: got nil error, want malformed file error")
		}
		if source != nil {
			t.Fatal("OpenSource: got source after failed initial load")
		}
	})

	t.Run("valid reload", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "tokens.toml")
		writeTokenFile(t, filePath, "sha256-old")
		source, err := OpenSource(SourceConfig{TokensFile: filePath})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}

		writeTokenFile(t, filePath, "sha256-new")
		if err := source.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got, want := source.Current().Hashes(), []string{"sha256-new"}; !slices.Equal(got, want) {
			t.Errorf("Hashes: got %v, want %v", got, want)
		}
	})

	t.Run("staged load", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "tokens.toml")
		writeTokenFile(t, filePath, "sha256-old")
		source, err := OpenSource(SourceConfig{TokensFile: filePath})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}
		current := source.Current()
		writeTokenFile(t, filePath, "sha256-new")
		staged, err := source.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if source.Current() != current {
			t.Fatal("Load published staged tokens")
		}
		if err := source.Publish(staged); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if source.Current() != staged {
			t.Fatal("Publish did not install staged tokens")
		}
		if err := source.Publish(nil); err == nil {
			t.Fatal("Publish accepted nil token store")
		}
		if source.Current() != staged {
			t.Fatal("nil publish replaced current tokens")
		}
	})

	t.Run("malformed reload preserves current store", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "tokens.toml")
		writeTokenFile(t, filePath, "sha256-good")
		source, err := OpenSource(SourceConfig{TokensFile: filePath})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}
		current := source.Current()

		if err := os.WriteFile(filePath, []byte("invalid {{{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := source.Reload(); err == nil {
			t.Fatal("Reload: got nil error, want malformed file error")
		}
		if source.Current() != current {
			t.Fatal("Current changed after failed reload")
		}
	})

	t.Run("rename replacement", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "tokens.toml")
		writeTokenFile(t, filePath, "sha256-old")
		source, err := OpenSource(SourceConfig{TokensFile: filePath})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}

		staged := filepath.Join(dir, "tokens.toml.new")
		writeTokenFile(t, staged, "sha256-replaced")
		if err := os.Rename(staged, filePath); err != nil {
			t.Fatal(err)
		}
		if err := source.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got, want := source.Current().Hashes(), []string{"sha256-replaced"}; !slices.Equal(got, want) {
			t.Errorf("Hashes: got %v, want %v", got, want)
		}
	})
}

func TestTokenStoreHashes(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var store *TokenStore
		if hashes := store.Hashes(); hashes != nil {
			t.Errorf("Hashes: got %v, want nil", hashes)
		}
	})

	t.Run("sorted isolated copy", func(t *testing.T) {
		store := NewTokenStore(map[string]Token{
			"sha256-c": {},
			"sha256-a": {},
			"sha256-b": {},
		})
		want := []string{"sha256-a", "sha256-b", "sha256-c"}
		hashes := store.Hashes()
		if !slices.Equal(hashes, want) {
			t.Fatalf("Hashes: got %v, want %v", hashes, want)
		}

		hashes[0] = "changed"
		if got := store.Hashes(); !slices.Equal(got, want) {
			t.Errorf("Hashes after caller mutation: got %v, want %v", got, want)
		}
	})
}

func TestSourceConcurrentCurrentReload(t *testing.T) {
	t.Run("readers observe complete stores", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "tokens.toml")
		writeTokenFile(t, filePath, "sha256-a")
		source, err := OpenSource(SourceConfig{TokensFile: filePath})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}

		done := make(chan struct{})
		errCh := make(chan error, 1)
		var readers sync.WaitGroup
		for range 8 {
			readers.Go(func() {
				for {
					select {
					case <-done:
						return
					default:
						store := source.Current()
						hashes := store.Hashes()
						if len(hashes) != 1 || hashes[0] != "sha256-a" && hashes[0] != "sha256-b" {
							select {
							case errCh <- fmt.Errorf("unexpected hashes: %v", hashes):
							default:
							}
							return
						}
						runtime.Gosched()
					}
				}
			})
		}

		var reloadErr error
		for i := range 100 {
			hash := "sha256-a"
			if i%2 == 0 {
				hash = "sha256-b"
			}
			staged := filepath.Join(dir, "tokens.toml.new")
			if err := os.WriteFile(staged, tokenFile(hash), 0o600); err != nil {
				reloadErr = err
				break
			}
			if err := os.Rename(staged, filePath); err != nil {
				reloadErr = err
				break
			}
			if err := source.Reload(); err != nil {
				reloadErr = err
				break
			}
		}
		close(done)
		readers.Wait()
		if reloadErr != nil {
			t.Fatalf("concurrent reload: %v", reloadErr)
		}
		select {
		case err := <-errCh:
			t.Fatal(err)
		default:
		}
	})
}

func writeTokenFile(t *testing.T, filePath string, hashes ...string) {
	t.Helper()
	if err := os.WriteFile(filePath, tokenFile(hashes...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func tokenFile(hashes ...string) []byte {
	var data strings.Builder
	for i, hash := range hashes {
		data.WriteString("[tokens.token" + strconv.Itoa(i) + "]\n")
		data.WriteString("hash = " + strconv.Quote(hash) + "\n")
		data.WriteString("paths = [\"/**\"]\n")
		data.WriteString("operations = [\"read\"]\n")
	}
	return []byte(data.String())
}

func TestOptionalSource(t *testing.T) {
	t.Run("missing file is a load error unless optional", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "tokens.toml")
		if _, err := OpenSource(SourceConfig{TokensFile: missing}); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("OpenSource: got %v, want os.ErrNotExist", err)
		}
	})

	t.Run("missing optional file opens empty and loads once it appears", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "tokens.toml")
		source, err := OpenSource(SourceConfig{TokensFile: filePath, Optional: true})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}
		if got := source.Current().Hashes(); len(got) != 0 {
			t.Fatalf("Hashes before the file exists: got %v, want none", got)
		}
		if _, err := source.Current().Authorize("raw", "/doc.md", "publish"); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("Authorize with no tokens: got %v, want ErrInvalidToken", err)
		}

		writeTokenFile(t, filePath, "sha256-a")
		if err := source.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got, want := source.Current().Hashes(), []string{"sha256-a"}; !slices.Equal(got, want) {
			t.Errorf("Hashes after the file appeared: got %v, want %v", got, want)
		}

		// The store mirrors the disk: a removed file revokes its tokens.
		if err := os.Remove(filePath); err != nil {
			t.Fatal(err)
		}
		if err := source.Reload(); err != nil {
			t.Fatalf("Reload after removal: %v", err)
		}
		if got := source.Current().Hashes(); len(got) != 0 {
			t.Errorf("Hashes after removal: got %v, want none", got)
		}
	})

	t.Run("malformed optional file still fails", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "tokens.toml")
		if err := os.WriteFile(filePath, []byte("invalid {{{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenSource(SourceConfig{TokensFile: filePath, Optional: true}); err == nil {
			t.Fatal("OpenSource: got nil error, want malformed file error")
		}
	})
}

func TestStaticTokensFile(t *testing.T) {
	t.Run("merges with the runtime file", func(t *testing.T) {
		dir := t.TempDir()
		runtimeFile := filepath.Join(dir, "tokens.toml")
		staticFile := filepath.Join(dir, "static-tokens.toml")
		writeTokenFile(t, runtimeFile, "sha256-runtime")
		writeTokenFile(t, staticFile, "sha256-static")
		source, err := OpenSource(SourceConfig{TokensFile: runtimeFile, StaticTokensFile: staticFile})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}
		if got, want := source.Current().Hashes(), []string{"sha256-runtime", "sha256-static"}; !slices.Equal(got, want) {
			t.Errorf("Hashes: got %v, want %v", got, want)
		}
	})

	t.Run("static only while the runtime file is missing", func(t *testing.T) {
		dir := t.TempDir()
		runtimeFile := filepath.Join(dir, "tokens.toml")
		staticFile := filepath.Join(dir, "static-tokens.toml")
		writeTokenFile(t, staticFile, "sha256-static")
		source, err := OpenSource(SourceConfig{TokensFile: runtimeFile, StaticTokensFile: staticFile, Optional: true})
		if err != nil {
			t.Fatalf("OpenSource: %v", err)
		}
		if got, want := source.Current().Hashes(), []string{"sha256-static"}; !slices.Equal(got, want) {
			t.Errorf("Hashes: got %v, want %v", got, want)
		}
		writeTokenFile(t, runtimeFile, "sha256-runtime")
		if err := source.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got, want := source.Current().Hashes(), []string{"sha256-runtime", "sha256-static"}; !slices.Equal(got, want) {
			t.Errorf("Hashes after the runtime file appeared: got %v, want %v", got, want)
		}
	})

	t.Run("hash in both files is a load error", func(t *testing.T) {
		dir := t.TempDir()
		runtimeFile := filepath.Join(dir, "tokens.toml")
		staticFile := filepath.Join(dir, "static-tokens.toml")
		writeTokenFile(t, runtimeFile, "sha256-shared")
		writeTokenFile(t, staticFile, "sha256-shared")
		_, err := OpenSource(SourceConfig{TokensFile: runtimeFile, StaticTokensFile: staticFile})
		if err == nil || !strings.Contains(err.Error(), "across tokens files") {
			t.Fatalf("OpenSource: got %v, want duplicate hash error", err)
		}
	})
}
