// Package knowledgeconfig parses and validates multi-world knowledge-server configuration.
package knowledgeconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/protocol/publishpolicy"
	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only supported YAML schema version.
const SchemaVersion = 1

const defaultPolicyPath = publishpolicy.DocumentPath

// Duration is a time.Duration parsed from a YAML string such as "30s".
type Duration time.Duration

// UnmarshalYAML rejects YAML numbers and other implicit scalar types.
func (duration *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("duration must be a string such as \"30s\"")
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*duration = Duration(parsed)
	return nil
}

// Config is one complete, validated knowledge-server configuration.
type Config struct {
	Version int          `yaml:"version"`
	Listen  ListenConfig `yaml:"listen"`
	Health  HealthConfig `yaml:"health"`
	TLS     TLSConfig    `yaml:"tls"`
	// WorldsFile optionally names a worlds-only YAML document appended
	// to Worlds at Load: the dynamic-world seam a provisioner (the
	// memory broker) owns while the operator owns this file.
	WorldsFile string        `yaml:"worldsFile"`
	Worlds     []WorldConfig `yaml:"worlds"`
}

// ListenConfig contains process-wide QUIC listener settings.
type ListenConfig struct {
	Address            string   `yaml:"address"`
	MaxIncomingStreams int64    `yaml:"maxIncomingStreams"`
	IdleTimeout        Duration `yaml:"idleTimeout"`
}

// HealthConfig contains the private management listener address.
type HealthConfig struct {
	Address string `yaml:"address"`
}

// TLSConfig identifies the production certificate and private key.
type TLSConfig struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

// WorldConfig contains one logical world's static startup configuration.
type WorldConfig struct {
	Name        string       `yaml:"name"`
	Authorities []string     `yaml:"authorities"`
	Bucket      BucketConfig `yaml:"bucket"`
	Auth        AuthConfig   `yaml:"auth"`
	Policy      PolicyConfig `yaml:"policy"`
	ReadOnly    bool         `yaml:"readOnly"`
	Limits      LimitsConfig `yaml:"limits"`
	// Bootstrap makes the server initialize the bucket's genesis on world
	// open when it holds no head object, and opens the store without
	// requiring a policy document (the provisioner seeds one later).
	Bootstrap bool `yaml:"bootstrap"`
}

// BucketConfig identifies one world's GCS bucket and immutable marker ID.
type BucketConfig struct {
	URL     string `yaml:"url"`
	WorldID string `yaml:"worldID"`
}

// Name returns the validated GCS bucket name.
func (config BucketConfig) Name() string {
	return strings.TrimPrefix(config.URL, "gs://")
}

// ParseBucketURL validates an exact gs://bucket URL and returns its bucket name.
func ParseBucketURL(value string) (string, error) {
	if err := validateBucketURL(value); err != nil {
		return "", err
	}
	return strings.TrimPrefix(value, "gs://"), nil
}

// ValidWorldID reports whether worldID is canonical lowercase with an RFC 4122 variant.
func ValidWorldID(worldID string) bool {
	return validWorldID(worldID)
}

// AuthConfig identifies the world-local capability token file.
type AuthConfig struct {
	TokensFile string `yaml:"tokensFile"`
}

// PolicyConfig identifies the world-local policy document.
type PolicyConfig struct {
	Path string `yaml:"path"`
}

// LimitsConfig contains enforceable world-local fairness controls.
type LimitsConfig struct {
	MaxConcurrentRequests int      `yaml:"maxConcurrentRequests"`
	RequestTimeout        Duration `yaml:"requestTimeout"`
	RequestsPerSecond     float64  `yaml:"requestsPerSecond"`
	Burst                 int      `yaml:"burst"`
	// MaxDocuments caps distinct document paths in the world (per-tenant
	// quota). 0 = unlimited.
	MaxDocuments int `yaml:"maxDocuments"`
}

type rawConfig struct {
	Version    int              `yaml:"version"`
	Listen     rawListenConfig  `yaml:"listen"`
	Health     rawHealthConfig  `yaml:"health"`
	TLS        TLSConfig        `yaml:"tls"`
	WorldsFile string           `yaml:"worldsFile"`
	Worlds     []rawWorldConfig `yaml:"worlds"`
}

