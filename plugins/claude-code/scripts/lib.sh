#!/usr/bin/env bash
# Shared helpers for the demarkus-memory plugin scripts.
# Source this file; do not execute it directly.
# shellcheck disable=SC2034  # many constants are consumed only by sourcing scripts (setup.sh, session-start.sh, mcp-wrapper.sh)

# Requires Bash 3.2+ (macOS stock bash). No Bash 4+ features are used.
if [ -n "${BASH_VERSINFO:-}" ] && [ "${BASH_VERSINFO[0]}" -lt 3 ]; then
  echo "[demarkus-memory] error: bash 3.2+ required (got ${BASH_VERSION:-unknown})" >&2
  return 1 2>/dev/null || exit 1
fi

# Paths — always the same regardless of mode.
readonly PLUGIN_HOME="${HOME}/.demarkus"
readonly PLUGIN_BIN_DIR="${PLUGIN_HOME}/bin"
readonly PLUGIN_CONFIG="${PLUGIN_HOME}/plugin-memory.conf"
readonly PLUGIN_TOKEN_FILE="${PLUGIN_HOME}/plugin-memory.token"
readonly TOKEN_LABEL="claude-code-plugin"

# Soul dirs that setup.sh may choose.
readonly SHARED_SOUL_DIR="${PLUGIN_HOME}/soul"
readonly ISOLATED_SOUL_DIR="${PLUGIN_HOME}/plugin-soul"

readonly DEMARKUS_PROTOCOL_PORT=6309   # demarkus-server's built-in default when nothing else is set
readonly DEFAULT_PORT=6310             # plugin's first-choice port for its own managed server
readonly ISOLATED_PORT_START=16310
readonly ISOLATED_PORT_END=16509

readonly SERVER_VERSION="0.17.8"
readonly CLIENT_VERSION="0.12.30"
# demarkus-token moved from the server archive to its own tools/ release
# in §6.7.A. Pin separately so a tools-only release can be picked up.
readonly TOOLS_VERSION="0.1.0"

# Sentinel file recording the SERVER/CLIENT versions of the binaries currently
# installed at PLUGIN_BIN_DIR. ensure_binaries compares this against the
# pinned versions above and re-downloads on drift, so a plugin update that
# bumps the pins propagates to existing installs on next session start.
readonly PLUGIN_VERSION_FILE="${PLUGIN_BIN_DIR}/.versions"

log()  { echo "[demarkus-memory] $*" >&2; }
warn() { echo "[demarkus-memory] warning: $*" >&2; }
die()  { echo "[demarkus-memory] error: $*" >&2; exit 1; }

# load_config — sources the config file, sets SOUL_DIR, PORT, MODE.
# Returns 1 if config missing; dies on malformed config.
load_config() {
  [[ -f "${PLUGIN_CONFIG}" ]] || return 1
  # shellcheck source=/dev/null
  . "${PLUGIN_CONFIG}"
  [[ -n "${SOUL_DIR:-}" && -n "${PORT:-}" && -n "${MODE:-}" ]] \
    || die "${PLUGIN_CONFIG} is malformed (missing SOUL_DIR/PORT/MODE)"
}

# save_config SOUL_DIR PORT MODE
save_config() {
  local soul="$1" port="$2" mode="$3"
  mkdir -p "${PLUGIN_HOME}"
  # printf '%q' produces bash-re-parseable quoted values (Bash 2+) — safer than
  # ${var@Q} which requires Bash 4.4+ (macOS stock bash is 3.2).
  local soul_q mode_q
  soul_q=$(printf '%q' "${soul}")
  mode_q=$(printf '%q' "${mode}")
  cat > "${PLUGIN_CONFIG}.tmp" <<EOF
# demarkus-memory plugin config — managed by scripts/setup.sh
SOUL_DIR=${soul_q}
PORT=${port}
MODE=${mode_q}
EOF
  mv "${PLUGIN_CONFIG}.tmp" "${PLUGIN_CONFIG}"
}

