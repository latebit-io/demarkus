#!/usr/bin/env bash
#
# Demarkus Install Script
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh | bash
#   curl -fsSL ... | bash -s -- --domain example.com --root /srv/site
#   curl -fsSL ... | bash -s -- --tls-cert /path/cert.pem --tls-key /path/key.pem
#   curl -fsSL ... | bash -s -- --client-only
#
# Single-host stack (server + broker + library on one Linux host):
#   curl -fsSL ... | sudo bash -s -- --domain example.com --with-broker --with-library \
#     --broker-public-url https://broker.example.com \
#     --broker-oidc-issuer https://accounts.google.com \
#     --broker-oidc-client-id <id> \
#     --broker-oidc-client-secret-file /root/oidc-secret
#   The broker binds loopback (:8080 OAuth/API, :8081 MCP); front the
#   public URL with a TLS reverse proxy. Omit the OIDC flags to scaffold
#   the broker config for later editing; --with-library alone serves a
#   read-only reading room: HTTPS on :443 with the world's cert when TLS
#   is configured (--domain or --tls-cert), else HTTP on :8090. Override
#   with --library-port; pin the version with --library-version.
#
# For private repos, set GITHUB_TOKEN:
#   curl -fsSL -H "Authorization: token $GITHUB_TOKEN" \
#     https://raw.githubusercontent.com/latebit-io/demarkus/main/install.sh \
#     | GITHUB_TOKEN=$GITHUB_TOKEN bash
#
# After install:
#   demarkus-install update          # update to latest version
#                                    #   --library-version pins the reading room
#   demarkus-install uninstall       # remove everything
#
set -euo pipefail

GITHUB_REPO="latebit-io/demarkus"
INSTALL_DIR="/usr/local/bin"

# If demarkus is already installed somewhere, use that directory so
# update/uninstall operate on the same location as the original install.
_detect_existing_install_dir() {
  # type -P returns only filesystem paths, never aliases or shell functions.
  # -x guard ensures the result is an actual executable before using dirname.
  local self
  self=$(type -P demarkus-install 2>/dev/null || true)
  if [ -x "$self" ]; then
    INSTALL_DIR=$(dirname "$self")
    return
  fi
  # Fall back to where the client binary lives.
  local cli
  cli=$(type -P demarkus 2>/dev/null || true)
  if [ -x "$cli" ]; then
    INSTALL_DIR=$(dirname "$cli")
  fi
}
_detect_existing_install_dir
CONFIG_DIR_LINUX="/etc/demarkus"
CONFIG_DIR_MACOS="$HOME/.demarkus"
DEFAULT_ROOT_LINUX="/var/lib/demarkus"
DEFAULT_ROOT_MACOS="$HOME/.demarkus/content"
SERVICE_NAME="demarkus"
SCRIPT_VERSION="1"
# Fixed to systemd's search path in production; the test harness redirects it
# via DEMARKUS_TEST_SYSTEMD_DIR only under DEMARKUS_INSTALL_TESTMODE=1, so a
# stray production override cannot misplace units.
SYSTEMD_DIR="/etc/systemd/system"
if [ "${DEMARKUS_INSTALL_TESTMODE:-}" = "1" ] && [ -n "${DEMARKUS_TEST_SYSTEMD_DIR:-}" ]; then
  SYSTEMD_DIR="$DEMARKUS_TEST_SYSTEMD_DIR"
fi
# Touched only by the hairpin self-dial fallback (ensure_self_dial).
HOSTS_FILE="/etc/hosts"

# Global state
SUDO=""       # set to "sudo" if needed for INSTALL_DIR writes
_TMPDIR=""    # global tmpdir for EXIT trap
trap '[ -n "$_TMPDIR" ] && rm -rf "$_TMPDIR"' EXIT

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

# validate_port aborts unless $2 is an integer in [1,65535]; $1 names the
# source for the error message (a flag or a config file).
validate_port() {
  local source="$1"
  local port="$2"
  case "$port" in
    ''|*[!0-9]*)
      log_error "${source} must be a number (1-65535), got '${port}'"
      exit 1 ;;
  esac
  if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    log_error "${source} must be between 1 and 65535, got '${port}'"
    exit 1
  fi
}

# --- Install permissions ---

_acquire_sudo() {
  # Set SUDO="sudo" if we can get authorization. Works in curl|bash pipes because
  # sudo opens /dev/tty directly. Returns 0 on success, 1 if unavailable/denied.
  if ! command -v sudo >/dev/null 2>&1; then
    return 1
  fi
  if sudo -n true 2>/dev/null || { ( : </dev/tty ) 2>/dev/null && sudo true </dev/tty 2>/dev/tty; }; then
    SUDO="sudo"
    return 0
  fi
  return 1
}

check_install_permissions() {
  local require=false
  [ "${1:-}" = "--require" ] && require=true

  # Running as root: all operations are already privileged, nothing to do.
  if [ "$(id -u)" -eq 0 ]; then
    mkdir -p "$INSTALL_DIR"
    return
  fi

  # On Linux, update/uninstall also touch CONFIG_DIR (/etc/demarkus) and systemd,
  # so we need sudo even when INSTALL_DIR happens to be writable by the current user.
  if [ "$require" = true ] && [ "${PLATFORM:-}" = "linux" ]; then
    if ! _acquire_sudo; then
      log_error "Cannot perform privileged operations and sudo is not available."
      log_error "Run as root or with sudo."
      exit 1
    fi
    mkdir -p "$INSTALL_DIR" 2>/dev/null || $SUDO mkdir -p "$INSTALL_DIR"
    return
  fi

  if [ -d "$INSTALL_DIR" ] && [ -w "$INSTALL_DIR" ]; then
    return
  fi

  # Use sudo if available. sudo can prompt via /dev/tty even in curl|bash pipes
  # where stdin is not a TTY. Try non-interactive first (cached creds), then
  # fall back to interactive (prompts via /dev/tty).
  if _acquire_sudo; then
    $SUDO mkdir -p "$INSTALL_DIR"
    log_info "Will use sudo to install to ${INSTALL_DIR}"
    return
  fi

  # For update/uninstall, switching directories would corrupt the existing install.
  if [ "$require" = true ]; then
    log_error "Cannot write to ${INSTALL_DIR} and sudo is not available."
    log_error "Run as root or with sudo."
    exit 1
  fi

  local user_bin="$HOME/.local/bin"
  mkdir -p "$user_bin"
  INSTALL_DIR="$user_bin"
  log_info "Installing to ${user_bin} (add to PATH if not already there)"
}

# --- Platform detection ---

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)
  IS_WSL=false
  IS_WSL2=false

  case "$OS" in
    linux)
      PLATFORM="linux"
      if grep -qi microsoft /proc/version 2>/dev/null; then
        IS_WSL=true
        # WSL2's kernel osrelease contains "WSL2"; WSL1 does not
        if grep -qi 'wsl2' /proc/sys/kernel/osrelease 2>/dev/null; then
          IS_WSL2=true
        fi
      fi
      ;;
    darwin) PLATFORM="darwin" ;;
    msys*|cygwin*|mingw*)
      log_error "Windows detected. Download binaries directly from GitHub Releases:"
      log_error "  https://github.com/${GITHUB_REPO}/releases"
      log_error "Or use WSL2 — see: https://latebit-io.github.io/demarkus/install/windows/"
      exit 1
      ;;
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
  set +u
  curl -fsSL "${CURL_AUTH_ARGS[@]}" "$url" 2>/dev/null
  set -u
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
  local archive_file="${archive_name}.tar.gz"
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
  local archive_file="${archive_name}.tar.gz"
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

# install_binary_atomic replaces dest by rename, never opening it for write:
# a running executable rejects that with ETXTBSY ("Text file busy"). The stage
# is per-process and beside dest, so the rename is atomic on one filesystem.
install_binary_atomic() {
  local src="$1" dest="$2"
  local stage="${dest}.new.$$"

  $SUDO install -m 755 "$src" "$stage" || return 1
  $SUDO mv -f "$stage" "$dest" || { $SUDO rm -f "$stage"; return 1; }
}

install_binaries() {
  local tmpdir="$1"
  shift
  local binaries=("$@")

  for bin in "${binaries[@]}"; do
    if [ -f "${tmpdir}/${bin}" ]; then
      $SUDO cp "${tmpdir}/${bin}" "${INSTALL_DIR}/${bin}"
      $SUDO chmod 755 "${INSTALL_DIR}/${bin}"
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
    chown root:"$SERVICE_NAME" "$CONFIG_DIR"
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

  log_step "Generating auth token" >&2
  local token_stderr
  token_stderr=$(mktemp)
  token=$("${INSTALL_DIR}/demarkus-token" generate -label "install" -paths "/**" -ops publish -tokens "$tokens_file" 2>"$token_stderr") || {
    log_error "Failed to generate auth token"
    cat "$token_stderr" >&2
    rm -f "$token_stderr"
    exit 1
  }
  rm -f "$token_stderr"

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

  # Renewal setup belongs to the caller: this function is skipped entirely when
  # the cert already exists, so wiring it here left re-runs with no reload hook.

  log_info "TLS configured for ${domain}"
}

fix_cert_permissions() {
  local domain="$1"

  if [ "$PLATFORM" != "linux" ]; then return; fi
  if [ ! -d "/etc/letsencrypt/live/${domain}" ]; then return; fi

  chmod 750 /etc/letsencrypt/live /etc/letsencrypt/archive
  chgrp "$SERVICE_NAME" /etc/letsencrypt/live /etc/letsencrypt/archive
  # The private key files in archive/ also need to be group-readable
  if [ -d "/etc/letsencrypt/archive/${domain}" ]; then
    chgrp -R "$SERVICE_NAME" "/etc/letsencrypt/archive/${domain}"
    chmod 640 "/etc/letsencrypt/archive/${domain}"/privkey*.pem
  fi
  log_info "Certificate permissions updated for ${SERVICE_NAME} user"
}

CERT_DEPLOY_HOOK="/etc/letsencrypt/renewal-hooks/deploy/demarkus-reload.sh"
CERT_CRON_MARKER="# demarkus-managed-renewal"

# cert_cron_pattern matches only cron entries this installer wrote: the marker,
# or the exact unmarked line older versions produced (setup_cert_renewal
# migrates those to the marker). A bare "certbot renew.*demarkus" would also
# match a user's own job that reloads demarkus.
cert_cron_pattern() {
  printf '%s|certbot renew --quiet --deploy-hook "pidof demarkus-server' "$CERT_CRON_MARKER"
}

# read_crontab prints the root crontab. 0 = read it (may be empty), 1 = no
# crontab installed, 2 = the read failed. crontab -l exits non-zero for both of
# the latter, and treating a failed read as "empty" makes the usual
# read-filter-write pipeline install an empty table, destroying the user's jobs.
# Callers must never write back on 2.
read_crontab() {
  local out rc
  out=$(crontab -l 2>&1)
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s\n' "$out"
    return 0
  fi
  case "$out" in
    *"no crontab for"*) return 1 ;;
    *) return 2 ;;
  esac
}

