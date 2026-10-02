package core

// Secret layout. Names and keys are pinned: renaming one would orphan live
// Secrets on an in-place upgrade.
const (
	// DynamicClientsSecretKey held the RFC 7591 registration map.
	DynamicClientsSecretKey = "dynamic-clients.json"
	// OAuthStateSecretKey holds the issued authorization codes and device
	// grants every replica shares.
	OAuthStateSecretKey = "grants.json"
	// SigningKeySecretKey holds the generated id_token signing key PEM.
	SigningKeySecretKey = "signing-key.pem"
	// CookieKeySecretKey holds the generated base64 state cookie key.
	CookieKeySecretKey = "cookie-key"
	// registrySecretKey holds the tenant registry JSON.
	registrySecretKey = "registry.json"
	// worldsFragmentKey holds the worlds fragment the knowledge server mounts.
	worldsFragmentKey = "worlds.yaml"

	DefaultDynamicClientsSecret = "demarkus-broker-dynamic-clients"
	DefaultOAuthStateSecret     = "demarkus-broker-oauth-state"
	DefaultSigningKeySecret     = "demarkus-broker-signing-key"
	DefaultCookieKeySecret      = "demarkus-broker-cookie-key"
)

func brokerSecretRef(cfg *Config, name, key string) SecretRef {
	return SecretRef{Namespace: cfg.Server.BrokerNamespace, Name: name, Key: key}
}

// DynamicClientsRef locates the pre-bucket RFC 7591 registration map.
func DynamicClientsRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.DynamicClientsSecret, DynamicClientsSecretKey)
}

// OAuthStateRef locates the in-flight authorization codes and device grants.
func OAuthStateRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.OAuthStateSecret, OAuthStateSecretKey)
}

// SigningKeyRef locates the generated id_token signing key.
func SigningKeyRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.SigningKeySecret, SigningKeySecretKey)
}

// CookieKeyRef locates the generated state cookie key.
func CookieKeyRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.CookieKeySecret, CookieKeySecretKey)
}

// RegistryRef locates the tenant registry.
func RegistryRef(cfg *Config) SecretRef {
	return SecretRef{
		Namespace: cfg.Server.BrokerNamespace,
		Name:      cfg.Provisioning.RegistrySecret,
		Key:       registrySecretKey,
	}
}

// WorldsFragmentRef locates the worlds fragment the knowledge server in
// this process mounts, beside the broker's own Secrets.
func WorldsFragmentRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Provisioning.WorldsSecret, worldsFragmentKey)
}
