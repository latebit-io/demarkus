package core

import "path/filepath"

// Secret layout. Names and keys are pinned: renaming one would orphan live
// Secrets on an in-place upgrade, and worlds read the write token Secrets by name.
const (
	// TokensSecretKey is the default data key holding a world's tokens.toml.
	TokensSecretKey = "tokens.toml"
	// RefreshTokensSecretKey holds the sha256(refresh_token) to record map.
	RefreshTokensSecretKey = "refresh_tokens.json"
	// DynamicClientsSecretKey holds the RFC 7591 registration map.
	DynamicClientsSecretKey = "dynamic-clients.json"
	// SigningKeySecretKey holds the generated id_token signing key PEM.
	SigningKeySecretKey = "signing-key.pem"
	// CookieKeySecretKey holds the generated base64 state cookie key.
	CookieKeySecretKey = "cookie-key"
	// worldWriteTokenSecretKey holds one world's JSON write token entry.
	worldWriteTokenSecretKey = "write-token.json"
	// agentTokenSecretKey holds one world's JSON agent token record.
	agentTokenSecretKey = "agent-token.json"
	// registrySecretKey holds the tenant registry JSON.
	registrySecretKey = "registry.json"
	// worldsFragmentKey holds the worlds fragment the knowledge server mounts.
	worldsFragmentKey = "worlds.yaml"

	DefaultRefreshTokensSecret  = "demarkus-broker-refresh-tokens"
	DefaultDynamicClientsSecret = "demarkus-broker-dynamic-clients"
	DefaultSigningKeySecret     = "demarkus-broker-signing-key"
	DefaultCookieKeySecret      = "demarkus-broker-cookie-key"
)

// worldWriteTokenSecretName is the per world write token Secret, one per
// world so RBAC can be scoped per world.
func worldWriteTokenSecretName(worldName string) string {
	return "demarkus-broker-write-token-" + worldName
}

// agentTokenSecretName is the broker's per world agent token record; its
// presence marks the agent's Secret as broker-managed.
func agentTokenSecretName(worldName string) string {
	return "demarkus-broker-agent-token-" + worldName
}

// brokerSecretRef locates a broker-owned document in both backends: a Secret
// in the broker namespace, or a file named by key under storage.dir.
func brokerSecretRef(cfg *Config, name, key string) SecretRef {
	ref := SecretRef{Namespace: cfg.Server.BrokerNamespace, Name: name, Key: key}
	if cfg.fileBackend() {
		ref.Path = filepath.Join(cfg.Storage.Dir, key)
	}
	return ref
}

// RefreshTokensRef locates the refresh token map.
func RefreshTokensRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.RefreshTokensSecret, RefreshTokensSecretKey)
}

// DynamicClientsRef locates the RFC 7591 registration map.
func DynamicClientsRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.DynamicClientsSecret, DynamicClientsSecretKey)
}

// SigningKeyRef locates the generated id_token signing key.
func SigningKeyRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.SigningKeySecret, SigningKeySecretKey)
}

// CookieKeyRef locates the generated state cookie key.
func CookieKeyRef(cfg *Config) SecretRef {
	return brokerSecretRef(cfg, cfg.Server.CookieKeySecret, CookieKeySecretKey)
}

// WorldWriteTokenRef locates the broker's copy of one world's write token.
func WorldWriteTokenRef(cfg *Config, worldName string) SecretRef {
	ref := SecretRef{
		Namespace: cfg.Server.BrokerNamespace,
		Name:      worldWriteTokenSecretName(worldName),
		Key:       worldWriteTokenSecretKey,
	}
	if cfg.fileBackend() {
		ref.Path = filepath.Join(cfg.Storage.Dir, worldWriteTokenSecretName(worldName)+".json")
	}
	return ref
}

// AgentTokenRecordRef locates the broker's record of one world's agent token.
func AgentTokenRecordRef(cfg *Config, worldName string) SecretRef {
	return SecretRef{
		Namespace: cfg.Server.BrokerNamespace,
		Name:      agentTokenSecretName(worldName),
		Key:       agentTokenSecretKey,
	}
}

// AgentTokenRef locates the raw agent token the agent mounts.
func AgentTokenRef(world *WorldConfig, spec *AgentTokenConfig) SecretRef {
	return SecretRef{Namespace: world.Namespace, Name: spec.Secret, Key: spec.Key}
}

// WorldTokensRef locates the world's own tokens.toml.
func WorldTokensRef(world *WorldConfig) SecretRef {
	key := world.TokensSecretKey
	if key == "" {
		key = TokensSecretKey
	}
	return SecretRef{
		Namespace: world.Namespace,
		Name:      world.TokensSecret,
		Key:       key,
		Path:      world.TokensFile,
	}
}

// RegistryRef locates the tenant registry.
func RegistryRef(cfg *Config) SecretRef {
	return SecretRef{
		Namespace: cfg.Server.BrokerNamespace,
		Name:      cfg.Provisioning.RegistrySecret,
		Key:       registrySecretKey,
	}
}

// WorldsFragmentRef locates the worlds fragment the knowledge server mounts.
func WorldsFragmentRef(cfg *Config) SecretRef {
	return SecretRef{
		Namespace: cfg.Provisioning.ServerNamespace,
		Name:      cfg.Provisioning.WorldsSecret,
		Key:       worldsFragmentKey,
	}
}
