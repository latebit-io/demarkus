package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A damaged config must not read as "no config": provision would then run the
// default setup over it and switch the memory directory without asking.
func TestLoadConfigDistinguishesAbsentFromIncomplete(t *testing.T) {
	tests := []struct {
		name    string
		content *string
		wantErr bool
		wantCfg bool
	}{
		{name: "absent file"},
		{name: "empty file", content: new("")},
		{name: "complete", content: new("SOUL_DIR=/m\nPORT=6309\nMODE=default\n"), wantCfg: true},
		{name: "missing MODE", content: new("SOUL_DIR=/m\nPORT=6309\n"), wantErr: true},
		{name: "truncated", content: new("SOUL_DI"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if tt.content != nil {
				dir := filepath.Join(home, ".demarkus")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "plugin-memory.conf"), []byte(*tt.content), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			cfg, err := LoadConfig()
			if tt.wantErr != (err != nil) {
				t.Fatalf("LoadConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrConfigIncomplete) {
				t.Errorf("error = %v, want ErrConfigIncomplete", err)
			}
			if tt.wantCfg != (cfg != nil) {
				t.Errorf("cfg = %+v, want present=%v", cfg, tt.wantCfg)
			}
		})
	}
}
