#!/usr/bin/env bash
# Demarkus Stack Install Script — the five-minute appliance.
#
# Stands up a complete, fully self-hosted knowledge system on one Linux
# host: world server + broker (knowledge system) + library (reading room)
# + Authelia (self-hosted OIDC) + Caddy (automatic HTTPS).
#
# Usage (fresh VPS, no flags needed):
#   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-stack.sh | sudo bash
#
# With a real domain (one wildcard A record: *.kb.example.com -> this host):
#   curl -fsSL ... | sudo bash -s -- --domain kb.example.com --owner-email you@example.com
#
# Without --domain the host defaults to <public-ip>.sslip.io (wildcard
# magic DNS): real DNS, real Let's Encrypt certificates, zero setup. The
# sslip identity is tied to this machine's IP; move to a real domain when
# you're keeping the system.
#
# Layering: this script drives install.sh (the server installer) for the
# demarkus components, then adds the identity and TLS layers on top. The
# base installer's `demarkus-install uninstall` removes everything,
# including the pieces added here.

set -euo pipefail

# --- shared constants ---------------------------------------------------------

INSTALL_DIR="/usr/local/bin"
GITHUB_REPO="latebit-io/demarkus"
RAW_BASE="https://raw.githubusercontent.com/${GITHUB_REPO}/main"

AUTH_SERVICE="demarkus-auth"
AUTH_CONFIG_DIR="/etc/demarkus-auth"
AUTH_STATE_DIR="/var/lib/demarkus-auth"
AUTHELIA_REPO="authelia/authelia"

PROXY_SERVICE="caddy"
PROXY_CONFIG_DIR="/etc/caddy"
PROXY_STATE_DIR="/var/lib/caddy"
CADDY_REPO="caddyserver/caddy"

BROKER_CONFIG_DIR="/etc/demarkus-broker"
LIBRARY_CONFIG_DIR="/etc/demarkus-library"
LIBRARY_SERVICE="demarkus-library"
SERVER_TOKENS="/etc/demarkus/tokens/tokens.toml"
AGENT_SERVICE="demarkus-agent"
AGENT_CONFIG_DIR="/etc/demarkus-agent"
AGENT_STATE_DIR="/var/lib/demarkus-agent"
WORLD_INTERNAL="localhost:6399"

# Fixed to systemd's search path in production; the test harness redirects it
# via DEMARKUS_TEST_SYSTEMD_DIR only under DEMARKUS_INSTALL_TESTMODE=1.
SYSTEMD_DIR="/etc/systemd/system"
if [ "${DEMARKUS_INSTALL_TESTMODE:-}" = "1" ] && [ -n "${DEMARKUS_TEST_SYSTEMD_DIR:-}" ]; then
  SYSTEMD_DIR="$DEMARKUS_TEST_SYSTEMD_DIR"
fi

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
log_step()  { printf "${BLUE}==>${NC} %s\n" "$1"; }
log_info()  { printf "${GREEN}  ✓${NC} %s\n" "$1"; }
log_warn()  { printf "${YELLOW}  !${NC} %s\n" "$1"; }
log_error() { printf "${RED}  ✗${NC} %s\n" "$1"; }

# --- platform -----------------------------------------------------------------

require_linux_root() {
  if [ "$(uname -s)" != "Linux" ]; then
    log_error "The stack installer is Linux-only (systemd). For a single component on macOS, use install.sh."
    exit 1
  fi
  if [ "$(id -u)" -ne 0 ]; then
    log_error "Run as root: curl ... | sudo bash"
    exit 1
  fi
  case "$(uname -m)" in
    x86_64|amd64)  GOARCH="amd64" ;;
    aarch64|arm64) GOARCH="arm64" ;;
    armv7l|armv7)  GOARCH="arm" ;;
    *) log_error "Unsupported architecture: $(uname -m)"; exit 1 ;;
  esac
}

# --- host resolution ----------------------------------------------------------

