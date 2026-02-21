#!/usr/bin/env bash
#
# Demarkus Install Script
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
#   curl -fsSL ... | bash -s -- --domain example.com --root /srv/site
#   curl -fsSL ... | bash -s -- --client-only
#
# For private repos, set GITHUB_TOKEN:
#   curl -fsSL -H "Authorization: token $GITHUB_TOKEN" \
#     https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
#     | GITHUB_TOKEN=$GITHUB_TOKEN bash
#
# After install:
#   demarkus-install update          # update to latest version
#   demarkus-install uninstall       # remove everything
#
set -euo pipefail

GITHUB_REPO="latebit-io/demarkus"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR_LINUX="/etc/demarkus"
CONFIG_DIR_MACOS="$HOME/.demarkus"
DEFAULT_ROOT_LINUX="/var/lib/demarkus"
DEFAULT_ROOT_MACOS="$HOME/.demarkus/content"
SERVICE_NAME="demarkus"
SCRIPT_VERSION="1"

# --- Logging ---

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()  { printf "${GREEN}[info]${NC}  %s\n" "$*"; }
log_warn()  { printf "${YELLOW}[warn]${NC}  %s\n" "$*"; }
log_error() { printf "${RED}[error]${NC} %s\n" "$*" >&2; }
log_step()  { printf "${BLUE}==> %s${NC}\n" "$*"; }

# --- Platform detection ---

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)

  case "$OS" in
    linux)  PLATFORM="linux" ;;
    darwin) PLATFORM="darwin" ;;
    *)      log_error "Unsupported OS: $OS"; exit 1 ;;
  esac

  case "$ARCH" in
    x86_64|amd64)  GOARCH="amd64" ;;
    aarch64|arm64)  GOARCH="arm64" ;;
    armv7l|armhf)   GOARCH="arm" ;;
    *)              log_error "Unsupported architecture: $ARCH"; exit 1 ;;
  esac

  # Set platform-specific defaults
  if [ "$PLATFORM" = "linux" ]; then
    CONFIG_DIR="$CONFIG_DIR_LINUX"
    DEFAULT_ROOT="$DEFAULT_ROOT_LINUX"
  else
    CONFIG_DIR="$CONFIG_DIR_MACOS"
    DEFAULT_ROOT="$DEFAULT_ROOT_MACOS"
  fi
}

# --- Semver helpers ---

# Parse version string into comparable integer: 1.2.3 -> 1002003
version_num() {
  local IFS='.'
  local parts=($1)
  echo $(( ${parts[0]:-0} * 1000000 + ${parts[1]:-0} * 1000 + ${parts[2]:-0} ))
}

version_lt() { [ "$(version_num "$1")" -lt "$(version_num "$2")" ]; }
version_gte() { [ "$(version_num "$1")" -ge "$(version_num "$2")" ]; }

# --- GitHub auth ---

# Auth args for curl when GITHUB_TOKEN is set (for private repos).
# Used as: curl -fsSL "${CURL_AUTH_ARGS[@]}" "$url"
CURL_AUTH_ARGS=()
if [ -n "${GITHUB_TOKEN:-}" ]; then
  CURL_AUTH_ARGS=(-H "Authorization: token ${GITHUB_TOKEN}")
fi

# --- GitHub API ---

github_api() {
  local url="$1"
  curl -fsSL "${CURL_AUTH_ARGS[@]}" "$url" 2>/dev/null
}

fetch_latest_version() {
  local component="$1" # "server" or "client"
  local url="https://api.github.com/repos/${GITHUB_REPO}/releases"
  local releases

  releases=$(github_api "$url") || {
    log_error "Failed to fetch releases from GitHub"
    exit 1
  }

  # Find the latest release tagged with the component prefix
  echo "$releases" | grep -o "\"tag_name\": \"${component}/v[^\"]*\"" \
    | head -1 \
    | sed "s/\"tag_name\": \"${component}\/v\(.*\)\"/\1/"
}