type rawListenConfig struct {
	Address            *string   `yaml:"address"`
	MaxIncomingStreams *int64    `yaml:"maxIncomingStreams"`
	IdleTimeout        *Duration `yaml:"idleTimeout"`
}

type rawHealthConfig struct {
	Address *string `yaml:"address"`
}

type rawWorldConfig struct {
	Name        string          `yaml:"name"`
	Authorities []string        `yaml:"authorities"`
	Bucket      BucketConfig    `yaml:"bucket"`
	Auth        AuthConfig      `yaml:"auth"`
	Policy      rawPolicyConfig `yaml:"policy"`
	ReadOnly    bool            `yaml:"readOnly"`
	Limits      rawLimitsConfig `yaml:"limits"`
	Bootstrap   bool            `yaml:"bootstrap"`
}

type rawPolicyConfig struct {
	Path *string `yaml:"path"`
}

type rawLimitsConfig struct {
	MaxConcurrentRequests *int      `yaml:"maxConcurrentRequests"`
	RequestTimeout        *Duration `yaml:"requestTimeout"`
	RequestsPerSecond     *float64  `yaml:"requestsPerSecond"`
	Burst                 *int      `yaml:"burst"`
	MaxDocuments          *int      `yaml:"maxDocuments"`
}

// Load reads, strictly parses, and validates a configuration file,
// merging the worldsFile fragment (relative to the config's directory).
// A missing or empty fragment contributes zero worlds.
func Load(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("knowledge config: read %q: %w", filename, err)
	}
	config, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("knowledge config %q: %w", filename, err)
	}
	if config.WorldsFile == "" {
		return config, nil
	}
	fragmentPath := config.WorldsFilePath(filename)
	fragment, err := os.ReadFile(fragmentPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("knowledge config: read worlds file %q: %w", fragmentPath, err)
		}
		fragment = nil
	}
	worlds, err := ParseWorldsFragment(fragment)
	if err != nil {
		return nil, fmt.Errorf("knowledge config: worlds file %q: %w", fragmentPath, err)
	}
	config.Worlds = append(config.Worlds, worlds...)
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("knowledge config: after merging worlds file %q: %w", fragmentPath, err)
	}
	return config, nil
}

// WorldsFilePath resolves the worldsFile fragment location against the
// main config's directory. Load and the hot-reload watcher share it so
// they can never watch a different file than the loader reads.
func (config *Config) WorldsFilePath(configFile string) string {
	if config.WorldsFile == "" || filepath.IsAbs(config.WorldsFile) {
		return config.WorldsFile
	}
	return filepath.Join(filepath.Dir(configFile), config.WorldsFile)
}

// ParseWorldsFragment strictly parses a worlds-only YAML document with
// Parse's per-world defaults; empty input yields zero worlds and
// cross-world invariants are validated by the caller after merging.
func ParseWorldsFragment(data []byte) ([]WorldConfig, error) {
	var raw struct {
		Worlds []rawWorldConfig `yaml:"worlds"`
	}
	if err := decodeStrictSingleDoc(data, &raw); err != nil {
		// Empty, whitespace-only, and comment-only fragments are a
		// legitimate not-yet-provisioned state.
		if errors.Is(err, errEmptyDocument) {
			return nil, nil
		}
		return nil, err
	}
	worlds := make([]WorldConfig, len(raw.Worlds))
	for index := range raw.Worlds {
		worlds[index] = worldFromRaw(&raw.Worlds[index])
	}
	return worlds, nil
}

// errEmptyDocument names the no-content outcome (empty, whitespace, or
// comments only) so callers state their own policy instead of matching
// the YAML library's io.EOF detail.
var errEmptyDocument = errors.New("empty YAML document")

// decodeStrictSingleDoc enforces the shared YAML posture: unknown fields
// rejected, exactly one document; no content is errEmptyDocument.
func decodeStrictSingleDoc(data []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errEmptyDocument
		}
		return fmt.Errorf("parse YAML: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("parse YAML: multiple documents are not allowed")
		}
		return fmt.Errorf("parse trailing YAML: %w", err)
	}
	return nil
}

// Parse strictly parses and validates one YAML document.
func Parse(data []byte) (*Config, error) {
	var raw rawConfig
	if err := decodeStrictSingleDoc(data, &raw); err != nil {
		if errors.Is(err, errEmptyDocument) {
			return nil, errors.New("configuration file is empty")
		}
		return nil, err
	}

	config := raw.config()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return config, nil
}