# strip_cron_entries prints TABLE without demarkus-owned entries. grep exits 1
# when it filters everything out, which is success here, and would otherwise
# trip set -e; only a status above 1 is a real failure.
strip_cron_entries() {
  local table="$1" out="" rc=0

  if [ -n "$table" ]; then
    out=$(printf '%s\n' "$table" | grep -Ev "$(cert_cron_pattern)") || rc=$?
    if [ "$rc" -gt 1 ]; then
      return "$rc"
    fi
  fi
  if [ -n "$out" ]; then
    printf '%s\n' "$out"
  fi
  return 0
}

# compose_cron_table prints TABLE with demarkus-owned entries dropped and ENTRY
# appended. Filtering with grep -v '^$' instead would strip blank lines the user
# put in their own crontab.
compose_cron_table() {
  local table="$1" entry="$2" kept

  kept=$(strip_cron_entries "$table") || return 1
  if [ -n "$kept" ]; then
    printf '%s\n%s\n' "$kept" "$entry"
  else
    printf '%s\n' "$entry"
  fi
}

# write_cert_deploy_hook installs the reload command as a certbot deploy hook.
# certbot.timer renews on its own schedule and runs only this directory, so
# without the file a timer-driven renewal leaves stale certs loaded.
# Staged through a temp file in the same directory: certbot executes whatever
# is at this path, so it must never be observable half-written.
write_cert_deploy_hook() {
  local hook="$1"
  local dir tmp
  dir=$(dirname "$CERT_DEPLOY_HOOK")

  if ! mkdir -p "$dir"; then
    log_error "Could not create ${dir}; renewals will not reload the services"
    exit 1
  fi
  if ! tmp=$(mktemp "${CERT_DEPLOY_HOOK}.XXXXXX"); then
    log_error "Could not stage the certbot deploy hook in ${dir}"
    exit 1
  fi

  if ! cat > "$tmp" << EOF
#!/bin/sh
# Managed by demarkus install.sh. Reloads the certificate holders after a
# successful renewal, whichever mechanism triggered it.
${hook}
EOF
  then
    rm -f "$tmp"
    log_error "Could not write the certbot deploy hook"
    exit 1
  fi
  if ! chmod 755 "$tmp" || ! mv -f "$tmp" "$CERT_DEPLOY_HOOK"; then
    rm -f "$tmp"
    log_error "Could not install the certbot deploy hook at ${CERT_DEPLOY_HOOK}"
    exit 1
  fi
}

# clear_cert_renewal drops demarkus-managed renewal wiring when this host is no
# longer Let's Encrypt backed, so a switch to custom certs or --no-tls does not
# leave a cron renewing and reloading against a cert nothing uses. Only our own
# cron line and hook path are touched; user-managed certbot config is left.
# Returns nonzero on a failed teardown but is deliberately not fatal: it runs
# after cert material is written, so aborting would leave the unit
# unregenerated — worse than a leftover entry renewing an unused cert.
clear_cert_renewal() {
  if [ "$PLATFORM" != "linux" ]; then return 0; fi

  local cleared=false failed=0
  if [ -f "$CERT_DEPLOY_HOOK" ]; then
    if rm -f "$CERT_DEPLOY_HOOK"; then
      cleared=true
    else
      log_warn "Could not remove the stale deploy hook ${CERT_DEPLOY_HOOK}"
      failed=1
    fi
  fi
  # Capture guarded with ||: set -e aborts on a bare assignment from a command
  # substitution that exits nonzero, and rc 1 (no crontab) is a normal state.
  local table rc=0
  table=$(read_crontab) || rc=$?
  if [ "$rc" -eq 2 ]; then
    # Leaving a stale entry is recoverable; rewriting from an unread table is not.
    log_warn "Could not read the root crontab; leaving any stale renewal entry in place"
    failed=1
  elif [ "$rc" -eq 0 ] && printf '%s\n' "$table" | grep -Eq "$(cert_cron_pattern)"; then
    if strip_cron_entries "$table" | crontab -; then
      cleared=true
    else
      log_warn "Could not drop the stale certbot renewal cron entry"
      failed=1
    fi
  fi
  if [ "$cleared" = true ]; then
    log_info "Removed demarkus certbot renewal wiring (no longer using Let's Encrypt)"
  fi
  return "$failed"
}

# report_stale_renewal names what to clean by hand after a failed teardown.
report_stale_renewal() {
  log_warn "Stale demarkus renewal wiring may remain; check the root crontab for '${CERT_CRON_MARKER}' and remove ${CERT_DEPLOY_HOOK}"
}

setup_cert_renewal() {
  local domain="$1"

  if [ "$PLATFORM" != "linux" ]; then return; fi

  local hook='pidof demarkus-server | xargs -r kill -HUP'
  local cron_line="0 */12 * * * certbot renew --quiet --deploy-hook \"${hook}\" ${CERT_CRON_MARKER}"

  # Never downgrade the library-aware hook back to the server-only one.
  if [ ! -f "$CERT_DEPLOY_HOOK" ] || ! grep -q "demarkus-library" "$CERT_DEPLOY_HOOK"; then
    write_cert_deploy_hook "$hook"
  fi

  # Guarded with ||: set -e aborts a bare assignment whose command substitution
  # exits nonzero, and rc 1 (no root crontab yet) is the normal fresh-host case.
  local table rc=0
  table=$(read_crontab) || rc=$?
  if [ "$rc" -eq 2 ]; then
    log_error "Could not read the root crontab; refusing to rewrite it. Renewal will not reload the services"
    exit 1
  fi
  if [ "$rc" -eq 1 ]; then table=""; fi

  if printf '%s\n' "$table" | grep -q "$CERT_CRON_MARKER"; then
    log_info "Certbot renewal cron already configured"
    return
  fi

  # An unmarked entry from an older install is replaced, not duplicated, so
  # hosts converge on the marker and stop relying on command matching.
  local verb="Added"
  if printf '%s\n' "$table" | grep -Eq "$(cert_cron_pattern)"; then
    verb="Migrated"
  fi
  if ! compose_cron_table "$table" "$cron_line" | crontab -; then
    log_error "Could not write the certbot renewal cron entry"
    exit 1
  fi
  log_info "${verb} certbot renewal cron job (twice daily, zero-downtime reload)"
}

# setup_library_cert_renewal extends the renewal deploy-hook so certbot also
# restarts the library serving the same cert (the server reloads on SIGHUP;
# the library needs a restart). Replaces the server-only cron line.
setup_library_cert_renewal() {
  if [ "$PLATFORM" != "linux" ]; then return; fi

  local hook='pidof demarkus-server | xargs -r kill -HUP; systemctl try-restart demarkus-library'

  # The deploy hook is rewritten even when the cron is already current: the
  # server-only install may have written the server-only version of it.
  write_cert_deploy_hook "$hook"

  local table rc=0
  table=$(read_crontab) || rc=$?
  if [ "$rc" -eq 2 ]; then
    log_error "Could not read the root crontab; refusing to rewrite it. Renewal will not restart the library"
    exit 1
  fi
  if [ "$rc" -eq 1 ]; then table=""; fi

  # Ours only: a user job mentioning the library must not suppress our entry.
  if printf '%s\n' "$table" | grep -F "$CERT_CRON_MARKER" | grep -q "try-restart demarkus-library"; then
    log_info "Certbot renewal cron already restarts the library"
    return
  fi

  local cron_line="0 */12 * * * certbot renew --quiet --deploy-hook \"${hook}\" ${CERT_CRON_MARKER}"
  if ! compose_cron_table "$table" "$cron_line" | crontab -; then
    log_error "Could not update the certbot renewal cron for the library"
    exit 1
  fi
  log_info "Certbot renewal cron now reloads the server and restarts the library"
}

setup_custom_tls() {
  local cert_src="$1"
  local key_src="$2"

  log_step "Setting up TLS with provided certificates"

  local tls_dir="${CONFIG_DIR}/tls"
  mkdir -p "$tls_dir"

  cp "$cert_src" "${tls_dir}/cert.pem"
  cp "$key_src" "${tls_dir}/key.pem"

  if [ "$PLATFORM" = "linux" ]; then
    chown root:"$SERVICE_NAME" "$tls_dir" "${tls_dir}/cert.pem" "${tls_dir}/key.pem"
    chmod 750 "$tls_dir"
    chmod 640 "${tls_dir}/cert.pem" "${tls_dir}/key.pem"
  else
    chmod 700 "$tls_dir"
    chmod 600 "${tls_dir}/cert.pem" "${tls_dir}/key.pem"
  fi

  log_info "Certificates installed to ${tls_dir}/"
}

# --- System tuning ---

setup_udp_buffers() {
  if [ "$PLATFORM" != "linux" ]; then return; fi

  local current
  current=$(sysctl -n net.core.rmem_max 2>/dev/null || echo 0)
  if [ "$current" -ge 7500000 ]; then
    log_info "UDP buffer sizes already configured"
    return
  fi

  log_info "Configuring UDP buffer sizes for QUIC..."
  sysctl -w net.core.rmem_max=7500000 >/dev/null 2>&1 || true
  sysctl -w net.core.wmem_max=7500000 >/dev/null 2>&1 || true

  # Persist across reboots
  local conf="/etc/sysctl.d/99-demarkus-quic.conf"
  cat > "$conf" << 'EOF'
net.core.rmem_max=7500000
net.core.wmem_max=7500000
EOF
  log_info "UDP buffer sizes set (persistent via ${conf})"
}

# --- Health check ---

verify_service_running() {
  if [ "$PLATFORM" != "linux" ]; then return; fi

  log_step "Verifying service is running"
  sleep 2

  if systemctl is-active --quiet demarkus 2>/dev/null; then
    log_info "Service is running"
  else
    log_error "Service failed to start"
    echo ""
    log_info "Recent logs:"
    journalctl -u demarkus -n 20 --no-pager 2>/dev/null || true
    echo ""
    log_info "Check full logs with: journalctl -u demarkus --no-pager"
    log_info "Check service status with: systemctl status demarkus"
    exit 1
  fi
}

# self_dial_probe reads the world's health doc at the public domain with
# certificate verification on; one retry covers a world still settling
# after a restart.
self_dial_probe() {
  local cli="$1"
  local domain="$2"
  if timeout 15 "$cli" "mark://${domain}/health" >/dev/null 2>&1; then
    return 0
  fi
  sleep 2
  timeout 15 "$cli" "mark://${domain}/health" >/dev/null 2>&1
}

# hosts_maps_domain reports whether a non-comment HOSTS_FILE line already
# maps the domain (exact token match, not substring).
hosts_maps_domain() {
  local domain="$1"
  awk -v d="$domain" '$1 !~ /^#/ { for (i = 2; i <= NF; i++) if ($i == d) found = 1 } END { exit found ? 0 : 1 }' "$HOSTS_FILE"
}