# Download a release asset. For private repos (GITHUB_TOKEN set), uses the
# GitHub API to resolve asset IDs and download via the API endpoint.
# For public repos, uses the direct github.com download URL.
download_asset_file() {
  local tag="$1"
  local filename="$2"
  local output_path="$3"

  if [ -n "${GITHUB_TOKEN:-}" ]; then
    # Private repo: resolve asset ID via API, then download with octet-stream accept
    local encoded_tag
    encoded_tag=$(printf '%s' "$tag" | sed 's|/|%2F|g')
    local release_url="https://api.github.com/repos/${GITHUB_REPO}/releases/tags/${encoded_tag}"
    local release_json

    release_json=$(github_api "$release_url") || {
      log_error "Failed to fetch release for tag ${tag}"
      return 1
    }

    # Extract the asset API URL for the given filename.
    # Look for the assets/NNNN URL that appears near our filename in the JSON.
    local asset_url
    asset_url=$(echo "$release_json" \
      | grep -B10 "\"name\": \"${filename}\"" \
      | grep '"url":.*releases/assets/' \
      | head -1 \
      | sed 's/.*"url": "\([^"]*\)".*/\1/')

    if [ -z "$asset_url" ]; then
      log_error "Asset ${filename} not found in release ${tag}"
      return 1
    fi

    curl -fsSL -H "Authorization: token ${GITHUB_TOKEN}" \
      -H "Accept: application/octet-stream" \
      -o "$output_path" "$asset_url" || return 1
  else
    # Public repo: direct download URL
    local encoded_tag
    encoded_tag=$(printf '%s' "$tag" | sed 's|/|%2F|g')
    local url="https://github.com/${GITHUB_REPO}/releases/download/${encoded_tag}/${filename}"
    curl -fsSL -o "$output_path" "$url" || return 1
  fi
}

download_and_verify() {
  local component="$1" # "server" or "client"
  local version="$2"
  local tmpdir="$3"

  local tag="${component}/v${version}"
  local archive_name="demarkus-${component}_${version}_${PLATFORM}_${GOARCH}"
  local archive_ext="tar.gz"
  if [ "$PLATFORM" = "windows" ]; then
    archive_ext="zip"
  fi
  local archive_file="${archive_name}.${archive_ext}"
  local checksums_file="demarkus-${component}_checksums.txt"

  log_info "Downloading ${component} v${version} for ${PLATFORM}/${GOARCH}..."

  download_asset_file "$tag" "$archive_file" "${tmpdir}/${archive_file}" || {
    log_error "Failed to download ${archive_file}"
    exit 1
  }

  download_asset_file "$tag" "$checksums_file" "${tmpdir}/${checksums_file}" || {
    log_warn "Could not download checksums file, skipping verification"
    tar xzf "${tmpdir}/${archive_file}" -C "${tmpdir}"
    return 0
  }

  log_info "Verifying checksum..."
  local expected
  expected=$(grep "${archive_file}" "${tmpdir}/${checksums_file}" | awk '{print $1}')
  if [ -z "$expected" ]; then
    log_warn "No checksum found for ${archive_file}, skipping verification"
    tar xzf "${tmpdir}/${archive_file}" -C "${tmpdir}"
    return 0
  fi

  local actual
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${tmpdir}/${archive_file}" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "${tmpdir}/${archive_file}" | awk '{print $1}')
  else
    log_warn "No sha256sum or shasum available, skipping verification"
    tar xzf "${tmpdir}/${archive_file}" -C "${tmpdir}"
    return 0
  fi

  if [ "$expected" != "$actual" ]; then
    log_error "Checksum mismatch!"
    log_error "  Expected: $expected"
    log_error "  Actual:   $actual"
    exit 1
  fi
  log_info "Checksum verified"

  # Extract
  tar xzf "${tmpdir}/${archive_file}" -C "${tmpdir}"
}