func (raw *rawConfig) config() *Config {
	config := &Config{
		Version: raw.Version,
		Listen: ListenConfig{
			Address:            valueOr(raw.Listen.Address, ":6309"),
			MaxIncomingStreams: valueOr(raw.Listen.MaxIncomingStreams, int64(128)),
			IdleTimeout:        valueOr(raw.Listen.IdleTimeout, Duration(30*time.Second)),
		},
		Health:     HealthConfig{Address: valueOr(raw.Health.Address, ":8081")},
		TLS:        raw.TLS,
		WorldsFile: strings.TrimSpace(raw.WorldsFile),
		Worlds:     make([]WorldConfig, len(raw.Worlds)),
	}
	for index := range raw.Worlds {
		config.Worlds[index] = worldFromRaw(&raw.Worlds[index])
	}
	return config
}

func worldFromRaw(world *rawWorldConfig) WorldConfig {
	return WorldConfig{
		Name:        world.Name,
		Authorities: append([]string(nil), world.Authorities...),
		Bucket:      world.Bucket,
		Auth:        world.Auth,
		Policy:      PolicyConfig{Path: valueOr(world.Policy.Path, defaultPolicyPath)},
		ReadOnly:    world.ReadOnly,
		Limits: LimitsConfig{
			MaxConcurrentRequests: valueOr(world.Limits.MaxConcurrentRequests, 32),
			RequestTimeout:        valueOr(world.Limits.RequestTimeout, Duration(10*time.Second)),
			RequestsPerSecond:     valueOr(world.Limits.RequestsPerSecond, 50.0),
			Burst:                 valueOr(world.Limits.Burst, 100),
			MaxDocuments:          valueOr(world.Limits.MaxDocuments, 0),
		},
		Bootstrap: world.Bootstrap,
	}
}

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

// Validate enforces schema, identity, routing, and limit invariants.
func (config *Config) Validate() error {
	if config == nil {
		return errors.New("config is nil")
	}
	if config.Version != SchemaVersion {
		return fmt.Errorf("version must be %d (got %d)", SchemaVersion, config.Version)
	}
	if strings.TrimSpace(config.Listen.Address) == "" {
		return errors.New("listen.address must not be empty")
	}
	if config.Listen.MaxIncomingStreams <= 0 {
		return fmt.Errorf("listen.maxIncomingStreams must be positive (got %d)", config.Listen.MaxIncomingStreams)
	}
	if config.Listen.IdleTimeout < 0 {
		return fmt.Errorf("listen.idleTimeout must not be negative (got %s)", time.Duration(config.Listen.IdleTimeout))
	}
	if strings.TrimSpace(config.Health.Address) == "" {
		return errors.New("health.address must not be empty")
	}
	if strings.TrimSpace(config.TLS.CertFile) == "" {
		return errors.New("tls.certFile is required")
	}
	if strings.TrimSpace(config.TLS.KeyFile) == "" {
		return errors.New("tls.keyFile is required")
	}
	// A dynamic deployment (worldsFile set) legitimately starts with zero
	// worlds: the server idles until the provisioner writes the first one.
	if len(config.Worlds) == 0 && config.WorldsFile == "" {
		return errors.New("worlds must contain at least one world")
	}
	return config.validateWorlds()
}