# ensure_self_dial proves this host can read its own world at the public
# domain; home NAT often cannot loop back, stranding same-host dialers.
# On failure, pin the domain to loopback in HOSTS_FILE and re-probe.
ensure_self_dial() {
  local domain="$1"
  if [ "$PLATFORM" != "linux" ]; then return; fi

  local cli="${INSTALL_DIR}/demarkus"
  if [ ! -x "$cli" ]; then
    log_warn "Client binary not found at ${cli}; skipping the self-dial check for ${domain}"
    return
  fi
  # Fail closed: an unbounded probe could hang on the very black-holed
  # dial this check exists to detect.
  if ! command -v timeout >/dev/null 2>&1; then
    log_error "timeout (coreutils) is required for the self-dial check; install it and re-run"
    exit 1
  fi

  log_step "Checking this host can read its own world (mark://${domain})"
  if self_dial_probe "$cli" "$domain"; then
    log_info "Self-dial OK"
    return
  fi

  if hosts_maps_domain "$domain"; then
    log_error "mark://${domain} is unreadable from this host even though ${HOSTS_FILE} already maps ${domain}."
    log_error "Check the world service (journalctl -u demarkus --no-pager), UDP 6309, and that the TLS certificate matches ${domain}"
    exit 1
  fi

  log_warn "This host cannot reach its own world at mark://${domain} (likely hairpin NAT)"
  log_info "Pinning ${domain} to loopback in ${HOSTS_FILE} so same-host components dial locally"
  # A hosts file without a trailing newline would merge the new entry into
  # its last line; normalize first.
  if [ -s "$HOSTS_FILE" ] && [ -n "$(tail -c 1 "$HOSTS_FILE")" ]; then
    if ! printf '\n' >> "$HOSTS_FILE"; then
      log_error "Could not update ${HOSTS_FILE}"
      exit 1
    fi
  fi
  if ! printf '127.0.0.1 %s # demarkus: self-dial (hairpin NAT workaround)\n' "$domain" >> "$HOSTS_FILE"; then
    log_error "Could not update ${HOSTS_FILE}"
    exit 1
  fi
  if ! self_dial_probe "$cli" "$domain"; then
    log_error "mark://${domain} is still unreadable after pinning it to loopback."
    log_error "Check the world service (journalctl -u demarkus --no-pager), UDP 6309, and that the TLS certificate matches ${domain}"
    exit 1
  fi
  log_info "Self-dial OK via loopback; certificate verification stays on"
}

# --- Service management ---

setup_systemd() {
  local content_root="$1"
  local tokens_file="$2"
  local cert_path="${3:-}"
  local key_path="${4:-}"

  log_step "Setting up systemd service"

  # Ensure content_root is absolute for both DEMARKUS_ROOT and ReadWritePaths
  content_root=$(readlink -f "$content_root" 2>/dev/null || echo "$content_root")

  local env_lines="Environment=DEMARKUS_ROOT=${content_root}
Environment=DEMARKUS_TOKENS=${tokens_file}"

  if [ -n "$cert_path" ] && [ -n "$key_path" ]; then
    env_lines="${env_lines}
Environment=DEMARKUS_TLS_CERT=${cert_path}
Environment=DEMARKUS_TLS_KEY=${key_path}"
  fi

  # ProtectHome=yes makes /home inaccessible — skip it if content root is there
  local protect_home="ProtectHome=yes"
  case "$content_root" in
    /home|/home/*|/root|/root/*|/run/user|/run/user/*) protect_home="" ;;
  esac

  cat > "${SYSTEMD_DIR}/demarkus.service" << EOF
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

# Security hardening — sandbox the server process
ProtectSystem=strict
ReadWritePaths=${content_root}
PrivateTmp=yes
NoNewPrivileges=yes
${protect_home}
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes

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
  local cert_path="${3:-}"
  local key_path="${4:-}"

  log_step "Setting up launchd service"

  local plist_dir="$HOME/Library/LaunchAgents"
  local plist_file="${plist_dir}/io.latebit.demarkus.plist"
  local log_dir="$HOME/.demarkus/logs"
  mkdir -p "$plist_dir" "$log_dir"

  local tls_env=""
  if [ -n "$cert_path" ] && [ -n "$key_path" ]; then
    tls_env="    <key>DEMARKUS_TLS_CERT</key>
    <string>${cert_path}</string>
    <key>DEMARKUS_TLS_KEY</key>
    <string>${key_path}</string>"
  fi

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
${tls_env}
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

  # macOS 14+ uses bootstrap/bootout; older macOS uses load/unload
  local macos_major
  macos_major=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)
  if [ "${macos_major:-0}" -ge 14 ] 2>/dev/null; then
    launchctl bootstrap "gui/$(id -u)" "$plist_file" 2>/dev/null || true
  else
    launchctl load "$plist_file" 2>/dev/null || true
  fi
  log_info "Service loaded (logs: ${log_dir}/)"
}

# --- Existing config detection ---

# Read values from an existing systemd unit or launchd plist.
# Sets EXISTING_ROOT and EXISTING_DOMAIN if found.
read_existing_config() {
  EXISTING_ROOT=""
  EXISTING_DOMAIN=""
  EXISTING_TLS_CERT=""
  EXISTING_TLS_KEY=""

  if [ "$PLATFORM" = "linux" ] && [ -f "${SYSTEMD_DIR}/demarkus.service" ]; then
    EXISTING_ROOT=$(grep 'DEMARKUS_ROOT=' "${SYSTEMD_DIR}/demarkus.service" 2>/dev/null \
      | sed 's/.*DEMARKUS_ROOT=//' || true)
    local cert_path
    cert_path=$(grep 'DEMARKUS_TLS_CERT=' "${SYSTEMD_DIR}/demarkus.service" 2>/dev/null \
      | sed 's/.*DEMARKUS_TLS_CERT=//' || true)
    if [ -n "$cert_path" ]; then
      case "$cert_path" in
        */letsencrypt/*)
          EXISTING_DOMAIN=$(echo "$cert_path" | sed 's|.*/live/\([^/]*\)/.*|\1|')
          ;;
        *)
          EXISTING_TLS_CERT="$cert_path"
          EXISTING_TLS_KEY=$(grep 'DEMARKUS_TLS_KEY=' "${SYSTEMD_DIR}/demarkus.service" 2>/dev/null \
            | sed 's/.*DEMARKUS_TLS_KEY=//' || true)
          ;;
      esac
    fi
  elif [ "$PLATFORM" = "darwin" ]; then
    local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
    if [ -f "$plist" ]; then
      EXISTING_ROOT=$(grep -A1 'DEMARKUS_ROOT' "$plist" 2>/dev/null \
        | grep '<string>' | sed 's/.*<string>\(.*\)<\/string>.*/\1/' || true)
      EXISTING_TLS_CERT=$(grep -A1 'DEMARKUS_TLS_CERT' "$plist" 2>/dev/null \
        | grep '<string>' | sed 's/.*<string>\(.*\)<\/string>.*/\1/' || true)
      EXISTING_TLS_KEY=$(grep -A1 'DEMARKUS_TLS_KEY' "$plist" 2>/dev/null \
        | grep '<string>' | sed 's/.*<string>\(.*\)<\/string>.*/\1/' || true)
    fi
  fi
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

# --- single-host stack: broker + library beside the server ------------------

BROKER_SERVICE="demarkus-broker"
BROKER_CONFIG_DIR="/etc/demarkus-broker"
BROKER_STATE_DIR="/var/lib/demarkus-broker"
LIBRARY_SERVICE="demarkus-library"
LIBRARY_CONFIG_DIR="/etc/demarkus-library"
LIBRARY_REPO="latebit-io/demarkus-library"

