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
	// worldWriteTokenSecretKey holds one world's JSON write token entry.
	worldWriteTokenSecretKey = "write-token.json"
	// registrySecretKey holds the tenant registry JSON.
	registrySecretKey = "registry.json"
	// worldsFragmentKey holds the worlds fragment the knowledge server mounts.
	worldsFragmentKey = "worlds.yaml"

	DefaultRefreshTokensSecret  = "demarkus-broker-refresh-tokens"
	DefaultDynamicClientsSecret = "demarkus-broker-dynamic-clients"
)

// worldWriteTokenSecretName is the per world write token Secret, one per
// world so RBAC can be scoped per world.
func worldWriteTokenSecretName(worldName string) string {
	return "demarkus-broker-write-token-" + worldName
}

// RefreshTokensRef locates the refresh token map. Every Ref builder resolves
// a document for both backends; Path is set only in file mode.
func RefreshTokensRef(cfg *Config) SecretRef {
	ref := SecretRef{
		Namespace: cfg.Server.BrokerNamespace,
		Name:      cfg.Server.RefreshTokensSecret,
		Key:       RefreshTokensSecretKey,
	}
	if cfg.fileBackend() {
		ref.Path = filepath.Join(cfg.Storage.Dir, RefreshTokensSecretKey)
	}
	return ref
}

// DynamicClientsRef locates the RFC 7591 registration map.
func DynamicClientsRef(cfg *Config) SecretRef {
	ref := SecretRef{
		Namespace: cfg.Server.BrokerNamespace,
		Name:      cfg.Server.DynamicClientsSecret,
		Key:       DynamicClientsSecretKey,
	}
	if cfg.fileBackend() {
		ref.Path = filepath.Join(cfg.Storage.Dir, DynamicClientsSecretKey)
	}
	return ref
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