// validateWorlds enforces per-world and cross-world invariants; split
// from Validate for the gocyclo budget.
func (config *Config) validateWorlds() error {
	worldNames := make(map[string]int, len(config.Worlds))
	authorities := make(map[string]int)
	bucketURLs := make(map[string]int, len(config.Worlds))
	worldIDs := make(map[string]int, len(config.Worlds))
	tokenFiles := make(map[string]int, len(config.Worlds))
	for worldIndex := range config.Worlds {
		world := &config.Worlds[worldIndex]
		location := fmt.Sprintf("worlds[%d]", worldIndex)
		if strings.TrimSpace(world.Name) == "" {
			return fmt.Errorf("%s.name is required", location)
		}
		if previous, exists := worldNames[world.Name]; exists {
			return fmt.Errorf("%s.name %q duplicates worlds[%d].name", location, world.Name, previous)
		}
		worldNames[world.Name] = worldIndex

		if len(world.Authorities) == 0 {
			return fmt.Errorf("%s.authorities must contain at least one authority", location)
		}
		for authorityIndex, authority := range world.Authorities {
			normalized, err := normalizeAuthority(authority)
			if err != nil {
				return fmt.Errorf("%s.authorities[%d] %q: %w", location, authorityIndex, authority, err)
			}
			if previous, exists := authorities[normalized]; exists {
				return fmt.Errorf("%s.authorities[%d] %q duplicates authority in worlds[%d]", location, authorityIndex, authority, previous)
			}
			authorities[normalized] = worldIndex
			world.Authorities[authorityIndex] = normalized
		}

		if err := validateBucketURL(world.Bucket.URL); err != nil {
			return fmt.Errorf("%s.bucket.url: %w", location, err)
		}
		if previous, exists := bucketURLs[world.Bucket.URL]; exists {
			return fmt.Errorf("%s.bucket.url %q duplicates worlds[%d].bucket.url", location, world.Bucket.URL, previous)
		}
		bucketURLs[world.Bucket.URL] = worldIndex
		if !validWorldID(world.Bucket.WorldID) {
			return fmt.Errorf("%s.bucket.worldID must be a canonical lowercase UUID with RFC 4122 variant (got %q)", location, world.Bucket.WorldID)
		}
		if previous, exists := worldIDs[world.Bucket.WorldID]; exists {
			return fmt.Errorf("%s.bucket.worldID %q duplicates worlds[%d].bucket.worldID", location, world.Bucket.WorldID, previous)
		}
		worldIDs[world.Bucket.WorldID] = worldIndex

		if strings.TrimSpace(world.Auth.TokensFile) == "" {
			return fmt.Errorf("%s.auth.tokensFile is required", location)
		}
		cleanTokensFile := filepath.Clean(world.Auth.TokensFile)
		if previous, exists := tokenFiles[cleanTokensFile]; exists {
			return fmt.Errorf("%s.auth.tokensFile %q duplicates worlds[%d].auth.tokensFile", location, world.Auth.TokensFile, previous)
		}
		tokenFiles[cleanTokensFile] = worldIndex

		if err := validatePolicyPath(world.Policy.Path); err != nil {
			return fmt.Errorf("%s.policy.path: %w", location, err)
		}
		if err := validateLimits(location, &world.Limits); err != nil {
			return err
		}
	}
	return nil
}

func normalizeAuthority(authority string) (string, error) {
	if authority == "" {
		return "", errors.New("authority is empty")
	}
	for index := range len(authority) {
		if authority[index] >= 0x80 {
			return "", errors.New("unicode is not allowed; use an ASCII DNS A-label")
		}
	}
	if strings.Contains(authority, "*") {
		return "", errors.New("wildcards are not allowed")
	}
	if strings.HasSuffix(authority, ".") {
		return "", errors.New("trailing dots are not allowed")
	}
	if strings.Contains(authority, ":") {
		return "", errors.New("ports and IP literals are not allowed")
	}
	normalized := asciiLower(authority)
	if net.ParseIP(normalized) != nil {
		return "", errors.New("IP literals are not allowed")
	}
	if len(normalized) > 253 {
		return "", fmt.Errorf("DNS name exceeds 253 bytes (got %d)", len(normalized))
	}
	for label := range strings.SplitSeq(normalized, ".") {
		if label == "" {
			return "", errors.New("DNS labels must not be empty")
		}
		if len(label) > 63 {
			return "", fmt.Errorf("DNS label %q exceeds 63 bytes", label)
		}
		if !asciiLetterOrDigit(label[0]) || !asciiLetterOrDigit(label[len(label)-1]) {
			return "", fmt.Errorf("DNS label %q must start and end with a letter or digit", label)
		}
		for index := range len(label) {
			if !asciiLetterOrDigit(label[index]) && label[index] != '-' {
				return "", fmt.Errorf("DNS label %q contains invalid character %q", label, label[index])
			}
		}
	}
	return normalized, nil
}

func validateBucketURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("must be gs://bucket: %w", err)
	}
	if value != "gs://"+parsed.Host || parsed.Scheme != "gs" || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Hostname() != parsed.Host {
		return fmt.Errorf("must be exactly gs://bucket with no path, query, fragment, credentials, or port (got %q)", value)
	}
	if err := validateBucketName(parsed.Host); err != nil {
		return err
	}
	return nil
}