# install_broker deploys demarkus-broker in file-backend (single-host) mode:
# the broker owns tokens.toml writes, the server keeps its read+hot-reload
# contract, and every credential the broker persists lives in BROKER_STATE_DIR.
install_broker() {
  local tools_version="$1"
  local tokens_file="$2"
  local domain="$3"
  local no_tls="$4"
  local oidc_issuer="$5"
  local oidc_client_id="$6"
  local oidc_client_secret="$7"
  local public_url="$8"
  local tmpdir="$9"

  if [ "$PLATFORM" != "linux" ]; then
    log_warn "--with-broker is Linux-only (systemd); skipping broker install"
    return
  fi

  log_step "Installing demarkus-broker (single-host mode)"
  download_and_verify_asset "demarkus-broker" "$tools_version" "tools" "$tmpdir"
  install_binaries "$tmpdir" "demarkus-broker"

  if ! id "$BROKER_SERVICE" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$BROKER_SERVICE"
  fi
  # The broker joins the server group so preserved group ownership on
  # tokens.toml keeps working, and owns the file so atomic replacement
  # (temp + rename in the tokens directory) needs no privilege.
  usermod -aG "$SERVICE_NAME" "$BROKER_SERVICE"
  mkdir -p "$BROKER_STATE_DIR"
  chown "$BROKER_SERVICE":"$BROKER_SERVICE" "$BROKER_STATE_DIR"
  chmod 700 "$BROKER_STATE_DIR"
  mkdir -p "$BROKER_CONFIG_DIR"
  chown root:"$BROKER_SERVICE" "$BROKER_CONFIG_DIR"
  chmod 750 "$BROKER_CONFIG_DIR"

  # Broker writes tokens.toml via temp+rename: it owns the tokens
  # subdirectory (never CONFIG_DIR itself, which holds the TLS keys);
  # the server traverses and reads via the shared group.
  chown "$BROKER_SERVICE":"$SERVICE_NAME" "$tokens_file"
  chmod 640 "$tokens_file"
  chown "$BROKER_SERVICE":"$SERVICE_NAME" "$(dirname "$tokens_file")"
  chmod 750 "$(dirname "$tokens_file")"

  # With verified TLS the dial address must match the certificate's name;
  # localhost only works when verification is skipped (self-signed).
  local world_host="localhost:6309"
  local dial_insecure="true"
  if [ -n "$domain" ] && [ "$no_tls" = false ]; then
    dial_insecure="false"
    world_host="${domain}:6309"
  fi

  local cookie_key
  cookie_key=$(head -c 32 /dev/urandom | base64)
  local signing_key
  signing_key=$(openssl ecparam -genkey -name prime256v1 -noout 2>/dev/null | openssl pkcs8 -topk8 -nocrypt 2>/dev/null)
  if [ -z "$signing_key" ]; then
    log_error "openssl is required to generate the broker signing key"
    exit 1
  fi

  local pub="${public_url:-https://EDIT-ME.example.com}"
  local issuer="${oidc_issuer:-https://accounts.google.com}"
  local client_id="${oidc_client_id:-EDIT-ME-client-id}"

  local config_kept=false
  if [ ! -f "${BROKER_CONFIG_DIR}/config.yaml" ]; then
    {
      printf 'storage:\n  backend: file\n  dir: %s\n' "$BROKER_STATE_DIR"
      # Loopback binds: both broker surfaces sit behind a TLS reverse
      # proxy (publicURL); exposing the plaintext listeners directly
      # would contradict the advertised https origin.
      printf 'server:\n  addr: "127.0.0.1:8080"\n  cookieKey: "%s"\n' "$cookie_key"
      printf '  publicURL: "%s"\n  mcp:\n    addr: "127.0.0.1:8081"\n' "$pub"
      printf 'oidc:\n  issuer: %s\n  clientID: %s\n' "$issuer" "$client_id"
      printf '  redirectURL: %s/auth/callback\n' "$pub"
      printf '  # clientSecret comes from OIDC_CLIENT_SECRET in %s/env\n' "$BROKER_CONFIG_DIR"
      printf '  brokerSigningKey: |\n'
      printf '%s\n' "$signing_key" | sed 's/^/    /'
      printf 'worldDialer:\n  insecureSkipVerify: %s\n' "$dial_insecure"
      printf 'worlds:\n  - name: soul\n'
      printf '    tokensFile: %s\n' "$tokens_file"
      printf '    internalAddress: %s\n' "$world_host"
      printf '    defaultToken:\n      paths: ["/**"]\n'
      printf '# Library SSO (register the reading room as a confidential web\n'
      printf '# client): see https://www.demarkus.io/deployment/single-host/\n'
    } > "${BROKER_CONFIG_DIR}/config.yaml"
    chown root:"$BROKER_SERVICE" "${BROKER_CONFIG_DIR}/config.yaml"
    chmod 640 "${BROKER_CONFIG_DIR}/config.yaml"
  else
    config_kept=true
    log_info "Keeping existing broker config: ${BROKER_CONFIG_DIR}/config.yaml"
    if [ -n "$oidc_issuer" ] || [ -n "$oidc_client_id" ] || [ -n "$public_url" ]; then
      log_warn "Broker OIDC flags were given but the existing config was kept; edit ${BROKER_CONFIG_DIR}/config.yaml to apply them"
    fi
    # Migrate the managed soul world's defaultToken from the single-segment /*
    # scope (fails on nested docs) to /**. Other worlds are untouched; the
    # broker restarts later in this run.
    # The scan exits 0 when the soul world carries the legacy scope, 1 when
    # not; anything else is an awk/read failure and must abort, not skip.
    local cfg="${BROKER_CONFIG_DIR}/config.yaml"
    local legacy_scope='/^  - name: /{soul=($3=="soul")} soul && prev ~ /defaultToken:[[:space:]]*$/ && $0 ~ /^      paths: \["\/\*"\]$/{found=1} {prev=$0} END{exit !found}'
    local scope_rc=0
    awk "$legacy_scope" "$cfg" || scope_rc=$?
    if [ "$scope_rc" -gt 1 ]; then
      log_error "Could not scan ${cfg} for the legacy soul token scope"
      exit 1
    fi
    if [ "$scope_rc" -eq 0 ]; then
      if ! awk '/^  - name: /{soul=($3=="soul")}
        { if (soul && prev ~ /defaultToken:[[:space:]]*$/ && $0 ~ /^      paths: \["\/\*"\]$/) $0="      paths: [\"/**\"]"
          prev=$0; print }' "$cfg" > "${cfg}.tmp" || ! mv "${cfg}.tmp" "$cfg"; then
        rm -f "${cfg}.tmp"
        log_error "Could not migrate the soul defaultToken.paths to /** in ${cfg}; edit it manually"
        exit 1
      fi
      if ! chown root:"$BROKER_SERVICE" "$cfg" || ! chmod 640 "$cfg"; then
        log_error "Could not restore ownership and permissions on ${cfg}"
        exit 1
      fi
      scope_rc=0
      awk "$legacy_scope" "$cfg" || scope_rc=$?
      if [ "$scope_rc" -ne 1 ]; then
        log_error "soul defaultToken.paths still \"/*\" (or unverifiable) after migration in ${cfg}; edit it manually"
        exit 1
      fi
      log_info "Migrated the soul world's defaultToken.paths to the recursive /** scope"
    fi
  fi

  if [ ! -f "${BROKER_CONFIG_DIR}/env" ]; then
    printf 'OIDC_CLIENT_SECRET=%s\n' "${oidc_client_secret:-EDIT-ME}" > "${BROKER_CONFIG_DIR}/env"
    chown root:"$BROKER_SERVICE" "${BROKER_CONFIG_DIR}/env"
    chmod 640 "${BROKER_CONFIG_DIR}/env"
  fi

  cat > "${SYSTEMD_DIR}/${BROKER_SERVICE}.service" << EOF
[Unit]
Description=Demarkus Broker (single-host mode)
After=network.target demarkus.service

[Service]
Type=simple
User=${BROKER_SERVICE}
ExecStart=${INSTALL_DIR}/demarkus-broker -config ${BROKER_CONFIG_DIR}/config.yaml
EnvironmentFile=${BROKER_CONFIG_DIR}/env
Restart=on-failure
RestartSec=5

# The broker is a credential-writing service: writable paths are exactly
# its state dir and the tokens directory it owns.
ProtectSystem=strict
ReadWritePaths=${BROKER_STATE_DIR} $(dirname "$tokens_file")
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

  # Enable only alongside a complete configuration: an enabled unit with
  # placeholder values would come up broken on every boot.
  if [ "$config_kept" = true ] || { [ -n "$oidc_issuer" ] && [ -n "$oidc_client_id" ] && [ -n "$oidc_client_secret" ] && [ -n "$public_url" ]; }; then
    systemctl enable "$BROKER_SERVICE"
    systemctl restart "$BROKER_SERVICE"
    log_info "Broker started (loopback :8080 OAuth/API, :8081 MCP); front publicURL with a TLS reverse proxy"
  else
    log_warn "Broker installed but NOT enabled: OIDC is not configured yet."
    log_info "Edit ${BROKER_CONFIG_DIR}/config.yaml (publicURL, oidc.clientID) and ${BROKER_CONFIG_DIR}/env (OIDC_CLIENT_SECRET), then: systemctl enable --now ${BROKER_SERVICE}"
  fi
}

# fetch_library_binary downloads, verifies and installs demarkus-library,
# leaving the resolved version in _LIBRARY_VERSION. Shared with the update path
# so install and update cannot drift apart.
fetch_library_binary() {
  local lib_version="$1"
  local tmpdir="$2"

  if [ -z "$lib_version" ]; then
    lib_version=$(curl -fsSL "https://api.github.com/repos/${LIBRARY_REPO}/releases/latest" | grep '"tag_name"' | head -1 | sed 's/.*"v\{0,1\}\([^"]*\)".*/\1/')
  fi
  if [ -z "$lib_version" ]; then
    log_error "Could not resolve the latest demarkus-library release (pin one with --library-version)"
    exit 1
  fi
  local lib_asset="demarkus-library_${lib_version}_${OS}_${GOARCH}.tar.gz"
  local lib_url="https://github.com/${LIBRARY_REPO}/releases/download/v${lib_version}/${lib_asset}"
  if ! curl -fsSL "$lib_url" -o "${tmpdir}/${lib_asset}"; then
    log_error "Could not download ${lib_asset}"
    exit 1
  fi
  if curl -fsSL "https://github.com/${LIBRARY_REPO}/releases/download/v${lib_version}/demarkus-library_checksums.txt" -o "${tmpdir}/demarkus-library_checksums.txt"; then
    (cd "$tmpdir" && grep "$lib_asset" demarkus-library_checksums.txt | sha256sum -c - >/dev/null) || {
      log_error "Checksum verification failed for ${lib_asset}"
      exit 1
    }
  else
    log_warn "Could not download library checksums; skipping verification"
  fi
  tar -xzf "${tmpdir}/${lib_asset}" -C "$tmpdir" demarkus-library
  install_binary_atomic "${tmpdir}/demarkus-library" "${INSTALL_DIR}/demarkus-library"
  _LIBRARY_VERSION="$lib_version"
}