download_and_verify_asset() {
  local asset_prefix="$1" # e.g. "demarkus-tui"
  local version="$2"
  local component="$3"    # release component for tag, e.g. "client"
  local tmpdir="$4"

  local tag="${component}/v${version}"
  local archive_name="${asset_prefix}_${version}_${PLATFORM}_${GOARCH}"
  local archive_ext="tar.gz"
  if [ "$PLATFORM" = "windows" ]; then
    archive_ext="zip"
  fi
  local archive_file="${archive_name}.${archive_ext}"
  local checksums_file="demarkus-${component}_checksums.txt"

  log_info "Downloading ${asset_prefix} v${version} for ${PLATFORM}/${GOARCH}..."

  download_asset_file "$tag" "$archive_file" "${tmpdir}/${archive_file}" || {
    log_error "Failed to download ${archive_file}"
    exit 1
  }

  # Checksums file may already be downloaded by a prior download_and_verify call
  if [ -f "${tmpdir}/${checksums_file}" ]; then
    log_info "Verifying checksum..."
    local expected
    expected=$(grep "${archive_file}" "${tmpdir}/${checksums_file}" | awk '{print $1}')
    if [ -z "$expected" ]; then
      log_warn "No checksum found for ${archive_file}, skipping verification"
    else
      local actual
      if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "${tmpdir}/${archive_file}" | awk '{print $1}')
      elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "${tmpdir}/${archive_file}" | awk '{print $1}')
      else
        log_warn "No sha256sum or shasum available, skipping verification"
        actual=""
      fi

      if [ -n "$actual" ] && [ "$expected" != "$actual" ]; then
        log_error "Checksum mismatch!"
        log_error "  Expected: $expected"
        log_error "  Actual:   $actual"
        exit 1
      fi
      if [ -n "$actual" ]; then
        log_info "Checksum verified"
      fi
    fi
  else
    log_warn "No checksums file available, skipping verification"
  fi

  # Extract
  tar xzf "${tmpdir}/${archive_file}" -C "${tmpdir}"
}

# --- Install functions ---

install_binaries() {
  local tmpdir="$1"
  shift
  local binaries=("$@")

  for bin in "${binaries[@]}"; do
    if [ -f "${tmpdir}/${bin}" ]; then
      cp "${tmpdir}/${bin}" "${INSTALL_DIR}/${bin}"
      chmod 755 "${INSTALL_DIR}/${bin}"
      log_info "Installed ${INSTALL_DIR}/${bin}"
    else
      log_warn "Binary ${bin} not found in archive"
    fi
  done
}

create_system_user() {
  if [ "$PLATFORM" != "linux" ]; then return; fi

  if id "$SERVICE_NAME" >/dev/null 2>&1; then
    log_info "User '${SERVICE_NAME}' already exists"
    return
  fi

  log_info "Creating system user '${SERVICE_NAME}'..."
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_NAME"
}

setup_config_dir() {
  mkdir -p "$CONFIG_DIR"
  if [ "$PLATFORM" = "linux" ]; then
    chown root:root "$CONFIG_DIR"
    chmod 750 "$CONFIG_DIR"
  fi
  log_info "Config directory: ${CONFIG_DIR}"
}

setup_content_dir() {
  local root="$1"
  mkdir -p "$root"
  if [ "$PLATFORM" = "linux" ]; then
    chown "$SERVICE_NAME":"$SERVICE_NAME" "$root"
  fi
  log_info "Content directory: ${root}"
}

generate_token() {
  local tokens_file="$1"
  local token

  log_step "Generating auth token"
  token=$("${INSTALL_DIR}/demarkus-token" generate -paths "/*" -ops publish -tokens "$tokens_file")

  if [ "$PLATFORM" = "linux" ]; then
    chown root:"$SERVICE_NAME" "$tokens_file"
    chmod 640 "$tokens_file"
  fi

  echo "$token"
}

# --- TLS setup ---