# resolve_stack_host prints the base host for the HTTPS surfaces: the domain
# when given, else <public-ip>.sslip.io so library./broker./auth. subdomains
# resolve with zero DNS setup.
resolve_stack_host() {
  local domain="$1"
  if [ -n "$domain" ]; then
    printf '%s' "$domain"
    return 0
  fi
  local ip
  # DigitalOcean metadata first (instant, authoritative), public echo fallback.
  ip=$(curl -fsS -m 2 http://169.254.169.254/metadata/v1/interfaces/public/0/ipv4/address 2>/dev/null) \
    || ip=$(curl -fsS -4 -m 5 https://api.ipify.org 2>/dev/null) || true
  if [ -z "$ip" ]; then
    log_error "Could not determine the public IP for sslip.io defaulting; pass --domain" >&2
    exit 1
  fi
  printf '%s.sslip.io' "$ip"
}

# --- base demarkus components (delegated to install.sh) -------------------------

# run_base_install drives the server installer for the demarkus components.
# No --domain is passed on purpose: Caddy owns ports 80/443 here, so the
# base installer's certbot path must not run; the QUIC server keeps its
# self-signed cert and every HTTP surface gets TLS from Caddy.
run_base_install() {
  local library_version="$1"
  local base_args=(--with-broker --with-library)
  if [ -n "$library_version" ]; then
    base_args+=(--library-version "$library_version")
  fi
  log_step "Installing demarkus server + broker + library (via install.sh)"
  if [ -f "./install.sh" ]; then
    bash ./install.sh "${base_args[@]}"
  else
    curl -fsSL "${RAW_BASE}/install.sh" | bash -s -- "${base_args[@]}"
  fi
}

# --- Authelia (self-hosted OIDC) ------------------------------------------------

# authelia_rand prints one generated alphanumeric secret.
authelia_rand() {
  "${INSTALL_DIR}/authelia" crypto rand --length 64 --charset alphanumeric 2>/dev/null | sed -n 's/^Random Value: //p'
}

# install_auth deploys Authelia. All secrets are generated with the freshly
# installed authelia binary (no extra dependencies). Prints key=value results
# the caller consumes: owner_user, owner_password, broker_client_secret.
install_auth() {
  local host="$1"
  local owner_email="$2"
  local tmpdir="$3"

  log_step "Installing Authelia (self-hosted OIDC)" >&2
  local av
  av=$(curl -fsSL "https://api.github.com/repos/${AUTHELIA_REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"\(v[^"]*\)".*/\1/')
  if [ -z "$av" ]; then
    log_error "Could not resolve the latest Authelia release" >&2
    exit 1
  fi
  local aasset="authelia-${av}-linux-${GOARCH}.tar.gz"
  if ! curl -fsSL "https://github.com/${AUTHELIA_REPO}/releases/download/${av}/${aasset}" -o "${tmpdir}/${aasset}"; then
    log_error "Could not download ${aasset}" >&2
    exit 1
  fi
  mkdir -p "${tmpdir}/authelia-x"
  tar -xzf "${tmpdir}/${aasset}" -C "${tmpdir}/authelia-x"
  local abin
  abin=$(find "${tmpdir}/authelia-x" -maxdepth 2 -type f -name 'authelia*' -perm -u+x | head -1)
  if [ -z "$abin" ]; then
    log_error "Authelia binary not found in ${aasset}" >&2
    exit 1
  fi
  install -m 755 "$abin" "${INSTALL_DIR}/authelia"

  if ! id "$AUTH_SERVICE" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$AUTH_SERVICE"
  fi
  mkdir -p "$AUTH_CONFIG_DIR" "$AUTH_STATE_DIR"
  chown root:"$AUTH_SERVICE" "$AUTH_CONFIG_DIR"
  chmod 750 "$AUTH_CONFIG_DIR"
  chown "$AUTH_SERVICE":"$AUTH_SERVICE" "$AUTH_STATE_DIR"
  chmod 700 "$AUTH_STATE_DIR"

  # Secrets: raw values only ever exist in this process and the config files.
  local session_secret storage_key jwt_secret hmac_secret
  session_secret=$(authelia_rand)
  storage_key=$(authelia_rand)
  jwt_secret=$(authelia_rand)
  hmac_secret=$(authelia_rand)
  if [ -z "$session_secret" ] || [ -z "$storage_key" ] || [ -z "$jwt_secret" ] || [ -z "$hmac_secret" ]; then
    log_error "Authelia secret generation failed (crypto rand output not recognized)" >&2
    exit 1
  fi

  # Broker OIDC client credential: authelia generates the random secret AND
  # its digest in one call; plaintext goes to the broker env, digest here.
  local pbkdf_out broker_client_secret broker_client_digest
  pbkdf_out=$("${INSTALL_DIR}/authelia" crypto hash generate pbkdf2 --variant sha512 --random --random.length 40 --random.charset alphanumeric 2>/dev/null)
  broker_client_secret=$(printf '%s\n' "$pbkdf_out" | sed -n 's/^Random Password: //p')
  broker_client_digest=$(printf '%s\n' "$pbkdf_out" | sed -n 's/^Digest: //p')

  # Owner account: random password, surfaced once in the summary card.
  local argon_out owner_user owner_password owner_digest
  owner_user="${owner_email%%@*}"
  [ -z "$owner_user" ] && owner_user="owner"
  argon_out=$("${INSTALL_DIR}/authelia" crypto hash generate argon2 --random --random.length 24 --random.charset alphanumeric 2>/dev/null)
  owner_password=$(printf '%s\n' "$argon_out" | sed -n 's/^Random Password: //p')
  owner_digest=$(printf '%s\n' "$argon_out" | sed -n 's/^Digest: //p')

  if [ -z "$broker_client_secret" ] || [ -z "$broker_client_digest" ] || [ -z "$owner_password" ] || [ -z "$owner_digest" ]; then
    log_error "Authelia hash generation failed (crypto hash output not recognized)" >&2
    exit 1
  fi

  # OIDC issuer signing key (authelia writes into an existing directory).
  mkdir -p "${tmpdir}/authelia-jwks"
  "${INSTALL_DIR}/authelia" crypto pair rsa generate --bits 2048 --directory "${tmpdir}/authelia-jwks" >/dev/null 2>&1
  if [ ! -f "${tmpdir}/authelia-jwks/private.pem" ]; then
    log_error "Authelia JWKS key generation failed" >&2
    exit 1
  fi

  if [ ! -f "${AUTH_CONFIG_DIR}/users.yml" ]; then
    {
      printf 'users:\n'
      printf '  %s:\n' "$owner_user"
      printf '    displayname: "%s"\n' "$owner_user"
      printf '    password: "%s"\n' "$owner_digest"
      printf '    email: %s\n' "$owner_email"
      printf '    groups:\n      - admins\n'
    } > "${AUTH_CONFIG_DIR}/users.yml"
    chown root:"$AUTH_SERVICE" "${AUTH_CONFIG_DIR}/users.yml"
    chmod 640 "${AUTH_CONFIG_DIR}/users.yml"
  else
    log_info "Keeping existing Authelia users: ${AUTH_CONFIG_DIR}/users.yml" >&2
  fi

  if [ ! -f "${AUTH_CONFIG_DIR}/configuration.yml" ]; then
    {
      printf 'server:\n  address: "tcp://127.0.0.1:9091"\n'
      printf 'log:\n  level: info\n'
      printf 'authentication_backend:\n  file:\n    path: %s/users.yml\n' "$AUTH_CONFIG_DIR"
      printf 'access_control:\n  default_policy: one_factor\n'
      printf 'session:\n  secret: "%s"\n' "$session_secret"
      printf '  cookies:\n    - domain: "%s"\n      authelia_url: "https://auth.%s"\n' "$host" "$host"
      printf 'storage:\n  encryption_key: "%s"\n  local:\n    path: %s/db.sqlite3\n' "$storage_key" "$AUTH_STATE_DIR"
      printf 'notifier:\n  filesystem:\n    filename: %s/notification.txt\n' "$AUTH_STATE_DIR"
      printf 'identity_validation:\n  reset_password:\n    jwt_secret: "%s"\n' "$jwt_secret"
      printf 'identity_providers:\n  oidc:\n'
      printf '    hmac_secret: "%s"\n' "$hmac_secret"
      printf '    jwks:\n      - key: |\n'
      sed 's/^/          /' "${tmpdir}/authelia-jwks/private.pem"
      # The claims policy is mandatory: the demarkus broker reads claims
      # from the id_token only and never calls UserInfo.
      printf '    claims_policies:\n      demarkus:\n'
      printf "        id_token: ['email', 'email_verified', 'groups', 'name']\n"
      printf '    clients:\n'
      printf '      - client_id: demarkus-broker\n'
      printf '        client_name: demarkus broker\n'
      printf '        client_secret: "%s"\n' "$broker_client_digest"
      printf '        public: false\n'
      printf '        authorization_policy: one_factor\n'
      printf '        claims_policy: demarkus\n'
      printf '        redirect_uris:\n          - "https://broker.%s/auth/callback"\n' "$host"
      printf "        scopes: ['openid', 'email', 'profile', 'groups']\n"
    } > "${AUTH_CONFIG_DIR}/configuration.yml"
    chown root:"$AUTH_SERVICE" "${AUTH_CONFIG_DIR}/configuration.yml"
    chmod 640 "${AUTH_CONFIG_DIR}/configuration.yml"
  else
    log_info "Keeping existing Authelia config: ${AUTH_CONFIG_DIR}/configuration.yml" >&2
  fi

  cat > "${SYSTEMD_DIR}/${AUTH_SERVICE}.service" << EOF
[Unit]
Description=Authelia (demarkus stack OIDC provider)
After=network.target

[Service]
Type=simple
User=${AUTH_SERVICE}
ExecStart=${INSTALL_DIR}/authelia --config ${AUTH_CONFIG_DIR}/configuration.yml
Restart=on-failure
RestartSec=5

ProtectSystem=strict
ReadWritePaths=${AUTH_STATE_DIR}
PrivateTmp=yes
NoNewPrivileges=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$AUTH_SERVICE"
  systemctl restart "$AUTH_SERVICE"
  log_info "Authelia running (loopback :9091, issuer https://auth.${host})" >&2

  printf 'owner_user=%s\n' "$owner_user"
  printf 'owner_password=%s\n' "$owner_password"
  printf 'broker_client_secret=%s\n' "$broker_client_secret"
}

# --- broker wiring --------------------------------------------------------------

# configure_broker fills the OIDC placeholders install.sh scaffolded and
# appends a webClients entry for the library (so the reading room can log
# users in via SSO). Restarts the broker once with the final config. Fails
# loudly if any placeholder survives.
configure_broker() {
  local host="$1"
  local broker_client_secret="$2"
  local library_client_hash="$3"

  log_step "Wiring the broker to Authelia + registering the library client"
  local cfg="${BROKER_CONFIG_DIR}/config.yaml"
  local env="${BROKER_CONFIG_DIR}/env"
  if [ ! -f "$cfg" ] || [ ! -f "$env" ]; then
    log_error "Broker scaffold missing (${cfg}); did the base install run?"
    exit 1
  fi
  sed -i \
    -e "s|https://EDIT-ME.example.com|https://broker.${host}|g" \
    -e "s|issuer: https://accounts.google.com|issuer: https://auth.${host}|" \
    -e "s|clientID: EDIT-ME-client-id|clientID: demarkus-broker|" \
    "$cfg"
  sed -i "s|^OIDC_CLIENT_SECRET=EDIT-ME$|OIDC_CLIENT_SECRET=${broker_client_secret}|" "$env"
  if grep -q "EDIT-ME" "$cfg" "$env"; then
    log_error "Broker scaffold placeholders not fully replaced; aborting (check ${cfg})"
    exit 1
  fi
  # Register the library as a confidential web client (idempotent).
  if ! grep -q "clientID: library" "$cfg"; then
    {
      printf '\nwebClients:\n'
      printf '  - clientID: library\n'
      printf '    clientSecretHash: %s\n' "$library_client_hash"
      printf '    redirectURIs: ["https://library.%s/auth/callback"]\n' "$host"
      printf '    name: reading room\n'
    } >> "$cfg"
  fi
  systemctl enable demarkus-broker
  systemctl restart demarkus-broker
  log_info "Broker running against https://auth.${host}, library client registered"
}

# configure_library flips the library from the base install's read-only quic
# mode to broker mode (per-reader SSO) and, if a key is given, enables the
# librarian AI. Restarts the library.
configure_library() {
  local host="$1"
  local library_client_secret="$2"
  local librarian_key="$3"

  log_step "Putting the library behind broker SSO"
  local env="${LIBRARY_CONFIG_DIR}/env"
  if [ ! -f "$env" ]; then
    log_error "Library scaffold missing (${env}); did the base install run?"
    exit 1
  fi
  {
    printf 'PORT=8090\n'
    printf 'DEMARKUS_TRANSPORT=broker\n'
    printf 'DEMARKUS_BROKER_URL=https://broker.%s\n' "$host"
    printf 'DEMARKUS_CLIENT_ID=library\n'
    printf 'DEMARKUS_CLIENT_SECRET=%s\n' "$library_client_secret"
    printf 'DEMARKUS_REDIRECT_URI=https://library.%s/auth/callback\n' "$host"
    printf 'DEMARKUS_WORLD=soul\n'
    if [ -n "$librarian_key" ]; then
      printf 'LLM_API_KEY=%s\n' "$librarian_key"
    fi
  } > "$env"
  chmod 640 "$env"
  chown root:"$LIBRARY_SERVICE" "$env" 2>/dev/null || true
  systemctl restart "$LIBRARY_SERVICE"
  if [ -n "$librarian_key" ]; then
    log_info "Library in SSO mode with the librarian AI enabled"
  else
    log_info "Library in SSO mode (librarian off; add LLM_API_KEY to ${env} to enable)"
  fi
}

# install_agent deploys the federation crawler as a daemon that indexes the
# local world and republishes its link graph (/graph.md), so the library and
# MCP graph/backlink views populate without per-client crawls.
install_agent() {
  local tmpdir="$1"

  log_step "Installing the indexing agent"
  local cver
  cver=$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases?per_page=20" \
    | grep '"tag_name": "client/' | head -1 | sed 's|.*client/v\([^"]*\)".*|\1|')
  if [ -z "$cver" ]; then
    log_error "Could not resolve the latest demarkus client release for the agent"
    exit 1
  fi
  local aasset="demarkus-agent_${cver}_linux_${GOARCH}.tar.gz"
  if ! curl -fsSL "https://github.com/${GITHUB_REPO}/releases/download/client/v${cver}/${aasset}" -o "${tmpdir}/${aasset}"; then
    log_error "Could not download ${aasset} (needs a client release carrying the agent tarball)"
    exit 1
  fi
  if curl -fsSL "https://github.com/${GITHUB_REPO}/releases/download/client/v${cver}/demarkus-client_checksums.txt" -o "${tmpdir}/agent-sums.txt"; then
    (cd "$tmpdir" && grep "$aasset" agent-sums.txt | sha256sum -c - >/dev/null) || {
      log_error "Checksum verification failed for ${aasset}"; exit 1; }
  else
    log_warn "Could not download client checksums; skipping agent verification"
  fi
  tar -xzf "${tmpdir}/${aasset}" -C "$tmpdir" demarkus-agent
  install -m 755 "${tmpdir}/demarkus-agent" "${INSTALL_DIR}/demarkus-agent"

  if ! id "$AGENT_SERVICE" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$AGENT_SERVICE"
  fi
  mkdir -p "$AGENT_CONFIG_DIR" "$AGENT_STATE_DIR"
  chown root:"$AGENT_SERVICE" "$AGENT_CONFIG_DIR"
  chmod 750 "$AGENT_CONFIG_DIR"
  chown "$AGENT_SERVICE":"$AGENT_SERVICE" "$AGENT_STATE_DIR"
  chmod 700 "$AGENT_STATE_DIR"

  # Publish token: appended to the broker-owned tokens.toml (the server
  # hot-reloads it) and handed to the agent via DEMARKUS_AUTH. generate
  # prints only the raw token on stdout; all messages go to stderr.
  local agent_token
  if ! agent_token=$("${INSTALL_DIR}/demarkus-token" generate -label agent-publish -paths "/*" -ops publish -tokens "$SERVER_TOKENS" 2>/dev/null); then
    log_error "Could not mint the agent publish token"
    exit 1
  fi
  agent_token=$(printf '%s' "$agent_token" | tr -d '[:space:]')
  if [ -z "$agent_token" ]; then
    log_error "Agent publish token was empty"
    exit 1
  fi

  if [ ! -f "${AGENT_CONFIG_DIR}/config.toml" ]; then
    {
      printf 'seeds = ["mark://%s"]\n' "$WORLD_INTERNAL"
      printf 'hubs = ["mark://%s"]\n' "$WORLD_INTERNAL"
      printf '\n[crawl]\nmax_depth = 3\n'
      printf '\n[schedule]\ninterval = "6h"\n'
      printf '\n[publish]\nretention = 20\n'
    } > "${AGENT_CONFIG_DIR}/config.toml"
    chown root:"$AGENT_SERVICE" "${AGENT_CONFIG_DIR}/config.toml"
    chmod 640 "${AGENT_CONFIG_DIR}/config.toml"
  fi
  printf 'DEMARKUS_AUTH=%s\n' "$agent_token" > "${AGENT_CONFIG_DIR}/env"
  chown root:"$AGENT_SERVICE" "${AGENT_CONFIG_DIR}/env"
  chmod 640 "${AGENT_CONFIG_DIR}/env"

  cat > "${SYSTEMD_DIR}/${AGENT_SERVICE}.service" << EOF
[Unit]
Description=Demarkus indexing agent (federation crawler)
After=network.target demarkus.service

[Service]
Type=simple
User=${AGENT_SERVICE}
EnvironmentFile=${AGENT_CONFIG_DIR}/env
ExecStart=${INSTALL_DIR}/demarkus-agent daemon -config ${AGENT_CONFIG_DIR}/config.toml -state ${AGENT_STATE_DIR}/fedcrawl.json -insecure -publish -publish-graph
Restart=on-failure
RestartSec=30

ProtectSystem=strict
ReadWritePaths=${AGENT_STATE_DIR}
PrivateTmp=yes
NoNewPrivileges=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$AGENT_SERVICE"
  systemctl restart "$AGENT_SERVICE"
  log_info "Indexing agent running (crawls ${WORLD_INTERNAL}, republishes /graph.md)"
}

# --- Caddy (TLS front) ----------------------------------------------------------

# install_proxy deploys Caddy as the TLS front for every HTTP surface:
# library.<host>, broker.<host> (path-split across the broker's two loopback
# listeners), auth.<host>. Caddy provisions certificates itself.
install_proxy() {
  local host="$1"
  local tmpdir="$2"

  log_step "Installing Caddy (TLS reverse proxy)"
  local cv
  cv=$(curl -fsSL "https://api.github.com/repos/${CADDY_REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"v\([^"]*\)".*/\1/')
  if [ -z "$cv" ]; then
    log_error "Could not resolve the latest Caddy release"
    exit 1
  fi
  local casset="caddy_${cv}_linux_${GOARCH}.tar.gz"
  if ! curl -fsSL "https://github.com/${CADDY_REPO}/releases/download/v${cv}/${casset}" -o "${tmpdir}/${casset}"; then
    log_error "Could not download ${casset}"
    exit 1
  fi
  mkdir -p "${tmpdir}/caddy-x"
  tar -xzf "${tmpdir}/${casset}" -C "${tmpdir}/caddy-x" caddy
  install -m 755 "${tmpdir}/caddy-x/caddy" "${INSTALL_DIR}/caddy"

  if ! id "$PROXY_SERVICE" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$PROXY_SERVICE"
  fi
  mkdir -p "$PROXY_CONFIG_DIR" "$PROXY_STATE_DIR"
  chown root:"$PROXY_SERVICE" "$PROXY_CONFIG_DIR"
  chmod 750 "$PROXY_CONFIG_DIR"
  chown "$PROXY_SERVICE":"$PROXY_SERVICE" "$PROXY_STATE_DIR"
  chmod 700 "$PROXY_STATE_DIR"

  if [ ! -f "${PROXY_CONFIG_DIR}/Caddyfile" ]; then
    cat > "${PROXY_CONFIG_DIR}/Caddyfile" << EOF
library.${host} {
	reverse_proxy 127.0.0.1:8090
}

broker.${host} {
	@mcp path /mcp /mcp/* /.well-known/oauth-protected-resource /.well-known/oauth-authorization-server
	handle @mcp {
		reverse_proxy 127.0.0.1:8081
	}
	handle {
		reverse_proxy 127.0.0.1:8080
	}
}

auth.${host} {
	reverse_proxy 127.0.0.1:9091
}
EOF
    chown root:"$PROXY_SERVICE" "${PROXY_CONFIG_DIR}/Caddyfile"
    chmod 640 "${PROXY_CONFIG_DIR}/Caddyfile"
  else
    log_info "Keeping existing Caddyfile: ${PROXY_CONFIG_DIR}/Caddyfile"
  fi

  if ! "${INSTALL_DIR}/caddy" validate --config "${PROXY_CONFIG_DIR}/Caddyfile" >/dev/null 2>&1; then
    log_error "Generated Caddyfile failed validation; aborting"
    exit 1
  fi

  cat > "${SYSTEMD_DIR}/${PROXY_SERVICE}.service" << EOF
[Unit]
Description=Caddy (demarkus stack TLS front)
After=network.target

[Service]
Type=simple
User=${PROXY_SERVICE}
Environment=XDG_DATA_HOME=${PROXY_STATE_DIR}
Environment=XDG_CONFIG_HOME=${PROXY_STATE_DIR}
ExecStart=${INSTALL_DIR}/caddy run --config ${PROXY_CONFIG_DIR}/Caddyfile
Restart=on-failure
RestartSec=5

AmbientCapabilities=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ReadWritePaths=${PROXY_STATE_DIR}
PrivateTmp=yes
NoNewPrivileges=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable "$PROXY_SERVICE"
  systemctl restart "$PROXY_SERVICE"

  if command -v ufw >/dev/null 2>&1; then
    if ! ufw allow 80/tcp >/dev/null 2>&1 || ! ufw allow 443/tcp >/dev/null 2>&1; then
      log_warn "Could not open 80/443 in ufw; Caddy may be unreachable until you open them"
    fi
  fi
  log_info "Caddy running: https://library.${host}, https://broker.${host}, https://auth.${host}"
}

# persist_self installs this script as demarkus-stack so `demarkus-stack
# uninstall` works later (mirrors install.sh's demarkus-install). Falls back
# to downloading when run from a pipe (curl | bash has no source file).
persist_self() {
  local self="${BASH_SOURCE[0]:-$0}"
  if [ -f "$self" ]; then
    install -m 755 "$self" "${INSTALL_DIR}/demarkus-stack"
  else
    local tmp; tmp=$(mktemp)
    if curl -fsSL "${RAW_BASE}/install-stack.sh" -o "$tmp" 2>/dev/null; then
      install -m 755 "$tmp" "${INSTALL_DIR}/demarkus-stack"; rm -f "$tmp"
    else
      rm -f "$tmp"
      log_warn "Could not persist demarkus-stack; uninstall with: curl ${RAW_BASE}/install-stack.sh | sudo bash -s -- uninstall"
    fi
  fi
}

# --- summary card ---------------------------------------------------------------

print_card() {
  local host="$1" owner_user="$2" owner_password="$3" owner_email="$4" librarian="$5"
  echo ""
  log_step "Your knowledge system is running"
  echo ""
  echo "  Library (read + edit):  https://library.${host}"
  echo "  Login:                  ${owner_user} / ${owner_password}"
  echo "                          (change it: ${AUTH_CONFIG_DIR}/users.yml, then restart demarkus-auth)"
  echo "  Owner email:            ${owner_email}"
  echo "  Agents join with:       /knowledge-join https://broker.${host}"
  echo "  Librarian AI:           ${librarian}"
  echo ""
  echo "  Notes:"
  echo "  - Certificates provision on first request; the first page load can take a few seconds."
  echo "  - The indexing agent republishes /graph.md every 6h so backlinks and the graph view populate."
  echo "  - Uninstall everything: demarkus-stack uninstall"
}

# --- uninstall ------------------------------------------------------------------

# do_uninstall removes the layers this script owns (Authelia, Caddy, the
# indexing agent) then delegates to the base uninstaller for the server,
# broker, and library. Each script tears down exactly what it installed.
do_uninstall() {
  require_linux_root
  log_step "Removing the stack's identity, proxy, and agent layers"
  for svc in "$AUTH_SERVICE" "$PROXY_SERVICE" "$AGENT_SERVICE"; do
    if [ -f "${SYSTEMD_DIR}/${svc}.service" ]; then
      systemctl stop "$svc" 2>/dev/null || true
      systemctl disable "$svc" 2>/dev/null || true
      rm -f "${SYSTEMD_DIR}/${svc}.service"
      log_info "Removed ${svc}"
    fi
  done
  systemctl daemon-reload 2>/dev/null || true
  for bin in authelia caddy demarkus-agent; do
    rm -f "${INSTALL_DIR}/${bin}"
  done
  # Authelia config holds credential hashes; the agent env holds a token.
  for d in "$AUTH_CONFIG_DIR" "$AUTH_STATE_DIR" "$PROXY_CONFIG_DIR" "$PROXY_STATE_DIR" "$AGENT_CONFIG_DIR" "$AGENT_STATE_DIR"; do
    [ -d "$d" ] && rm -rf "$d" && log_info "Removed ${d}/"
  done
  for u in "$AUTH_SERVICE" "$PROXY_SERVICE" "$AGENT_SERVICE"; do
    id "$u" >/dev/null 2>&1 && userdel "$u" 2>/dev/null && log_info "Removed user ${u}" || true
  done

  log_step "Delegating server/broker/library teardown to demarkus-install"
  if [ -x "${INSTALL_DIR}/demarkus-install" ]; then
    "${INSTALL_DIR}/demarkus-install" uninstall "$@"
  else
    log_warn "demarkus-install not found; server/broker/library not removed"
  fi
  log_step "Stack uninstall complete"
}

# --- main -----------------------------------------------------------------------

main() {
  # Subcommand: uninstall tears the whole stack down.
  if [ "${1:-}" = "uninstall" ]; then
    shift
    do_uninstall "$@"
    return
  fi

  local domain=""
  local owner_email=""
  local library_version=""
  local librarian_key=""

  while [ $# -gt 0 ]; do
    case "$1" in
      --domain)          domain="$2"; shift 2 ;;
      --owner-email)     owner_email="$2"; shift 2 ;;
      --librarian-key)   librarian_key="$2"; shift 2 ;;
      --library-version) library_version="$2"; shift 2 ;;
      *) log_error "Unknown option: $1"; exit 1 ;;
    esac
  done

  require_linux_root

  local host
  host=$(resolve_stack_host "$domain")
  log_step "Stack host: ${host} (library.${host}, broker.${host}, auth.${host})"
  [ -z "$owner_email" ] && owner_email="owner@${host}"

  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  run_base_install "$library_version"

  local auth_out owner_user owner_password broker_client_secret
  auth_out=$(install_auth "$host" "$owner_email" "$tmpdir")
  owner_user=$(printf '%s\n' "$auth_out" | sed -n 's/^owner_user=//p')
  owner_password=$(printf '%s\n' "$auth_out" | sed -n 's/^owner_password=//p')
  broker_client_secret=$(printf '%s\n' "$auth_out" | sed -n 's/^broker_client_secret=//p')
  if [ -z "$owner_user" ] || [ -z "$owner_password" ] || [ -z "$broker_client_secret" ]; then
    log_error "Authelia install did not return the generated credentials; aborting"
    exit 1
  fi

  # Library SSO credential: broker holds the hash, library holds the secret.
  local lib_secret lib_hash
  lib_secret=$(head -c 30 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 40)
  lib_hash=$(printf '%s' "$lib_secret" | sha256sum | cut -d' ' -f1)

  configure_broker "$host" "$broker_client_secret" "$lib_hash"
  configure_library "$host" "$lib_secret" "$librarian_key"
  install_agent "$tmpdir"
  install_proxy "$host" "$tmpdir"
  persist_self

  local librarian_status="off (add LLM_API_KEY to ${LIBRARY_CONFIG_DIR}/env)"
  [ -n "$librarian_key" ] && librarian_status="on"
  print_card "$host" "$owner_user" "$owner_password" "$owner_email" "$librarian_status"
}

# Guard so tests can source this script and call individual functions
# without running the installer.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  main "$@"
fi