# install_library deploys the demarkus-library reading room in direct-QUIC
# (read-only) mode against the local world. With TLS material it serves
# HTTPS itself on lib_port, reusing the world's cert (one hostname, one
# identity). Broker/SSO mode is a config change documented in the env file.
install_library() {
  local lib_version="$1"
  local domain="$2"
  local no_tls="$3"
  local tmpdir="$4"
  local lib_port="$5"
  local cert_path="$6"
  local key_path="$7"

  if [ "$PLATFORM" != "linux" ]; then
    log_warn "--with-library is Linux-only (systemd); skipping library install"
    return
  fi

  log_step "Installing demarkus-library (reading room)"

  # A kept env is authoritative: its PORT and TLS lines win over flags and
  # defaults, and the capability grant, port check, cert group access, and
  # firewall rule below must match what the service will actually run with.
  local lib_cert="$cert_path"
  local lib_key="$key_path"
  if [ -f "${LIBRARY_CONFIG_DIR}/env" ]; then
    local kept_port
    kept_port=$(sed -n 's/^PORT=//p' "${LIBRARY_CONFIG_DIR}/env" | head -1)
    if [ -n "$kept_port" ] && [ "$kept_port" != "$lib_port" ]; then
      validate_port "PORT in ${LIBRARY_CONFIG_DIR}/env" "$kept_port"
      log_info "Existing library config sets PORT=${kept_port}; edit ${LIBRARY_CONFIG_DIR}/env to change it"
      lib_port="$kept_port"
    fi
    lib_cert=$(sed -n 's/^DEMARKUS_TLS_CERT=//p' "${LIBRARY_CONFIG_DIR}/env" | head -1)
    lib_key=$(sed -n 's/^DEMARKUS_TLS_KEY=//p' "${LIBRARY_CONFIG_DIR}/env" | head -1)
  fi

  # Fail now rather than install a unit that crash-loops on a taken port.
  # Fail closed: no check means no refusal guarantee.
  if ! command -v ss >/dev/null 2>&1; then
    log_error "ss (iproute2) is required for the port availability check; install it and re-run"
    exit 1
  fi
  local ss_out
  if ! ss_out=$(ss -ltnpH "sport = :${lib_port}" 2>&1); then
    log_error "Could not check whether TCP port ${lib_port} is free (ss failed): ${ss_out}"
    exit 1
  fi
  # Exempt only this unit's own listener, matched by MainPID; a look-alike
  # process that merely shares the name must still count as a conflict.
  local unit_pid=""
  unit_pid=$(systemctl show -p MainPID --value "$LIBRARY_SERVICE" 2>/dev/null) || unit_pid=""
  local port_holder="$ss_out"
  if [ -n "$unit_pid" ] && [ "$unit_pid" != "0" ]; then
    local grep_rc=0
    port_holder=$(printf '%s\n' "$ss_out" | grep -v "pid=${unit_pid},") || grep_rc=$?
    # grep 1 = nothing left after filtering; anything higher is a real error.
    if [ "$grep_rc" -gt 1 ]; then
      log_error "Could not filter the port ${lib_port} listener list (grep exit ${grep_rc})"
      exit 1
    fi
  fi
  if [ -n "$port_holder" ]; then
    log_error "TCP port ${lib_port} is already in use:"
    log_error "  ${port_holder}"
    log_error "Pick a free port with --library-port <port>"
    exit 1
  fi

  fetch_library_binary "$lib_version" "$tmpdir"
  lib_version="$_LIBRARY_VERSION"

  if ! id "$LIBRARY_SERVICE" >/dev/null 2>&1; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "$LIBRARY_SERVICE"
  fi
  mkdir -p "$LIBRARY_CONFIG_DIR"
  chown root:"$LIBRARY_SERVICE" "$LIBRARY_CONFIG_DIR"
  chmod 750 "$LIBRARY_CONFIG_DIR"

  # Shared cert, shared group: the world's TLS files are root:demarkus 640,
  # so the library user joins the demarkus group to read them.
  if [ -n "$lib_cert" ]; then
    if ! id -nG "$LIBRARY_SERVICE" | tr ' ' '\n' | grep -qx "$SERVICE_NAME"; then
      if ! usermod -aG "$SERVICE_NAME" "$LIBRARY_SERVICE"; then
        log_error "Could not add ${LIBRARY_SERVICE} to the ${SERVICE_NAME} group for TLS cert access"
        exit 1
      fi
    fi
  fi

  local lib_host="localhost:6309"
  local lib_insecure="true"
  if [ -n "$domain" ] && [ "$no_tls" = false ]; then
    lib_host="$domain"
    lib_insecure="false"
  fi

  if [ ! -f "${LIBRARY_CONFIG_DIR}/env" ]; then
    {
      printf 'PORT=%s\n' "$lib_port"
      printf 'DEMARKUS_TRANSPORT=quic\n'
      printf 'DEMARKUS_HOST=%s\n' "$lib_host"
      printf 'DEMARKUS_INSECURE=%s\n' "$lib_insecure"
      if [ -n "$lib_cert" ]; then
        printf 'DEMARKUS_TLS_CERT=%s\n' "$lib_cert"
        printf 'DEMARKUS_TLS_KEY=%s\n' "$lib_key"
      fi
      printf '# Broker/SSO mode (per-reader login, browser editing): see\n'
      printf '# https://www.demarkus.io/deployment/single-host/\n'
    } > "${LIBRARY_CONFIG_DIR}/env"
    chown root:"$LIBRARY_SERVICE" "${LIBRARY_CONFIG_DIR}/env"
    chmod 640 "${LIBRARY_CONFIG_DIR}/env"
  else
    log_info "Keeping existing library config: ${LIBRARY_CONFIG_DIR}/env"
  fi

  # Ports below 1024 need the bind capability; the unit runs unprivileged.
  local cap_line=""
  if [ "$lib_port" -lt 1024 ]; then
    cap_line="AmbientCapabilities=CAP_NET_BIND_SERVICE"
  fi

  cat > "${SYSTEMD_DIR}/${LIBRARY_SERVICE}.service" << EOF
[Unit]
Description=Demarkus Library (reading room)
After=network.target demarkus.service

[Service]
Type=simple
User=${LIBRARY_SERVICE}
ExecStart=${INSTALL_DIR}/demarkus-library
EnvironmentFile=${LIBRARY_CONFIG_DIR}/env
Restart=on-failure
RestartSec=5
${cap_line}

ProtectSystem=strict
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

  # Let's Encrypt renewals must also restart the library holding the cert.
  case "$lib_cert" in
    /etc/letsencrypt/*) setup_library_cert_renewal ;;
  esac

  systemctl daemon-reload
  systemctl enable "$LIBRARY_SERVICE"
  systemctl restart "$LIBRARY_SERVICE"
  sleep 1
  if ! systemctl is-active --quiet "$LIBRARY_SERVICE"; then
    log_error "Library failed to start. Recent logs:"
    journalctl -u "$LIBRARY_SERVICE" -n 20 --no-pager 2>/dev/null \
      || log_warn "Could not read logs; run: journalctl -u ${LIBRARY_SERVICE} --no-pager"
    exit 1
  fi
  if command -v ufw >/dev/null 2>&1; then
    if ! ufw allow "${lib_port}/tcp" >/dev/null 2>&1; then
      log_warn "Could not open TCP ${lib_port} in ufw; the library may be unreachable until you open it"
    fi
  fi
  if [ -n "$lib_cert" ]; then
    log_info "Library started: HTTPS on :${lib_port} (reading room over ${lib_host})"
  else
    log_info "Library started on :${lib_port} (read-only reading room over ${lib_host})"
  fi
}

do_install() {
  local domain=""
  local content_root=""
  local version=""
  local client_only=false
  local no_tls=false
  local tls_cert=""
  local tls_key=""
  local with_broker=false
  local with_library=false
  local library_version=""
  local library_port=""
  local broker_public_url=""
  local broker_oidc_issuer=""
  local broker_oidc_client_id=""
  local broker_oidc_client_secret=""
  local broker_oidc_client_secret_file=""

  # Parse flags
  while [ $# -gt 0 ]; do
    case "$1" in
      --domain)      domain="$2"; shift 2 ;;
      --root)        content_root="$2"; shift 2 ;;
      --version)     version="$2"; shift 2 ;;
      --client-only) client_only=true; shift ;;
      --no-tls)      no_tls=true; shift ;;
      --tls-cert)    tls_cert="$2"; shift 2 ;;
      --tls-key)     tls_key="$2"; shift 2 ;;
      --with-broker)  with_broker=true; shift ;;
      --with-library) with_library=true; shift ;;
      --library-version)           library_version="$2"; shift 2 ;;
      --library-port)              library_port="$2"; shift 2 ;;
      --broker-public-url)         broker_public_url="$2"; shift 2 ;;
      --broker-oidc-issuer)        broker_oidc_issuer="$2"; shift 2 ;;
      --broker-oidc-client-id)     broker_oidc_client_id="$2"; shift 2 ;;
      --broker-oidc-client-secret) broker_oidc_client_secret="$2"; shift 2 ;;
      --broker-oidc-client-secret-file) broker_oidc_client_secret_file="$2"; shift 2 ;;
      *)             log_error "Unknown option: $1"; exit 1 ;;
    esac
  done

  # Prefer the file form for the OIDC secret: an argv secret is visible in
  # ps and shell history for the install's duration.
  if [ -n "$broker_oidc_client_secret_file" ]; then
    if [ -n "$broker_oidc_client_secret" ]; then
      log_error "--broker-oidc-client-secret and --broker-oidc-client-secret-file are mutually exclusive"
      exit 1
    fi
    # Require a regular file: -r alone accepts FIFOs and devices, and
    # head would then block the install waiting for input.
    if [ ! -f "$broker_oidc_client_secret_file" ] || [ ! -r "$broker_oidc_client_secret_file" ]; then
      log_error "Secret file must be a readable regular file: ${broker_oidc_client_secret_file}"
      exit 1
    fi
    # Refuse a group/world-accessible secret file: another local user could
    # read the OIDC client secret. stat is GNU (-c) with a BSD (-f) fallback.
    local secret_mode secret_owner
    secret_mode=$(stat -c '%a' "$broker_oidc_client_secret_file" 2>/dev/null \
      || stat -f '%Lp' "$broker_oidc_client_secret_file" 2>/dev/null || echo "")
    secret_owner=$(stat -c '%u' "$broker_oidc_client_secret_file" 2>/dev/null \
      || stat -f '%u' "$broker_oidc_client_secret_file" 2>/dev/null || echo "")
    if [ -z "$secret_mode" ]; then
      log_error "Could not stat secret file: ${broker_oidc_client_secret_file}"
      exit 1
    fi
    if [ "$(( 8#$secret_mode & 0077 ))" -ne 0 ]; then
      log_error "Secret file ${broker_oidc_client_secret_file} is group/world-accessible (mode ${secret_mode}); chmod 600 it first"
      exit 1
    fi
    if [ -n "$secret_owner" ] && [ "$secret_owner" != "$(id -u)" ] && [ "$secret_owner" != "0" ]; then
      log_error "Secret file ${broker_oidc_client_secret_file} must be owned by you or root (owner uid ${secret_owner})"
      exit 1
    fi
    broker_oidc_client_secret=$(head -1 "$broker_oidc_client_secret_file" | tr -d '[:space:]')
    if [ -z "$broker_oidc_client_secret" ]; then
      log_error "Secret file is empty: ${broker_oidc_client_secret_file}"
      exit 1
    fi
  fi

  if [ -n "$library_port" ]; then
    validate_port "--library-port" "$library_port"
  fi

  # Validate TLS flag combinations
  if [ -n "$tls_cert" ] || [ -n "$tls_key" ]; then
    if [ -z "$tls_cert" ] || [ -z "$tls_key" ]; then
      log_error "--tls-cert and --tls-key must be used together"
      exit 1
    fi
    if [ ! -f "$tls_cert" ]; then
      log_error "TLS certificate not found: ${tls_cert}"
      exit 1
    fi
    if [ ! -f "$tls_key" ]; then
      log_error "TLS key not found: ${tls_key}"
      exit 1
    fi
  fi

  detect_platform

  # Client-only mode — permissions are handled inside do_install_client
  if [ "$client_only" = true ]; then
    do_install_client "$version"
    return
  fi

  # WSL: server requires WSL2 with systemd. Check this BEFORE the root check
  # so WSL1 / no-systemd users aren't told to sudo-retry a run that can't succeed.
  if [ "$IS_WSL" = true ]; then
    if [ "$IS_WSL2" != true ]; then
      log_error "WSL1 detected. Demarkus server requires WSL2."
      log_error ""
      log_error "Convert your distro to WSL2 from PowerShell:"
      log_error "  wsl --list --verbose         # find your distro name"
      log_error "  wsl --set-version <distro> 2"
      log_error ""
      log_error "Then re-run this installer."
      exit 1
    fi
    if [ ! -d /run/systemd/system ]; then
      log_error "Running inside WSL2 without systemd enabled."
      log_error ""
      log_error "Enable systemd:"
      log_error "  1. Add to /etc/wsl.conf:"
      log_error "       [boot]"
      log_error "       systemd=true"
      log_error "  2. From PowerShell:  wsl --shutdown"
      log_error "  3. Re-open WSL and retry."
      log_error ""
      log_error "Or install the client only:"
      log_error "  curl -fsSL https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh | bash -s -- --client-only"
      exit 1
    fi
    log_warn "Running inside WSL2."
    log_warn "UDP port 6309 (QUIC) does not auto-forward from the Windows host."
    log_warn "For external access, see: https://latebit-io.github.io/demarkus/install/windows/"
  fi

  # Server install requires elevated privileges on Linux
  if [ "$PLATFORM" = "linux" ] && [ "$(id -u)" -ne 0 ]; then
    log_error "Server install requires root. Run with sudo or as root."
    exit 1
  fi

  check_install_permissions

  # On reinstall, recover values from existing service config
  read_existing_config
  if [ -n "$EXISTING_ROOT" ] && [ -z "$content_root" ]; then
    content_root="$EXISTING_ROOT"
    log_info "Using existing content directory: ${content_root}"
  fi
  if [ -n "$EXISTING_DOMAIN" ] && [ -z "$domain" ]; then
    domain="$EXISTING_DOMAIN"
    log_info "Using existing domain: ${domain}"
  fi

  # Interactive prompts for missing values (fresh install only)
  # When piped (curl | bash), stdin is not a terminal — use defaults silently.
  if [ -z "$content_root" ]; then
    if [ -t 0 ]; then
      printf "Content directory [${DEFAULT_ROOT}]: "
      read -r content_root
    fi
    content_root="${content_root:-$DEFAULT_ROOT}"
  fi

  if [ -z "$domain" ] && [ "$no_tls" = false ] && [ -z "$tls_cert" ]; then
    if [ -t 0 ]; then
      printf "Domain name (leave empty to skip TLS): "
      read -r domain
    fi
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

  # Detect existing installation FIRST (before stopping anything)
  local is_reinstall=false
  if [ -f "${CONFIG_DIR}/version" ]; then
    is_reinstall=true
    local existing_version
    existing_version=$(cat "${CONFIG_DIR}/version")
    log_info "Existing installation detected (v${existing_version})"
  fi

  # Stop service before binary replacement (avoids "Text file busy")
  if pgrep -x demarkus-server >/dev/null 2>&1; then
    log_info "Stopping running service before replacing binaries"
    if [ "$PLATFORM" = "linux" ]; then
      $SUDO systemctl stop demarkus 2>/dev/null || true
    elif [ "$PLATFORM" = "darwin" ]; then
      local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
      if [ -f "$plist" ]; then
        local macos_major
        macos_major=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)
        if [ "${macos_major:-0}" -ge 14 ] 2>/dev/null; then
          launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
        else
          launchctl unload "$plist" 2>/dev/null || true
        fi
      fi
    fi
    # Wait for process to exit, escalate if needed
    local wait_count=0
    while pgrep -x demarkus-server >/dev/null 2>&1 && [ $wait_count -lt 5 ]; do
      sleep 1
      wait_count=$((wait_count + 1))
    done
    # SIGTERM if still running (handles manually started processes)
    if pgrep -x demarkus-server >/dev/null 2>&1; then
      $SUDO pkill -x demarkus-server 2>/dev/null || true
      sleep 2
    fi
    # SIGKILL as last resort
    if pgrep -x demarkus-server >/dev/null 2>&1; then
      $SUDO pkill -9 -x demarkus-server 2>/dev/null || true
      sleep 1
    fi
  fi

  _TMPDIR=$(mktemp -d) || {
    log_error "Failed to create temporary directory"
    exit 1
  }

  # Download and install server binaries
  download_and_verify "server" "$version" "$_TMPDIR"
  install_binaries "$_TMPDIR" "demarkus-server"

  # Download and install client binaries (separate archives)
  local client_version
  client_version=$(fetch_latest_version "client")
  if [ -n "$client_version" ]; then
    download_and_verify "client" "$client_version" "$_TMPDIR"
    install_binaries "$_TMPDIR" "demarkus"
    download_and_verify_asset "demarkus-tui" "$client_version" "client" "$_TMPDIR"
    install_binaries "$_TMPDIR" "demarkus-tui"
    download_and_verify_asset "demarkus-mcp" "$client_version" "client" "$_TMPDIR"
    install_binaries "$_TMPDIR" "demarkus-mcp"
  else
    log_warn "Could not find client release, skipping client install"
  fi

  # Download and install admin CLIs from the tools/ release.
  # demarkus-token + demarkus-publish moved out of the server archive in §6.7.A.
  local tools_version
  tools_version=$(fetch_latest_version "tools")
  if [ -z "$tools_version" ]; then
    log_error "Could not find tools release — demarkus-token is required for token generation."
    exit 1
  fi
  # Pre-fetch the tools-release checksums file so download_and_verify_asset
  # can verify each per-binary archive against it.
  download_asset_file "tools/v${tools_version}" "demarkus-tools_checksums.txt" \
    "${_TMPDIR}/demarkus-tools_checksums.txt" 2>/dev/null \
    || log_warn "Could not download tools checksums; per-binary verification will be skipped"
  download_and_verify_asset "demarkus-token" "$tools_version" "tools" "$_TMPDIR"
  install_binaries "$_TMPDIR" "demarkus-token"
  download_and_verify_asset "demarkus-publish" "$tools_version" "tools" "$_TMPDIR"
  install_binaries "$_TMPDIR" "demarkus-publish"

  # Create user before directories (chown needs the user to exist)
  create_system_user

  # Set up directories and config
  setup_config_dir
  setup_content_dir "$content_root"

  local tokens_file="${CONFIG_DIR}/tokens.toml"
  # Broker-owned tokens subdir: the broker needs directory write for atomic
  # replacement, never on CONFIG_DIR (TLS keys live there). Sticky once
  # created; reinstalls honor it even without --with-broker (else split brain).
  if [ -f "${CONFIG_DIR}/tokens/tokens.toml" ]; then
    tokens_file="${CONFIG_DIR}/tokens/tokens.toml"
    log_info "Using broker-owned tokens layout: ${tokens_file}"
  elif [ "$with_broker" = true ] && [ "$PLATFORM" = "linux" ]; then
    local broker_tokens_dir="${CONFIG_DIR}/tokens"
    # Abort on failure: pointing the server at the new path while the old
    # file is still in place would leave it reading a missing tokens file.
    if ! mkdir -p "$broker_tokens_dir"; then
      log_error "Could not create broker tokens directory ${broker_tokens_dir}"
      exit 1
    fi
    if [ -f "$tokens_file" ] && ! mv "$tokens_file" "${broker_tokens_dir}/tokens.toml"; then
      log_error "Could not move ${tokens_file} into ${broker_tokens_dir}/"
      exit 1
    fi
    tokens_file="${broker_tokens_dir}/tokens.toml"
    log_info "Using broker-owned tokens layout: ${tokens_file}"
  fi

  local raw_token=""
  if [ -f "$tokens_file" ]; then
    log_info "Keeping existing tokens file: ${tokens_file}"
    if [ "$PLATFORM" = "linux" ]; then
      chown root:"$SERVICE_NAME" "$tokens_file"
      chmod 640 "$tokens_file"
    fi
  else
    raw_token=$(generate_token "$tokens_file")
  fi

  # TLS setup
  local tls_cert_path=""
  local tls_key_path=""

  if [ -n "$tls_cert" ] && [ -n "$tls_key" ]; then
    # User-provided certificates
    setup_custom_tls "$tls_cert" "$tls_key"
    clear_cert_renewal || report_stale_renewal
    tls_cert_path="${CONFIG_DIR}/tls/cert.pem"
    tls_key_path="${CONFIG_DIR}/tls/key.pem"
  elif [ -n "$domain" ] && [ "$no_tls" = false ]; then
    # Let's Encrypt
    if [ -d "/etc/letsencrypt/live/${domain}" ]; then
      log_info "TLS certificates already exist for ${domain}, skipping cert setup"
    else
      setup_tls "$domain"
    fi
    fix_cert_permissions "$domain"
    # Unconditional: a re-run over an existing cert must still get a hook.
    setup_cert_renewal "$domain"
    tls_cert_path="/etc/letsencrypt/live/${domain}/fullchain.pem"
    tls_key_path="/etc/letsencrypt/live/${domain}/privkey.pem"
  elif [ -d "${CONFIG_DIR}/tls" ] \
    && { [ -f "${CONFIG_DIR}/tls/cert.pem" ] || [ -f "${CONFIG_DIR}/tls/key.pem" ]; }; then
    # Existing custom certs from a previous install. Half a pair would configure
    # the unit against a missing file and crash-loop the service on start.
    if [ ! -f "${CONFIG_DIR}/tls/cert.pem" ] || [ ! -f "${CONFIG_DIR}/tls/key.pem" ]; then
      log_error "Incomplete custom TLS material in ${CONFIG_DIR}/tls/: need both cert.pem and key.pem"
      log_error "Restore the missing file, or pass --tls-cert/--tls-key, or --no-tls"
      exit 1
    fi
    log_info "Using existing custom certificates from ${CONFIG_DIR}/tls/"
    clear_cert_renewal || report_stale_renewal
    tls_cert_path="${CONFIG_DIR}/tls/cert.pem"
    tls_key_path="${CONFIG_DIR}/tls/key.pem"
  else
    # No TLS material: --no-tls, or a domainless install.
    clear_cert_renewal || report_stale_renewal
  fi

  # System tuning
  setup_udp_buffers

  # Open firewall (Linux, if ufw is available)
  if [ "$PLATFORM" = "linux" ] && command -v ufw >/dev/null 2>&1; then
    ufw allow 6309/udp >/dev/null 2>&1 || true
    log_info "Opened UDP port 6309 in firewall"
  fi

  # Always (re)generate service config to pick up new binaries, TLS paths, etc.
  if [ "$PLATFORM" = "linux" ]; then
    setup_systemd "$content_root" "$tokens_file" "$tls_cert_path" "$tls_key_path"
  else
    setup_launchd "$content_root" "$tokens_file" "$tls_cert_path" "$tls_key_path"
  fi

  # Verify the service actually started
  verify_service_running

  # Same-host dialers (broker, library) reach the world by its public name
  # with verification on; prove that works from this host before installing
  # them, and pin the name to loopback when hairpin NAT breaks it.
  if [ "$PLATFORM" = "linux" ] && [ -n "$domain" ] && [ "$no_tls" = false ] \
    && { [ "$with_broker" = true ] || [ "$with_library" = true ]; }; then
    ensure_self_dial "$domain"
  fi

  # Optional single-host stack components (after the world server is up:
  # the broker writes its tokens file, the library reads its documents).
  if [ "$with_broker" = true ]; then
    install_broker "$tools_version" "$tokens_file" "$domain" "$no_tls" \
      "$broker_oidc_issuer" "$broker_oidc_client_id" "$broker_oidc_client_secret" \
      "$broker_public_url" "$_TMPDIR"
    # Broker listeners are loopback-only; the reverse proxy's HTTPS port
    # is the operator's to open. No firewall change here.
  fi
  if [ "$with_library" = true ]; then
    # Default port: standard HTTPS when TLS material exists (the reading room
    # is browser-facing), else unprivileged 8090 plain HTTP. Firewall opening
    # happens inside install_library, which knows the effective port.
    if [ -z "$library_port" ]; then
      if [ -n "$tls_cert_path" ]; then
        library_port=443
      else
        library_port=8090
      fi
    fi
    install_library "$library_version" "$domain" "$no_tls" "$_TMPDIR" \
      "$library_port" "$tls_cert_path" "$tls_key_path"
  fi
  if [ "$PLATFORM" = "darwin" ]; then
    local log_dir="$HOME/.demarkus/logs"
    if [ -x "${INSTALL_DIR}/demarkus" ]; then
      log_info "Verifying server is running..."
      sleep 2
      if "${INSTALL_DIR}/demarkus" --insecure mark://localhost:6309/health >/dev/null 2>&1; then
        log_info "Server is running"
      else
        log_warn "Server may not be running yet. Check logs: ${log_dir}/demarkus.err"
      fi
    else
      log_warn "Client binary not found; skipping health check. Check logs: ${log_dir}/demarkus.err"
    fi
  fi

  # Write version marker
  echo "$version" > "${CONFIG_DIR}/version"

  # Copy this script for future updates
  local self="${BASH_SOURCE[0]:-$0}"
  if [ -f "$self" ]; then
    $SUDO cp "$self" "${INSTALL_DIR}/demarkus-install"
    $SUDO chmod 755 "${INSTALL_DIR}/demarkus-install"
  else
    # Running from a pipe (curl | bash) — download the script instead
    local script_url="https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh"
    local tmp_script
    tmp_script=$(mktemp)
    set +u
    if curl -fsSL "${CURL_AUTH_ARGS[@]}" "$script_url" -o "$tmp_script" 2>/dev/null; then
      set -u
      $SUDO mv "$tmp_script" "${INSTALL_DIR}/demarkus-install"
      $SUDO chmod 755 "${INSTALL_DIR}/demarkus-install"
    else
      set -u
      rm -f "$tmp_script"
      if ! [ -x "${INSTALL_DIR}/demarkus-install" ]; then
        log_warn "Could not fetch install script for demarkus-install; update/uninstall won't be available"
      fi
    fi
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
  log_info "Publish tool: ${INSTALL_DIR}/demarkus-publish"
  log_info "Content:   ${content_root}"
  log_info "Config:    ${CONFIG_DIR}/"
  log_info "Tokens:    ${tokens_file}"
  if [ -n "$tls_cert" ] && [ -n "$tls_key" ]; then
    log_info "TLS:       Custom certificates (${CONFIG_DIR}/tls/)"
  elif [ -n "$domain" ] && [ "$no_tls" = false ]; then
    log_info "TLS:       Let's Encrypt (${domain})"
  elif [ -n "$tls_cert_path" ]; then
    log_info "TLS:       Custom certificates (${CONFIG_DIR}/tls/)"
  else
    log_info "TLS:       Self-signed dev certificate"
  fi

  if [ -n "$raw_token" ]; then
    local token_file="${CONFIG_DIR}/initial-token.txt"
    echo "$raw_token" > "$token_file"
    chmod 600 "$token_file"
    if [ "$PLATFORM" = "linux" ]; then
      chown root:"$SERVICE_NAME" "$token_file"
      chmod 640 "$token_file"
    fi
    echo ""
    printf "${YELLOW}Auth token (also saved to ${token_file}):${NC}\n"
    echo ""
    echo "  $raw_token"
    echo ""
    log_info "Publish your first document:"
    if [ -n "$domain" ] && [ "$no_tls" = false ]; then
      echo "  demarkus -X PUBLISH -auth \$TOKEN -body \"# Hello World\" mark://${domain}/index.md"
    else
      echo "  demarkus --insecure -X PUBLISH -auth \$TOKEN -body \"# Hello World\" mark://localhost:6309/index.md"
    fi

    # Ready-to-paste join line: one string carries host + token.
    local join_host="$domain"
    [ -z "$join_host" ] && join_host=$(hostname 2>/dev/null || echo localhost)
    local join_url=""
    if ! join_url=$("${INSTALL_DIR}/demarkus-token" join -host "$join_host" -token "$raw_token"); then
      log_warn "Could not generate a join URL; run: demarkus-token join -host <host> -token <TOKEN>"
      join_url=""
    fi
    if [ -n "$join_url" ]; then
      echo ""
      log_info "Join this server from another machine (paste one line):"
      echo "  Claude Code (demarkus-memory plugin):  /soul-join $join_url"
      echo "  CLI:                                   demarkus join '$join_url'"
      if [ -z "$domain" ]; then
        log_warn "'$join_host' must resolve from the joining machine; if it doesn't, rebuild the line with the public address: demarkus-token join -host <public-host> -token <TOKEN>"
      fi
      if [ -z "$domain" ] || [ "$no_tls" = true ]; then
        log_info "Self-signed cert: joining clients need --insecure alongside the join URL"
      fi
    fi
  fi
  echo ""
  log_info "Generate additional tokens:"
  echo "  demarkus-token generate -paths \"/**\" -ops publish -tokens ${tokens_file}"
  echo ""
  log_info "Update later with: demarkus-install update"

  # PATH hint if we installed to a non-standard location
  if [ "$INSTALL_DIR" != "/usr/local/bin" ]; then
    echo ""
    log_warn "Binaries installed to ${INSTALL_DIR} — make sure it's in your PATH."
    if [ "$PLATFORM" = "darwin" ]; then
      echo "  Add to ~/.zshrc:  export PATH=\"${INSTALL_DIR}:\$PATH\""
    else
      echo "  Add to ~/.bashrc: export PATH=\"${INSTALL_DIR}:\$PATH\""
    fi
  fi
}

do_install_client() {
  local version="$1"

  detect_platform
  check_install_permissions

  if [ -z "$version" ]; then
    log_info "Fetching latest client version..."
    version=$(fetch_latest_version "client")
    if [ -z "$version" ]; then
      log_error "Could not determine latest version. Use --version to specify."
      exit 1
    fi
  fi

  log_step "Installing Demarkus client v${version}"

  _TMPDIR=$(mktemp -d) || {
    log_error "Failed to create temporary directory"
    exit 1
  }

  download_and_verify "client" "$version" "$_TMPDIR"
  install_binaries "$_TMPDIR" "demarkus"
  download_and_verify_asset "demarkus-tui" "$version" "client" "$_TMPDIR"
  install_binaries "$_TMPDIR" "demarkus-tui"
  download_and_verify_asset "demarkus-mcp" "$version" "client" "$_TMPDIR"
  install_binaries "$_TMPDIR" "demarkus-mcp"

  echo ""
  log_step "Client installed"
  echo ""
  log_info "Binaries: demarkus, demarkus-tui, demarkus-mcp"
  log_info "Usage: demarkus --insecure mark://localhost:6309/index.md"
  log_info "       demarkus-tui --insecure mark://localhost:6309/index.md"
  log_info "       demarkus-mcp -host mark://localhost:6309 -insecure"
}

do_update() {
  local version=""
  local library_version=""

  # Parse flags
  while [ $# -gt 0 ]; do
    case "$1" in
      --version)         version="$2"; shift 2 ;;
      --library-version) library_version="$2"; shift 2 ;;
      *)                 log_error "Unknown option: $1"; exit 1 ;;
    esac
  done

  detect_platform
  check_install_permissions --require

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
  set +u
  new_script=$(curl -fsSL "${CURL_AUTH_ARGS[@]}" "$script_url" 2>/dev/null) || {
    set -u
    log_warn "Could not fetch updated install script, continuing with current version"
    new_script=""
  }
  set -u

  if [ -n "$new_script" ]; then
    echo "$new_script" | $SUDO tee "${INSTALL_DIR}/demarkus-install" > /dev/null
    $SUDO chmod 755 "${INSTALL_DIR}/demarkus-install"
    # Re-execute with the new script for migrations
    exec "${INSTALL_DIR}/demarkus-install" _do_update_inner \
      --from "$current_version" --to "$version" --library-version "$library_version"
  fi

  _do_update_inner --from "$current_version" --to "$version" --library-version "$library_version"
}

# update_stack_component refreshes an installed single-host component in place:
# binary only, never its config or unit. Absent components are skipped, so a
# server-only install is unaffected. version empty means latest, as elsewhere.
update_stack_component() {
  local binary="$1" service="$2" version="$3" tmpdir="$4"

  if [ "$PLATFORM" != "linux" ]; then
    return
  fi
  if [ ! -x "${INSTALL_DIR}/${binary}" ] && ! $SUDO test -f "${SYSTEMD_DIR}/${service}.service"; then
    return
  fi

  log_step "Updating ${binary}"
  case "$binary" in
    demarkus-library)
      fetch_library_binary "$version" "$tmpdir"
      log_info "${binary} updated to ${_LIBRARY_VERSION}"
      ;;
    *)
      if [ -z "$version" ]; then
        log_warn "No tools release resolved; skipping ${binary} update"
        return
      fi
      download_and_verify_asset "$binary" "$version" "tools" "$tmpdir"
      install_binary_atomic "${tmpdir}/${binary}" "${INSTALL_DIR}/${binary}"
      log_info "${binary} updated to ${version}"
      ;;
  esac
  $SUDO systemctl try-restart "$service" 2>/dev/null \
    || log_warn "Could not restart ${service}; restart it by hand"
}

_do_update_inner() {
  local from="" to="" library_version=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --from)            from="$2"; shift 2 ;;
      --to)              to="$2"; shift 2 ;;
      --library-version) library_version="$2"; shift 2 ;;
      *)                 shift ;;
    esac
  done

  detect_platform
  check_install_permissions --require

  _TMPDIR=$(mktemp -d) || {
    log_error "Failed to create temporary directory"
    exit 1
  }

  # Download new server binaries
  download_and_verify "server" "$to" "$_TMPDIR"

  # Download new client binaries (separate archives)
  local client_version
  client_version=$(fetch_latest_version "client")
  if [ -n "$client_version" ]; then
    download_and_verify "client" "$client_version" "$_TMPDIR"
    download_and_verify_asset "demarkus-tui" "$client_version" "client" "$_TMPDIR"
    download_and_verify_asset "demarkus-mcp" "$client_version" "client" "$_TMPDIR"
  fi

  # Download new admin CLIs from the tools/ release.
  local tools_version
  tools_version=$(fetch_latest_version "tools")
  if [ -n "$tools_version" ]; then
    download_asset_file "tools/v${tools_version}" "demarkus-tools_checksums.txt" \
      "${_TMPDIR}/demarkus-tools_checksums.txt" 2>/dev/null \
      || log_warn "Could not download tools checksums; per-binary verification will be skipped"
    download_and_verify_asset "demarkus-token" "$tools_version" "tools" "$_TMPDIR"
    download_and_verify_asset "demarkus-publish" "$tools_version" "tools" "$_TMPDIR"
  else
    log_warn "Could not find tools release; skipping demarkus-token/demarkus-publish update"
  fi

  # Installed once by install/install-stack and never refreshed here, so a
  # single-host deployment drifted behind for as long as it ran.
  update_stack_component "demarkus-broker" "$BROKER_SERVICE" "$tools_version" "$_TMPDIR"
  update_stack_component "demarkus-library" "$LIBRARY_SERVICE" "$library_version" "$_TMPDIR"

  # Run migrations before replacing binaries
  migrate "$from" "$to"

  # Stop service before replacing binaries (avoids "Text file busy")
  if [ "$PLATFORM" = "linux" ]; then
    $SUDO systemctl stop demarkus 2>/dev/null || true
    sleep 1
    $SUDO pkill -f demarkus-server 2>/dev/null || true
    sleep 1
  elif [ "$PLATFORM" = "darwin" ]; then
    local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
    if [ -f "$plist" ]; then
      local macos_major
      macos_major=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)
      if [ "${macos_major:-0}" -ge 14 ] 2>/dev/null; then
        launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
      else
        launchctl unload "$plist" 2>/dev/null || true
      fi
    fi
    pkill -f demarkus-server 2>/dev/null || true
    sleep 1
  fi

  # Replace binaries
  install_binaries "$_TMPDIR" "demarkus-server"
  if [ -n "$client_version" ]; then
    install_binaries "$_TMPDIR" "demarkus"
    install_binaries "$_TMPDIR" "demarkus-tui"
    install_binaries "$_TMPDIR" "demarkus-mcp"
  fi
  if [ -n "$tools_version" ]; then
    install_binaries "$_TMPDIR" "demarkus-token"
    install_binaries "$_TMPDIR" "demarkus-publish"
  fi

  # Restart service
  if [ "$PLATFORM" = "linux" ]; then
    $SUDO systemctl restart demarkus 2>/dev/null || log_warn "Could not restart service"
  elif [ "$PLATFORM" = "darwin" ]; then
    local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
    if [ -f "$plist" ]; then
      local macos_major
      macos_major=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)
      if [ "${macos_major:-0}" -ge 14 ] 2>/dev/null; then
        launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
        launchctl bootstrap "gui/$(id -u)" "$plist" 2>/dev/null || true
      else
        launchctl unload "$plist" 2>/dev/null || true
        launchctl load "$plist" 2>/dev/null || true
      fi
    fi
  fi

  # Harden existing systemd unit if it lacks security directives
  if [ "$PLATFORM" = "linux" ]; then
    local unit="${SYSTEMD_DIR}/demarkus.service"
    if $SUDO test -f "$unit" && ! $SUDO grep -q 'ProtectSystem' "$unit"; then
      local content_root
      content_root=$($SUDO grep -m1 'DEMARKUS_ROOT=' "$unit" 2>/dev/null \
        | sed 's/.*DEMARKUS_ROOT=//' || true)
      content_root=$(readlink -f "$content_root" 2>/dev/null || echo "$content_root")
      if [ -n "$content_root" ]; then
        log_warn "Systemd unit is missing security hardening (ProtectSystem, NoNewPrivileges, etc.)"
        log_warn "This sandboxes the server to only write to ${content_root}"
        local apply_hardening="n"
        if [ -t 0 ] || ( : </dev/tty ) 2>/dev/null; then
          printf "Apply security hardening to systemd unit? [Y/n]: " >/dev/tty
          read -r apply_hardening </dev/tty || apply_hardening=""
          apply_hardening="${apply_hardening:-Y}"
        else
          log_info "Non-interactive mode — skipping. Add hardening manually (see https://latebit-io.github.io/demarkus/security/)."
        fi
        case "$apply_hardening" in
          [Yy]*)
            # ProtectHome=yes makes /home inaccessible — skip it if content root is there
            local protect_home="ProtectHome=yes"
            case "$content_root" in
              /home|/home/*|/root|/root/*|/run/user|/run/user/*) protect_home="" ;;
            esac
            # Back up the existing unit before modifying
            $SUDO cp "$unit" "${unit}.bak"
            # Build hardening block and insert before [Install]
            local hardening="# Security hardening — sandbox the server process
ProtectSystem=strict
ReadWritePaths=${content_root}
PrivateTmp=yes
NoNewPrivileges=yes
${protect_home}
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
"
            # Remove blank lines from conditional omission, write via temp file
            hardening=$(echo "$hardening" | grep -v '^[[:space:]]*$')
            local tmpfile
            tmpfile=$(mktemp)
            $SUDO awk -v block="$hardening" '/^\[Install\]/{print block; print ""}1' "$unit" > "$tmpfile"
            $SUDO mv "$tmpfile" "$unit"
            $SUDO systemctl daemon-reload
            $SUDO systemctl restart demarkus 2>/dev/null || true
            # Verify the service started successfully
            sleep 2
            if $SUDO systemctl is-active --quiet demarkus; then
              log_info "Systemd unit hardened — server sandboxed to ${content_root}"
              $SUDO rm -f "${unit}.bak"
            else
              log_error "Server failed to start with hardened config — rolling back"
              $SUDO mv "${unit}.bak" "$unit"
              $SUDO systemctl daemon-reload
              $SUDO systemctl restart demarkus 2>/dev/null || log_warn "Could not restart service after rollback"
            fi
            ;;
          *)
            log_info "Skipped — you can add hardening later (see https://latebit-io.github.io/demarkus/security/)"
            ;;
        esac
      fi
    fi
  fi

  # Update version marker (use sudo only when CONFIG_DIR isn't user-writable, e.g. /etc/demarkus on Linux)
  if [ -w "$CONFIG_DIR" ]; then
    echo "$to" > "${CONFIG_DIR}/version"
  else
    echo "$to" | $SUDO tee "${CONFIG_DIR}/version" > /dev/null
  fi

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
  check_install_permissions --require

  log_step "Uninstalling Demarkus"

  # Track removal failures so a partial teardown reports honestly instead of
  # logging "complete". stop/disable stay best-effort; removals are verified.
  local uninstall_errors=0
  # remove_path deletes a file or directory and confirms it is gone.
  remove_path() {
    local target="$1"
    $SUDO rm -rf "$target"
    if $SUDO test -e "$target"; then
      log_warn "Could not remove ${target}"
      uninstall_errors=$((uninstall_errors + 1))
      return 1
    fi
    return 0
  }

  # Read content root from service file before removing it
  local content_root=""
  if [ "$PLATFORM" = "linux" ] && $SUDO test -f "${SYSTEMD_DIR}/demarkus.service"; then
    content_root=$($SUDO grep DEMARKUS_ROOT "${SYSTEMD_DIR}/demarkus.service" 2>/dev/null \
      | sed 's/.*=//' || true)
  fi

  # Stop and disable service
  if [ "$PLATFORM" = "linux" ]; then
    $SUDO systemctl stop demarkus 2>/dev/null || true
    $SUDO systemctl disable demarkus 2>/dev/null || true
    remove_path "${SYSTEMD_DIR}/demarkus.service"
    # Remove the optional single-host stack components too. Each is a
    # no-op when it was never installed.
    for svc in "$BROKER_SERVICE" "$LIBRARY_SERVICE"; do
      if $SUDO test -f "${SYSTEMD_DIR}/${svc}.service"; then
        $SUDO systemctl stop "$svc" 2>/dev/null || true
        $SUDO systemctl disable "$svc" 2>/dev/null || true
        remove_path "${SYSTEMD_DIR}/${svc}.service" && log_info "Removed ${svc} service"
      fi
    done
    $SUDO systemctl daemon-reload 2>/dev/null || true
    log_info "Removed systemd service"
  elif [ "$PLATFORM" = "darwin" ]; then
    local plist="$HOME/Library/LaunchAgents/io.latebit.demarkus.plist"
    if [ -f "$plist" ]; then
      local macos_major
      macos_major=$(sw_vers -productVersion 2>/dev/null | cut -d. -f1)
      if [ "${macos_major:-0}" -ge 14 ] 2>/dev/null; then
        launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
      else
        launchctl unload "$plist" 2>/dev/null || true
      fi
      rm -f "$plist"
      log_info "Removed launchd service"
    fi
  fi

  # Remove binaries
  for bin in demarkus-server demarkus-token demarkus-publish demarkus demarkus-tui demarkus-mcp demarkus-install demarkus-broker demarkus-library; do
    remove_path "${INSTALL_DIR}/${bin}"
  done
  log_info "Removed binaries from ${INSTALL_DIR}/"

  # Remove config (server, plus broker/library when present — the broker
  # config and state hold credential-bearing artifacts, so always clear).
  for d in "$CONFIG_DIR" "$BROKER_CONFIG_DIR" "$BROKER_STATE_DIR" "$LIBRARY_CONFIG_DIR"; do
    if [ -n "$d" ] && $SUDO test -d "$d"; then
      remove_path "$d" && log_info "Removed ${d}/"
    fi
  done

  # Remove content directory
  if [ "$keep_data" = false ]; then
    if [ -n "$content_root" ] && [ -d "$content_root" ]; then
      printf "Remove content directory ${content_root}? [y/N]: "
      read -r confirm </dev/tty || confirm="N"
      if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
        $SUDO rm -rf "$content_root"
        log_info "Removed ${content_root}/"
      fi
    fi
  fi

  # Remove system users (Linux only). The broker user is removed before
  # the server user because it is a member of the server group.
  if [ "$PLATFORM" = "linux" ]; then
    for u in "$BROKER_SERVICE" "$LIBRARY_SERVICE" "$SERVICE_NAME"; do
      if id "$u" >/dev/null 2>&1; then
        if $SUDO userdel "$u" 2>/dev/null; then
          log_info "Removed system user '${u}'"
        else
          log_warn "Could not remove system user '${u}' (still referenced or logged in?)"
          uninstall_errors=$((uninstall_errors + 1))
        fi
      fi
    done
  fi

  # Remove certbot renewal cron and the deploy hook
  if [ "$PLATFORM" = "linux" ]; then
    local cron_table cron_rc=0
    cron_table=$(read_crontab) || cron_rc=$?
    if [ "$cron_rc" -eq 2 ]; then
      log_warn "Could not read the root crontab; the renewal entry may remain"
      uninstall_errors=$((uninstall_errors + 1))
    elif [ "$cron_rc" -eq 0 ] && printf '%s\n' "$cron_table" | grep -Eq "$(cert_cron_pattern)"; then
      if ! strip_cron_entries "$cron_table" | crontab -; then
        log_warn "Could not drop the certbot renewal entry from the root crontab"
        uninstall_errors=$((uninstall_errors + 1))
      fi
    fi
    remove_path "$CERT_DEPLOY_HOOK"
  fi

  if [ "$uninstall_errors" -ne 0 ]; then
    log_error "Uninstall completed with ${uninstall_errors} error(s); some artifacts remain (see warnings above)"
    exit 1
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
      echo "  --tls-cert FILE    Path to TLS certificate (PEM)"
      echo "  --tls-key FILE     Path to TLS private key (PEM)"
      echo "  --root DIR         Content directory (default: platform-specific)"
      echo "  --version X.Y.Z    Install specific version (default: latest)"
      echo "  --client-only      Only install the client binary"
      echo "  --no-tls           Skip TLS setup"
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