setup_tls() {
  local domain="$1"

  log_step "Setting up TLS with Let's Encrypt"

  # Install certbot if not present
  if ! command -v certbot >/dev/null 2>&1; then
    log_info "Installing certbot..."
    if command -v apt-get >/dev/null 2>&1; then
      apt-get update -qq && apt-get install -y -qq certbot >/dev/null
    elif command -v dnf >/dev/null 2>&1; then
      dnf install -y -q certbot >/dev/null
    elif command -v yum >/dev/null 2>&1; then
      yum install -y -q certbot >/dev/null
    else
      log_error "Could not install certbot. Install it manually and re-run."
      exit 1
    fi
  fi

  log_info "Obtaining certificate for ${domain}..."
  log_info "This requires port 80 to be temporarily available."

  certbot certonly --standalone -d "$domain" --non-interactive --agree-tos \
    --register-unsafely-without-email || {
    log_error "certbot failed. Make sure port 80 is open and the domain points to this server."
    exit 1
  }

  fix_cert_permissions "$domain"

  # Set up auto-renewal with SIGHUP reload
  setup_cert_renewal "$domain"

  log_info "TLS configured for ${domain}"
}

fix_cert_permissions() {
  local domain="$1"

  if [ "$PLATFORM" != "linux" ]; then return; fi
  if [ ! -d "/etc/letsencrypt/live/${domain}" ]; then return; fi

  chmod 750 /etc/letsencrypt/live /etc/letsencrypt/archive
  chgrp "$SERVICE_NAME" /etc/letsencrypt/live /etc/letsencrypt/archive
  log_info "Certificate permissions updated for ${SERVICE_NAME} user"
}

setup_cert_renewal() {
  local domain="$1"

  if [ "$PLATFORM" != "linux" ]; then return; fi

  local hook='pidof demarkus-server | xargs -r kill -HUP'
  local cron_line="0 */12 * * * certbot renew --quiet --deploy-hook \"${hook}\""

  # Add to root crontab if not already present
  if ! (crontab -l 2>/dev/null | grep -q "certbot renew.*demarkus"); then
    (crontab -l 2>/dev/null; echo "$cron_line") | crontab -
    log_info "Added certbot renewal cron job (twice daily, zero-downtime reload)"
  else
    log_info "Certbot renewal cron already configured"
  fi
}

# --- Service management ---

setup_systemd() {
  local content_root="$1"
  local tokens_file="$2"
  local domain="${3:-}"

  log_step "Setting up systemd service"

  local env_lines="Environment=DEMARKUS_ROOT=${content_root}
Environment=DEMARKUS_TOKENS=${tokens_file}"

  if [ -n "$domain" ]; then
    env_lines="${env_lines}
Environment=DEMARKUS_TLS_CERT=/etc/letsencrypt/live/${domain}/fullchain.pem
Environment=DEMARKUS_TLS_KEY=/etc/letsencrypt/live/${domain}/privkey.pem"
  fi

  cat > /etc/systemd/system/demarkus.service << EOF
[Unit]
Description=Demarkus Mark Protocol Server
After=network.target

[Service]
Type=simple
User=${SERVICE_NAME}
ExecStart=${INSTALL_DIR}/demarkus-server
${env_lines}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable demarkus
  systemctl start demarkus
  log_info "Service started and enabled"
}

setup_launchd() {
  local content_root="$1"
  local tokens_file="$2"

  log_step "Setting up launchd service"

  local plist_dir="$HOME/Library/LaunchAgents"
  local plist_file="${plist_dir}/io.latebit.demarkus.plist"
  local log_dir="$HOME/.demarkus/logs"
  mkdir -p "$plist_dir" "$log_dir"

  cat > "$plist_file" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>io.latebit.demarkus</string>
  <key>ProgramArguments</key>
  <array>
    <string>${INSTALL_DIR}/demarkus-server</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>DEMARKUS_ROOT</key>
    <string>${content_root}</string>
    <key>DEMARKUS_TOKENS</key>
    <string>${tokens_file}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${log_dir}/demarkus.log</string>
  <key>StandardErrorPath</key>
  <string>${log_dir}/demarkus.err</string>
</dict>
</plist>
EOF

  launchctl load "$plist_file" 2>/dev/null || true
  log_info "Service loaded (logs: ${log_dir}/)"
}

