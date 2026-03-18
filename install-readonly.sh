#!/usr/bin/env bash
#
# Demarkus Read-Only Server Install
#
# Installs a chrooted, read-only Demarkus server for maximum security.
# The server cannot write to the filesystem — publish content locally
# with demarkus-publish, then serve it publicly.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/latebit-io/demarkus/main/install-readonly.sh | sudo bash
#   curl -fsSL ... | sudo bash -s -- --root /srv/demarkus --domain example.com
#   curl -fsSL ... | sudo bash -s -- --tls-cert /path/cert.pem --tls-key /path/key.pem
#
# After install:
#   demarkus-publish -root /srv/demarkus/content -path /index.md -body "# Hello"
#   echo "# Hello" | demarkus-publish -root /srv/demarkus/content -path /index.md
#
set -euo pipefail

GITHUB_REPO="latebit-io/demarkus"
SERVICE_NAME="demarkus"
DEFAULT_ROOT="/srv/demarkus"

log_info()  { echo "  [info]  $*"; }
log_step()  { echo ""; echo "==> $*"; }
log_warn()  { echo "  [warn]  $*" >&2; }
log_error() { echo "  [ERROR] $*" >&2; }

# --- Platform detection ---

detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  ARCH=$(uname -m)

  case "$OS" in
    linux) PLATFORM="linux" ;;
    *)
      log_error "Read-only chroot install is only supported on Linux."
      exit 1
      ;;
  esac

  case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)
      log_error "Unsupported architecture: ${ARCH}"
      exit 1
      ;;
  esac
}

# --- GitHub release helpers ---

CURL_AUTH_ARGS=()
if [ -n "${GITHUB_TOKEN:-}" ]; then
  CURL_AUTH_ARGS=(-H "Authorization: token ${GITHUB_TOKEN}")
fi

fetch_latest_version() {
  local component="$1"
  local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases"
  local tag_prefix="${component}/v"

  curl -fsSL "${CURL_AUTH_ARGS[@]}" "$api_url" 2>/dev/null \
    | grep '"tag_name"' \
    | grep "$tag_prefix" \
    | head -1 \
    | sed 's/.*"tag_name": *"'"$tag_prefix"'\([^"]*\)".*/\1/'
}

download_binary() {
  local component="$1"
  local version="$2"
  local binary="$3"
  local dest="$4"

  local tag="${component}/v${version}"
  local archive="demarkus-${component}_${version}_linux_${ARCH}.tar.gz"
  local checksums="demarkus-${component}_checksums.txt"
  local base_url="https://github.com/${GITHUB_REPO}/releases/download/${tag}"

  log_info "Downloading ${binary} v${version}..."
  curl -fsSL "${CURL_AUTH_ARGS[@]}" -o "${dest}/${archive}" "${base_url}/${archive}"

  # Verify checksum
  if curl -fsSL "${CURL_AUTH_ARGS[@]}" -o "${dest}/${checksums}" "${base_url}/${checksums}" 2>/dev/null; then
    local expected actual
    expected=$(grep "${archive}" "${dest}/${checksums}" | awk '{print $1}')
    if [ -n "$expected" ]; then
      if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum "${dest}/${archive}" | awk '{print $1}')
      elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 "${dest}/${archive}" | awk '{print $1}')
      fi
      if [ -n "$actual" ] && [ "$expected" != "$actual" ]; then
        log_error "Checksum mismatch for ${archive}"
        log_error "  Expected: $expected"
        log_error "  Actual:   $actual"
        exit 1
      fi
      log_info "Checksum verified"
    fi
  fi

  tar -xzf "${dest}/${archive}" -C "$dest" "$binary" 2>/dev/null || \
    tar -xzf "${dest}/${archive}" -C "$dest"
}

# --- System user ---

create_system_user() {
  if id "$SERVICE_NAME" >/dev/null 2>&1; then
    return
  fi
  log_info "Creating system user: ${SERVICE_NAME}"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_NAME"
}

# --- Main install ---