detect_platform() {
  local os arch
  case "$(uname -s)" in
    Darwin) os="darwin" ;;
    Linux)  os="linux"  ;;
    *)      die "unsupported OS: $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    arm64|aarch64) arch="arm64" ;;
    x86_64)        arch="amd64" ;;
    *)             die "unsupported arch: $(uname -m)" ;;
  esac
  echo "${os}_${arch}"
}

# sha256_verify CHECKSUMS_FILE ARCHIVE — portable across macOS/Linux.
sha256_verify() {
  local checksums_file="$1" archive="$2"
  local expected actual
  expected=$(grep " ${archive}\$" "${checksums_file}" | awk '{print $1}')
  [[ -n "${expected}" ]] || die "no checksum entry for ${archive}"
  if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${archive}" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    actual=$(shasum -a 256 "${archive}" | awk '{print $1}')
  else
    die "no sha256 tool available (install sha256sum or shasum)"
  fi
  [[ "${actual}" == "${expected}" ]] \
    || die "checksum mismatch for ${archive} (expected ${expected}, got ${actual})"
}

# _installed_versions — emits the recorded versions string (or empty).
_installed_versions() {
  [[ -f "${PLUGIN_VERSION_FILE}" ]] && cat "${PLUGIN_VERSION_FILE}" || echo ""
}

# _desired_versions — the version pin the plugin currently expects.
_desired_versions() {
  echo "server=${SERVER_VERSION},client=${CLIENT_VERSION},tools=${TOOLS_VERSION}"
}