# --- Migration ---

migrate() {
  local from="$1"
  local to="$2"

  if [ "$from" = "$to" ]; then return; fi

  log_step "Running migrations from v${from} to v${to}"

  # Migration blocks go here as the project evolves.
  # Each block checks: version_lt "$from" "X.Y.Z" && version_gte "$to" "X.Y.Z"
  #
  # Example (not active yet):
  # if version_lt "$from" "0.6.0" && version_gte "$to" "0.6.0"; then
  #   log_info "Migrating tokens.toml format for v0.6.0..."
  #   # migration code
  # fi

  log_info "Migrations complete"
}

# --- Main flows ---

do_install() {
  local domain=""
  local content_root=""
  local version=""
  local client_only=false
  local no_tls=false

  # Parse flags
  while [ $# -gt 0 ]; do
    case "$1" in
      --domain)     domain="$2"; shift 2 ;;
      --root)       content_root="$2"; shift 2 ;;
      --version)    version="$2"; shift 2 ;;
      --client-only) client_only=true; shift ;;
      --no-tls)     no_tls=true; shift ;;
      *)            log_error "Unknown option: $1"; exit 1 ;;
    esac
  done

  detect_platform

  # Client-only mode
  if [ "$client_only" = true ]; then
    do_install_client "$version"
    return
  fi

  # Server install requires elevated privileges on Linux
  if [ "$PLATFORM" = "linux" ] && [ "$(id -u)" -ne 0 ]; then
    log_error "Server install requires root. Run with sudo or as root."
    exit 1
  fi

  # Interactive prompts for missing values
  if [ -z "$content_root" ]; then
    printf "Content directory [${DEFAULT_ROOT}]: "
    read -r content_root
    content_root="${content_root:-$DEFAULT_ROOT}"
  fi

  if [ -z "$domain" ] && [ "$no_tls" = false ]; then
    printf "Domain name (leave empty to skip TLS): "
    read -r domain
  fi

  # Fetch latest version if not specified
  if [ -z "$version" ]; then
    log_info "Fetching latest server version..."
    version=$(fetch_latest_version "server")
    if [ -z "$version" ]; then
      log_error "Could not determine latest version. Use --version to specify."
      exit 1
    fi
  fi

  log_step "Installing Demarkus server v${version}"

  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  # Download and install server binaries
  download_and_verify "server" "$version" "$tmpdir"
  install_binaries "$tmpdir" "demarkus-server" "demarkus-token"

  # Download and install client binaries (separate archives)
  local client_version
  client_version=$(fetch_latest_version "client")
  if [ -n "$client_version" ]; then
    download_and_verify "client" "$client_version" "$tmpdir"
    install_binaries "$tmpdir" "demarkus"
    download_and_verify_asset "demarkus-tui" "$client_version" "client" "$tmpdir"
    install_binaries "$tmpdir" "demarkus-tui"
  else
    log_warn "Could not find client release, skipping client install"
  fi

  # Detect existing installation
  local is_reinstall=false
  if [ -f "${CONFIG_DIR}/version" ]; then
    is_reinstall=true
    local existing_version
    existing_version=$(cat "${CONFIG_DIR}/version")
    log_info "Existing installation detected (v${existing_version})"
  fi

  # Create user before directories (chown needs the user to exist)
  create_system_user

  # Set up directories and config
  setup_config_dir
  setup_content_dir "$content_root"

  local tokens_file="${CONFIG_DIR}/tokens.toml"

  # Generate initial token only on fresh install
  local raw_token=""
  if [ "$is_reinstall" = true ] && [ -f "$tokens_file" ]; then
    log_info "Keeping existing tokens file: ${tokens_file}"
  else
    raw_token=$(generate_token "$tokens_file")
  fi

  # TLS setup
  if [ -n "$domain" ] && [ "$no_tls" = false ]; then
    if [ -d "/etc/letsencrypt/live/${domain}" ]; then
      log_info "TLS certificates already exist for ${domain}, skipping cert setup"
    else
      setup_tls "$domain"
    fi
    # Always ensure the demarkus user can read the certs
    fix_cert_permissions "$domain"
  fi

  # Open firewall (Linux, if ufw is available)
  if [ "$PLATFORM" = "linux" ] && command -v ufw >/dev/null 2>&1; then
    ufw allow 6309/udp >/dev/null 2>&1 || true
    log_info "Opened UDP port 6309 in firewall"
  fi

  # Set up service only on fresh install
  if [ "$is_reinstall" = true ]; then
    log_info "Keeping existing service configuration"
    # Restart to pick up new binaries
    if [ "$PLATFORM" = "linux" ]; then
      systemctl restart demarkus 2>/dev/null || log_warn "Could not restart service"
      log_info "Service restarted with new binaries"
    elif [ "$PLATFORM" = "darwin" ]; then
      local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
      if [ -f "$plist" ]; then
        launchctl unload "$plist" 2>/dev/null || true
        launchctl load "$plist" 2>/dev/null || true
        log_info "Service restarted with new binaries"
      fi
    fi
  else
    if [ "$PLATFORM" = "linux" ]; then
      setup_systemd "$content_root" "$tokens_file" "$domain"
    else
      setup_launchd "$content_root" "$tokens_file"
    fi
  fi

  # Write version marker
  echo "$version" > "${CONFIG_DIR}/version"

  # Copy this script for future updates
  local self="${BASH_SOURCE[0]:-$0}"
  if [ -f "$self" ]; then
    cp "$self" "${INSTALL_DIR}/demarkus-install"
    chmod 755 "${INSTALL_DIR}/demarkus-install"
  fi

  # Summary
  echo ""
  if [ "$is_reinstall" = true ]; then
    log_step "Reinstall complete (v${version})"
  else
    log_step "Installation complete"
  fi
  echo ""
  log_info "Server:    ${INSTALL_DIR}/demarkus-server"
  log_info "Client:    ${INSTALL_DIR}/demarkus"
  log_info "Token tool: ${INSTALL_DIR}/demarkus-token"
  log_info "Content:   ${content_root}"
  log_info "Config:    ${CONFIG_DIR}/"
  log_info "Tokens:    ${tokens_file}"
  if [ -n "$domain" ] && [ "$no_tls" = false ]; then
    log_info "TLS:       Let's Encrypt (${domain})"
  else
    log_info "TLS:       Self-signed dev certificate"
  fi

  if [ -n "$raw_token" ]; then
    echo ""
    printf "${YELLOW}Save this auth token (shown once):${NC}\n"
    echo ""
    echo "  $raw_token"
    echo ""
    log_info "Publish your first document:"
    if [ -n "$domain" ] && [ "$no_tls" = false ]; then
      echo "  demarkus -X PUBLISH -auth \$TOKEN mark://${domain}/index.md -body \"# Hello World\""
    else
      echo "  demarkus --insecure -X PUBLISH -auth \$TOKEN mark://localhost:6309/index.md -body \"# Hello World\""
    fi
  fi
  echo ""
  log_info "Generate additional tokens:"
  echo "  demarkus-token generate -paths \"/*\" -ops publish -tokens ${tokens_file}"
  echo ""
  log_info "Update later with: demarkus-install update"
}

