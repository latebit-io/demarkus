package core

import "testing"

func TestLoadConfigCookieKeyOptional(t *testing.T) {
	clearConfigEnv(t)
	body := mustReplace(t, validConfig, "  cookieKey: \"dGVzdC1rZXk=\"\n", "")
	cfg, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("blank cookieKey rejected: %v", err)
	}
	if cfg.Server.CookieKey != "" || cfg.Server.CookieKeySecret != DefaultCookieKeySecret {
		t.Errorf("cookieKey = %q, cookieKeySecret = %q; want blank and %q", cfg.Server.CookieKey, cfg.Server.CookieKeySecret, DefaultCookieKeySecret)
	}
}

func TestLoadConfigCookieKeyEnvOverride(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("BROKER_COOKIE_KEY", "ZnJvbS1lbnYta2V5LTEyMzQ1Ng==")
	body := mustReplace(t, validConfig, "  cookieKey: \"dGVzdC1rZXk=\"\n", "")
	cfg, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.CookieKey != "ZnJvbS1lbnYta2V5LTEyMzQ1Ng==" {
		t.Errorf("cookieKey = %q, want the BROKER_COOKIE_KEY value", cfg.Server.CookieKey)
	}
}