# ensure_binaries — download + install to PLUGIN_BIN_DIR if any are missing
# or if the recorded versions don't match the pins above. The version
# sentinel lets a CLIENT_VERSION/SERVER_VERSION bump propagate to existing
# installs on the next session start; without it, ensure_binaries would
# never re-fetch and users would silently stay on an old release.
ensure_binaries() {
  if [[ -x "${PLUGIN_BIN_DIR}/demarkus-server" \
     && -x "${PLUGIN_BIN_DIR}/demarkus-mcp" \
     && -x "${PLUGIN_BIN_DIR}/demarkus-token" \
     && "$(_installed_versions)" == "$(_desired_versions)" ]]; then
    return 0
  fi

  if [[ -n "$(_installed_versions)" && "$(_installed_versions)" != "$(_desired_versions)" ]]; then
    log "binary version drift detected (installed=$(_installed_versions), desired=$(_desired_versions)); re-downloading"
  fi

  mkdir -p "${PLUGIN_BIN_DIR}"
  command -v curl >/dev/null 2>&1 || die "curl is required for first-run binary download"
  command -v tar  >/dev/null 2>&1 || die "tar is required for first-run binary download"

  local plat
  plat=$(detect_platform)

  # Download + verify + extract + install happens inside a subshell so the
  # EXIT trap cleans up the tmpdir deterministically on any failure path
  # (curl/tar/sha256 failure, die, etc.). RETURN traps can't be used here:
  # bash's default RETURN traps persist globally and would fire for every
  # subsequent function return with `tmp` out of scope.
  (
    local tmp
    tmp=$(mktemp -d)
    trap 'rm -rf "${tmp}"' EXIT

    log "downloading demarkus binaries (server v${SERVER_VERSION}, client v${CLIENT_VERSION}, tools v${TOOLS_VERSION}, ${plat})"

    local server_archive="demarkus-server_${SERVER_VERSION}_${plat}.tar.gz"
    local server_base="https://github.com/latebit-io/demarkus/releases/download/server%2Fv${SERVER_VERSION}"
    curl -fsSL -o "${tmp}/${server_archive}"    "${server_base}/${server_archive}"              || die "download ${server_archive}"
    curl -fsSL -o "${tmp}/server_checksums.txt" "${server_base}/demarkus-server_checksums.txt"  || die "download server checksums"
    (cd "${tmp}" && sha256_verify server_checksums.txt "${server_archive}")
    tar -xzf "${tmp}/${server_archive}" -C "${tmp}"

    local mcp_archive="demarkus-mcp_${CLIENT_VERSION}_${plat}.tar.gz"
    local client_base="https://github.com/latebit-io/demarkus/releases/download/client%2Fv${CLIENT_VERSION}"
    curl -fsSL -o "${tmp}/${mcp_archive}"       "${client_base}/${mcp_archive}"                 || die "download ${mcp_archive}"
    curl -fsSL -o "${tmp}/client_checksums.txt" "${client_base}/demarkus-client_checksums.txt"  || die "download client checksums"
    (cd "${tmp}" && sha256_verify client_checksums.txt "${mcp_archive}")
    tar -xzf "${tmp}/${mcp_archive}" -C "${tmp}"

    # demarkus-token ships in the tools/ release as of §6.7.A; pre-§6.7.A
    # plugin installs pulled it from the server archive.
    local token_archive="demarkus-token_${TOOLS_VERSION}_${plat}.tar.gz"
    local tools_base="https://github.com/latebit-io/demarkus/releases/download/tools%2Fv${TOOLS_VERSION}"
    curl -fsSL -o "${tmp}/${token_archive}"    "${tools_base}/${token_archive}"               || die "download ${token_archive}"
    curl -fsSL -o "${tmp}/tools_checksums.txt" "${tools_base}/demarkus-tools_checksums.txt"   || die "download tools checksums"
    (cd "${tmp}" && sha256_verify tools_checksums.txt "${token_archive}")
    tar -xzf "${tmp}/${token_archive}" -C "${tmp}"

    install -m 0755 "${tmp}/demarkus-server" "${PLUGIN_BIN_DIR}/demarkus-server"
    install -m 0755 "${tmp}/demarkus-token"  "${PLUGIN_BIN_DIR}/demarkus-token"
    install -m 0755 "${tmp}/demarkus-mcp"    "${PLUGIN_BIN_DIR}/demarkus-mcp"
  )
  # Check the subshell exit explicitly rather than relying on the caller's
  # set -e. If any download or install step failed, drop the sentinel so the
  # next ensure_binaries call retries instead of trusting partial binaries.
  local install_rc=$?
  if (( install_rc != 0 )); then
    rm -f "${PLUGIN_VERSION_FILE}"
    return "${install_rc}"
  fi

  # Record installed versions so subsequent ensure_binaries calls can detect
  # drift after a plugin update bumps SERVER_VERSION / CLIENT_VERSION.
  printf 'server=%s,client=%s\n' "${SERVER_VERSION}" "${CLIENT_VERSION}" > "${PLUGIN_VERSION_FILE}"

  log "binaries installed to ${PLUGIN_BIN_DIR}"
}

# _proc_env PID VAR — echoes the value of env var VAR for process PID, or empty.
# Portable across macOS (ps eww) and Linux (/proc/<pid>/environ).
_proc_env() {
  local pid="$1" var="$2"
  if [[ -r "/proc/${pid}/environ" ]]; then
    tr '\0' '\n' < "/proc/${pid}/environ" 2>/dev/null | awk -F= -v v="${var}" '$1==v {print substr($0, length(v)+2); exit}'
    return
  fi
  ps eww -p "${pid}" 2>/dev/null | tr ' ' '\n' | awk -F= -v v="${var}" '$1==v {print substr($0, length(v)+2); exit}'
}