do_install() {
  local root=""
  local domain=""
  local tls_cert=""
  local tls_key=""
  local version=""
  local port="6309"

  while [ $# -gt 0 ]; do
    case "$1" in
      --root)     root="$2"; shift 2 ;;
      --domain)   domain="$2"; shift 2 ;;
      --port)     port="$2"; shift 2 ;;
      --version)  version="$2"; shift 2 ;;
      --tls-cert) tls_cert="$2"; shift 2 ;;
      --tls-key)  tls_key="$2"; shift 2 ;;
      *)          log_error "Unknown option: $1"; exit 1 ;;
    esac
  done

  if [ "$(id -u)" -ne 0 ]; then
    log_error "Read-only chroot install requires root. Run with sudo."
    exit 1
  fi

  detect_platform

  root="${root:-$DEFAULT_ROOT}"
  root=$(readlink -f "$root" 2>/dev/null || echo "$root")

  # Reject dangerous root paths
  case "$root" in
    /|/usr|/etc|/var|/bin|/sbin|/lib|/boot|/root)
      log_error "Refusing dangerous --root path: ${root}"
      exit 1
      ;;
  esac

  # Validate TLS flags
  if [ -n "$tls_cert" ] || [ -n "$tls_key" ]; then
    if [ -z "$tls_cert" ] || [ -z "$tls_key" ]; then
      log_error "--tls-cert and --tls-key must be used together"
      exit 1
    fi
  fi

  # Fetch version
  if [ -z "$version" ]; then
    log_info "Fetching latest server version..."
    version=$(fetch_latest_version "server")
    if [ -z "$version" ]; then
      log_error "Could not determine latest version. Use --version to specify."
      exit 1
    fi
  fi

  log_step "Installing read-only Demarkus server v${version}"
  log_info "Chroot: ${root}"

  # Create chroot structure
  mkdir -p "${root}/bin" "${root}/content" "${root}/tls"

  # Download binaries
  local tmpdir
  tmpdir=$(mktemp -d)

  download_binary "server" "$version" "demarkus-server" "$tmpdir"
  cp "${tmpdir}/demarkus-server" "${root}/bin/demarkus-server"
  chmod 755 "${root}/bin/demarkus-server"

  # Also install demarkus-publish to /usr/local/bin (runs outside the chroot)
  download_binary "server" "$version" "demarkus-publish" "$tmpdir" 2>/dev/null || true
  if [ -f "${tmpdir}/demarkus-publish" ]; then
    cp "${tmpdir}/demarkus-publish" /usr/local/bin/demarkus-publish
    chmod 755 /usr/local/bin/demarkus-publish
    log_info "Installed demarkus-publish to /usr/local/bin/"
  else
    log_warn "demarkus-publish not found in release — build from source or publish via protocol"
  fi

  rm -rf "$tmpdir"

  # TLS setup
  if [ -n "$tls_cert" ] && [ -n "$tls_key" ]; then
    cp "$tls_cert" "${root}/tls/cert.pem"
    cp "$tls_key" "${root}/tls/key.pem"
    log_info "Copied TLS certificates into chroot"
  elif [ -n "$domain" ] && [ -d "/etc/letsencrypt/live/${domain}" ]; then
    cp "/etc/letsencrypt/live/${domain}/fullchain.pem" "${root}/tls/cert.pem"
    cp "/etc/letsencrypt/live/${domain}/privkey.pem" "${root}/tls/key.pem"
    log_info "Copied Let's Encrypt certificates into chroot"
  fi

  # Set ownership
  create_system_user
  chown -R "root:${SERVICE_NAME}" "$root"
  chmod -R o-rwx "$root"
  # Content must be readable by the service user
  chmod -R g+rX "${root}/content"
  chmod -R g+r "${root}/tls" 2>/dev/null || true

  # Build ExecStart with flags
  local exec_start="/bin/demarkus-server -read-only -root /content -port ${port}"
  if [ -f "${root}/tls/cert.pem" ]; then
    exec_start="${exec_start} -tls-cert /tls/cert.pem -tls-key /tls/key.pem"
  fi

  # Systemd unit — chrooted, fully read-only
  cat > /etc/systemd/system/demarkus.service << EOF
[Unit]
Description=Demarkus Mark Protocol Server (read-only, chrooted)
After=network.target

[Service]
Type=simple
User=${SERVICE_NAME}
RootDirectory=${root}
ExecStart=${exec_start}
Restart=on-failure
RestartSec=5

# Security hardening — maximum lockdown
ReadOnlyPaths=/
BindReadOnlyPaths=/dev/urandom
NoNewPrivileges=yes
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictNamespaces=yes
RestrictSUIDSGID=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable demarkus
  systemctl start demarkus

  # Verify
  sleep 2
  if systemctl is-active --quiet demarkus; then
    log_info "Service started successfully"
  else
    log_error "Service failed to start — check: journalctl -u demarkus"
    exit 1
  fi

  log_info "Firewall not modified — open UDP port ${port} manually if needed:"
  log_info "  sudo ufw allow ${port}/udp"

  log_step "Install complete"
  echo ""
  log_info "Chroot:       ${root}"
  log_info "Content:      ${root}/content"
  log_info "Server:       read-only, chrooted, no write access"
  log_info ""
  log_info "Publish content locally:"
  log_info "  demarkus-publish -root ${root}/content -path /index.md -body '# Hello'"
  log_info "  echo '# Hello' | demarkus-publish -root ${root}/content -path /index.md"
  if [ -f "${root}/tls/cert.pem" ]; then
    log_info ""
    log_info "TLS certificates are inside the chroot. To update them:"
    log_info "  cp /etc/letsencrypt/live/\${DOMAIN}/fullchain.pem ${root}/tls/cert.pem"
    log_info "  cp /etc/letsencrypt/live/\${DOMAIN}/privkey.pem ${root}/tls/key.pem"
    log_info "  systemctl restart demarkus"
  fi
}

do_install "$@"