do_install_client() {
  local version="$1"

  detect_platform

  if [ -z "$version" ]; then
    log_info "Fetching latest client version..."
    version=$(fetch_latest_version "client")
    if [ -z "$version" ]; then
      log_error "Could not determine latest version. Use --version to specify."
      exit 1
    fi
  fi

  log_step "Installing Demarkus client v${version}"

  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  download_and_verify "client" "$version" "$tmpdir"
  install_binaries "$tmpdir" "demarkus"
  download_and_verify_asset "demarkus-tui" "$version" "client" "$tmpdir"
  install_binaries "$tmpdir" "demarkus-tui"

  echo ""
  log_step "Client installed"
  echo ""
  log_info "Usage: demarkus --insecure mark://localhost:6309/index.md"
  log_info "       demarkus --insecure -X LIST mark://localhost:6309/"
}

do_update() {
  local version=""

  # Parse flags
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version="$2"; shift 2 ;;
      *)         log_error "Unknown option: $1"; exit 1 ;;
    esac
  done

  detect_platform

  # Read current version
  local version_file="${CONFIG_DIR}/version"
  local current_version=""
  if [ -f "$version_file" ]; then
    current_version=$(cat "$version_file")
  fi

  if [ -z "$current_version" ]; then
    log_error "No version marker found at ${version_file}. Is Demarkus installed?"
    log_error "Run 'install' instead of 'update'."
    exit 1
  fi

  # Fetch target version
  if [ -z "$version" ]; then
    log_info "Fetching latest server version..."
    version=$(fetch_latest_version "server")
    if [ -z "$version" ]; then
      log_error "Could not determine latest version. Use --version to specify."
      exit 1
    fi
  fi

  if [ "$current_version" = "$version" ]; then
    log_info "Already at v${version}, nothing to do."
    return
  fi

  log_step "Updating Demarkus from v${current_version} to v${version}"

  # First, update this script itself.
  # Fetch from main — migrations are always committed alongside code changes,
  # so main always has the latest migration blocks.
  log_info "Updating install script..."
  local script_url="https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh"
  local new_script
  new_script=$(curl -fsSL "${CURL_AUTH_ARGS[@]}" "$script_url" 2>/dev/null) || {
    log_warn "Could not fetch updated install script, continuing with current version"
    new_script=""
  }

  if [ -n "$new_script" ]; then
    echo "$new_script" > "${INSTALL_DIR}/demarkus-install"
    chmod 755 "${INSTALL_DIR}/demarkus-install"
    # Re-execute with the new script for migrations
    exec "${INSTALL_DIR}/demarkus-install" _do_update_inner \
      --from "$current_version" --to "$version"
  fi

  _do_update_inner --from "$current_version" --to "$version"
}