# pid_of_server_at_root ROOT — emits the PID of a demarkus-server whose
# -root flag or DEMARKUS_ROOT env var equals ROOT (literal match, no regex).
# Empty output when none match. Handles paths containing regex metacharacters
# or spaces safely.
pid_of_server_at_root() {
  local target="$1"
  local pids pid args env_root
  pids=$(pgrep -f demarkus-server 2>/dev/null || true)
  [[ -z "${pids}" ]] && return 0
  while read -r pid; do
    [[ -z "${pid}" ]] && continue
    args=$(ps -p "${pid}" -o args= 2>/dev/null || true)
    # Literal substring match across both "-root TARGET" and "-root=TARGET" forms,
    # at end of args or followed by more args.
    case "${args}" in
      *" -root ${target} "* | *" -root ${target}" | \
      *" -root=${target} "* | *" -root=${target}")
        echo "${pid}"
        return 0
        ;;
    esac
    env_root=$(_proc_env "${pid}" DEMARKUS_ROOT)
    if [[ "${env_root}" == "${target}" ]]; then
      echo "${pid}"
      return 0
    fi
  done <<< "${pids}"
}

# find_running_demarkus — emits "PID PORT ROOT" per process, one per line.
# Empty output when no demarkus-server is running. Args take precedence over
# env vars; falls back to DEMARKUS_PORT/DEMARKUS_ROOT env when flags are absent.
find_running_demarkus() {
  local pids
  pids=$(pgrep -f demarkus-server 2>/dev/null || true)
  [[ -z "${pids}" ]] && return 0
  local pid args port root
  while read -r pid; do
    [[ -z "${pid}" ]] && continue
    args=$(ps -p "${pid}" -o args= 2>/dev/null || true)
    [[ -z "${args}" ]] && continue
    # Accept both "-port 6310" and "-port=6310" (Go flag package supports both).
    port=$(echo "${args}" | grep -oE -- '-port[= ]+[0-9]+' | sed -E 's/.*[= ]//')
    if [[ -z "${port}" ]]; then
      port=$(_proc_env "${pid}" DEMARKUS_PORT)
    fi
    port="${port:-${DEMARKUS_PROTOCOL_PORT}}"
    root=$(echo "${args}" | grep -oE -- '-root[= ]+[^ ]+' | sed -E 's/^-root[= ]+//')
    if [[ -z "${root}" ]]; then
      root=$(_proc_env "${pid}" DEMARKUS_ROOT)
    fi
    [[ -z "${root}" ]] && root="(unknown)"
    echo "${pid} ${port} ${root}"
  done <<< "${pids}"
}

# port_is_free PORT — true if nothing appears to own UDP PORT. Uses lsof when
# available (macOS + many Linux), falls back to ss (Linux default), and warns
# once before degrading to permissive when neither is installed. If the check
# is wrong, the caller's spawn will fail loudly via the post-spawn PID probe in
# ensure_managed_server.
port_is_free() {
  local port="$1"
  if command -v lsof >/dev/null 2>&1; then
    if lsof -iUDP:"${port}" -nP 2>/dev/null | grep -q 'UDP'; then
      return 1
    fi
    return 0
  fi
  if command -v ss >/dev/null 2>&1; then
    if ss -lunH 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$"; then
      return 1
    fi
    return 0
  fi
  if [[ -z "${_PORT_CHECK_WARNED:-}" ]]; then
    warn "no lsof or ss available; port availability checks are best-effort (server bind will fail loudly if the port is actually taken)"
    _PORT_CHECK_WARNED=1
  fi
  return 0
}

# find_free_port_from START — echoes the first apparently-free port in
# [START, START+199]. Dies if none are free.
find_free_port_from() {
  local start="$1"
  local end=$((start + 199))
  local p
  for (( p=start; p<=end; p++ )); do
    if port_is_free "${p}"; then
      echo "${p}"
      return 0
    fi
  done
  die "no free UDP port in ${start}..${end}"
}