func validateBucketName(name string) error {
	maximumLength := 63
	if strings.Contains(name, ".") {
		maximumLength = 222
	}
	if len(name) < 3 || len(name) > maximumLength {
		return fmt.Errorf("bucket name length must be between 3 and %d bytes", maximumLength)
	}
	if !asciiLowercaseAlphanumeric(name[0]) || !asciiLowercaseAlphanumeric(name[len(name)-1]) {
		return fmt.Errorf("invalid GCS bucket name %q", name)
	}
	componentStart := 0
	for index := range len(name) {
		character := name[index]
		if character == '.' {
			if index == componentStart || index-componentStart > 63 || !asciiLowercaseAlphanumeric(name[index-1]) {
				return fmt.Errorf("invalid GCS bucket name %q", name)
			}
			componentStart = index + 1
			continue
		}
		if index == componentStart && !asciiLowercaseAlphanumeric(character) {
			return fmt.Errorf("invalid GCS bucket name %q", name)
		}
		if !asciiLowercaseAlphanumeric(character) && character != '-' && character != '_' {
			return fmt.Errorf("invalid GCS bucket name %q", name)
		}
	}
	if len(name)-componentStart > 63 || net.ParseIP(name) != nil || reservedBucketName(name) {
		return fmt.Errorf("invalid GCS bucket name %q", name)
	}
	return nil
}

func reservedBucketName(name string) bool {
	return strings.HasPrefix(name, "goog") || strings.Contains(name, "google") ||
		strings.Contains(name, "g00gle") || strings.Contains(name, "go0gle") || strings.Contains(name, "g0ogle")
}

func validWorldID(worldID string) bool {
	if len(worldID) != 36 {
		return false
	}
	for index, character := range []byte(worldID) {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if !asciiHex(character) {
				return false
			}
		}
	}
	return worldID[19] == '8' || worldID[19] == '9' || worldID[19] == 'a' || worldID[19] == 'b'
}

func validatePolicyPath(value string) error {
	if value == "" {
		return errors.New("path is required")
	}
	if strings.ContainsRune(value, '\x00') || !path.IsAbs(value) || path.Clean(value) != value {
		return fmt.Errorf("must be a canonical absolute path (got %q)", value)
	}
	if value != publishpolicy.DocumentPath {
		return fmt.Errorf("only %q is supported (got %q)", publishpolicy.DocumentPath, value)
	}
	return nil
}

func validateLimits(location string, limits *LimitsConfig) error {
	if limits.MaxDocuments < 0 {
		return fmt.Errorf("%s.limits.maxDocuments must not be negative (got %d)", location, limits.MaxDocuments)
	}
	if limits.MaxConcurrentRequests <= 0 {
		return fmt.Errorf("%s.limits.maxConcurrentRequests must be positive (got %d)", location, limits.MaxConcurrentRequests)
	}
	if limits.RequestTimeout < 0 {
		return fmt.Errorf("%s.limits.requestTimeout must not be negative (got %s)", location, time.Duration(limits.RequestTimeout))
	}
	if math.IsNaN(limits.RequestsPerSecond) || math.IsInf(limits.RequestsPerSecond, 0) {
		return fmt.Errorf("%s.limits.requestsPerSecond must be finite (got %v)", location, limits.RequestsPerSecond)
	}
	if limits.RequestsPerSecond < 0 {
		return fmt.Errorf("%s.limits.requestsPerSecond must not be negative (got %v)", location, limits.RequestsPerSecond)
	}
	if limits.Burst < 0 {
		return fmt.Errorf("%s.limits.burst must not be negative (got %d)", location, limits.Burst)
	}
	if limits.RequestsPerSecond > 0 && limits.Burst < 1 {
		return fmt.Errorf("%s.limits.burst must be positive when requestsPerSecond is enabled (got %d)", location, limits.Burst)
	}
	return nil
}

func asciiLower(value string) string {
	buffer := []byte(value)
	for index, character := range buffer {
		if character >= 'A' && character <= 'Z' {
			buffer[index] = character + ('a' - 'A')
		}
	}
	return string(buffer)
}

func asciiLetterOrDigit(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
}

func asciiLowercaseAlphanumeric(character byte) bool {
	return asciiLetterOrDigit(character)
}

func asciiHex(character byte) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f'
}