_do_update_inner() {
  local from="" to=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --from) from="$2"; shift 2 ;;
      --to)   to="$2"; shift 2 ;;
      *)      shift ;;
    esac
  done

  detect_platform

  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  # Download new server binaries
  download_and_verify "server" "$to" "$tmpdir"

  # Download new client binaries (separate archives)
  local client_version
  client_version=$(fetch_latest_version "client")
  if [ -n "$client_version" ]; then
    download_and_verify "client" "$client_version" "$tmpdir"
    download_and_verify_asset "demarkus-tui" "$client_version" "client" "$tmpdir"
  fi

  # Run migrations before replacing binaries
  migrate "$from" "$to"

  # Replace binaries
  install_binaries "$tmpdir" "demarkus-server" "demarkus-token"
  if [ -n "$client_version" ]; then
    install_binaries "$tmpdir" "demarkus"
    install_binaries "$tmpdir" "demarkus-tui"
  fi

  # Restart service
  if [ "$PLATFORM" = "linux" ]; then
    systemctl restart demarkus 2>/dev/null || log_warn "Could not restart service"
  elif [ "$PLATFORM" = "darwin" ]; then
    local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
    if [ -f "$plist" ]; then
      launchctl unload "$plist" 2>/dev/null || true
      launchctl load "$plist" 2>/dev/null || true
    fi
  fi

  # Update version marker
  echo "$to" > "${CONFIG_DIR}/version"

  log_step "Updated to v${to}"
}