# ensure_token_entry TOKENS_TOML
# Generates a plugin-scoped token, writes the raw token to PLUGIN_TOKEN_FILE
# (mode 0600), and appends an entry for TOKEN_LABEL to TOKENS_TOML. Idempotent.
ensure_token_entry() {
  local tokens_toml="$1"

  if [[ -f "${PLUGIN_TOKEN_FILE}" ]] && [[ -f "${tokens_toml}" ]] \
     && grep -q "tokens\.${TOKEN_LABEL}\]" "${tokens_toml}" 2>/dev/null; then
    return 0
  fi

  # Stale state — revoke any prior entry with our label before regenerating.
  if [[ -f "${tokens_toml}" ]] && grep -q "tokens\.${TOKEN_LABEL}\]" "${tokens_toml}" 2>/dev/null; then
    "${PLUGIN_BIN_DIR}/demarkus-token" revoke \
      -label  "${TOKEN_LABEL}" \
      -tokens "${tokens_toml}" 2>/dev/null || true
  fi

  mkdir -p "$(dirname "${tokens_toml}")" "$(dirname "${PLUGIN_TOKEN_FILE}")"

  if ! "${PLUGIN_BIN_DIR}/demarkus-token" generate \
        -label  "${TOKEN_LABEL}" \
        -paths  "/*" \
        -ops    "publish,archive" \
        -tokens "${tokens_toml}" \
        2>/dev/null > "${PLUGIN_TOKEN_FILE}.tmp"; then
    rm -f "${PLUGIN_TOKEN_FILE}.tmp"
    die "demarkus-token generate failed (tokens.toml: ${tokens_toml})"
  fi

  mv "${PLUGIN_TOKEN_FILE}.tmp" "${PLUGIN_TOKEN_FILE}"
  chmod 600 "${PLUGIN_TOKEN_FILE}"
  log "generated plugin token (label=${TOKEN_LABEL})"
}

# ensure_managed_server SOUL_DIR PORT
# Spawns a demarkus-server for SOUL_DIR on PORT if ours isn't already running.
# Writes PID to ${SOUL_DIR}/.pid. For default/isolated modes only — do not
# call for reuse mode (the user's server is not ours to restart).
ensure_managed_server() {
  local soul_dir="$1" port="$2"
  local pid_file="${soul_dir}/.pid"
  local log_file="${soul_dir}/.log"

  # Note: the read+kill below is not atomic (classic TOCTOU), but the PID-reuse
  # window is microseconds and the worst-case failure is "plugin thinks a stale
  # PID is alive, skips spawn, MCP connections fail, user reruns /soul-init" —
  # not data loss. flock would add a macOS/Linux portability burden that isn't
  # worth closing this theoretical race.
  if [[ -f "${pid_file}" ]] && kill -0 "$(cat "${pid_file}")" 2>/dev/null; then
    return 0
  fi
  rm -f "${pid_file}"

  mkdir -p "${soul_dir}"

  nohup "${PLUGIN_BIN_DIR}/demarkus-server" \
        -root   "${soul_dir}" \
        -port   "${port}" \
        -tokens "${soul_dir}/tokens.toml" \
        >> "${log_file}" 2>&1 &
  local pid=$!
  echo "${pid}" > "${pid_file}"
  disown || true

  # Bounded poll: fail fast if the process died (bind or startup error),
  # succeed once the port is observed as bound, or accept once we reach the
  # attempt cap with the process still alive (permissive when no lsof/ss is
  # available to probe the port).
  local attempts=0
  local max_attempts=20  # ~2s at 100ms intervals
  while (( attempts < max_attempts )); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      rm -f "${pid_file}"
      local tail_info=""
      [[ -f "${log_file}" ]] && tail_info=$'\n'"recent log:"$'\n'"$(tail -5 "${log_file}")"
      die "demarkus-server failed to start (port ${port} may be in use; re-run /soul-init)${tail_info}"
    fi
    if ! port_is_free "${port}"; then
      break
    fi
    sleep 0.1
    attempts=$((attempts + 1))
  done
  log "spawned demarkus-server (pid=${pid}, port=${port}, root=${soul_dir})"
}