do_uninstall() {
  local keep_data=false

  while [ $# -gt 0 ]; do
    case "$1" in
      --keep-data) keep_data=true; shift ;;
      *)           log_error "Unknown option: $1"; exit 1 ;;
    esac
  done

  detect_platform

  log_step "Uninstalling Demarkus"

  # Stop and disable service
  if [ "$PLATFORM" = "linux" ]; then
    systemctl stop demarkus 2>/dev/null || true
    systemctl disable demarkus 2>/dev/null || true
    rm -f /etc/systemd/system/demarkus.service
    systemctl daemon-reload 2>/dev/null || true
    log_info "Removed systemd service"
  elif [ "$PLATFORM" = "darwin" ]; then
    local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
    if [ -f "$plist" ]; then
      launchctl unload "$plist" 2>/dev/null || true
      rm -f "$plist"
      log_info "Removed launchd service"
    fi
  fi

  # Remove binaries
  for bin in demarkus-server demarkus-token demarkus demarkus-tui demarkus-install; do
    rm -f "${INSTALL_DIR}/${bin}"
  done
  log_info "Removed binaries from ${INSTALL_DIR}/"

  # Remove config
  if [ -d "$CONFIG_DIR" ]; then
    rm -rf "$CONFIG_DIR"
    log_info "Removed config directory ${CONFIG_DIR}/"
  fi

  # Remove content directory
  if [ "$keep_data" = false ]; then
    local content_root=""
    if [ "$PLATFORM" = "linux" ] && [ -f /etc/systemd/system/demarkus.service ]; then
      content_root=$(grep DEMARKUS_ROOT /etc/systemd/system/demarkus.service 2>/dev/null \
        | sed 's/.*=//' || true)
    fi
    if [ -n "$content_root" ] && [ -d "$content_root" ]; then
      printf "Remove content directory ${content_root}? [y/N]: "
      read -r confirm
      if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
        rm -rf "$content_root"
        log_info "Removed ${content_root}/"
      fi
    fi
  fi

  # Remove system user (Linux only)
  if [ "$PLATFORM" = "linux" ] && id "$SERVICE_NAME" >/dev/null 2>&1; then
    userdel "$SERVICE_NAME" 2>/dev/null || true
    log_info "Removed system user '${SERVICE_NAME}'"
  fi

  # Remove certbot renewal cron
  if [ "$PLATFORM" = "linux" ]; then
    (crontab -l 2>/dev/null | grep -v "certbot renew.*demarkus" | crontab -) 2>/dev/null || true
  fi

  log_step "Uninstall complete"
}

# --- Entry point ---

main() {
  local command="${1:-install}"

  case "$command" in
    install)
      shift 2>/dev/null || true
      do_install "$@"
      ;;
    update)
      shift
      do_update "$@"
      ;;
    uninstall)
      shift
      do_uninstall "$@"
      ;;
    _do_update_inner)
      shift
      _do_update_inner "$@"
      ;;
    --*)
      # Flags passed directly (curl | bash -s -- --domain ...)
      do_install "$@"
      ;;
    *)
      echo "Usage: demarkus-install {install|update|uninstall}"
      echo ""
      echo "Commands:"
      echo "  install     Install Demarkus server and client"
      echo "  update      Update to the latest version"
      echo "  uninstall   Remove Demarkus"
      echo ""
      echo "Install options:"
      echo "  --domain DOMAIN    Domain name for Let's Encrypt TLS"
      echo "  --root DIR         Content directory (default: platform-specific)"
      echo "  --version X.Y.Z    Install specific version (default: latest)"
      echo "  --client-only      Only install the client binary"
      echo "  --no-tls           Skip Let's Encrypt setup"
      echo ""
      echo "Update options:"
      echo "  --version X.Y.Z    Update to specific version (default: latest)"
      echo ""
      echo "Uninstall options:"
      echo "  --keep-data        Don't remove content directory"
      exit 1
      ;;
  esac
}

main "$@"
