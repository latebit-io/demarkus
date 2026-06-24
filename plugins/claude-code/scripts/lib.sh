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

# Strictness for the publish tag-gate (hooks/publish-gate.sh). One line:
# warn | block | ask. Kept in its own file, orthogonal to PLUGIN_CONFIG, so
# setup.sh rewrites of the conf never clobber a chosen strictness. Absent file
# means the warn default. (Organizational knowledge systems and their per-slug
# strictness live in the separate demarkus-knowledge plugin, not here.)
readonly PLUGIN_STRICTNESS_FILE="${PLUGIN_HOME}/plugin-memory.strictness"

# Strictness for the destination gate (hooks/dest-gate.sh): warn | block | ask.
# Separate file from the tag-gate strictness so the two enforcement axes are set
# independently. Absent file means the block default — an explicit project→soul
# binding means writes belong to the bound soul, so a misroute is an error to
# deny, not merely warn.
readonly PLUGIN_DEST_STRICTNESS_FILE="${PLUGIN_HOME}/plugin-memory.dest-strictness"

# Registry of joined knowledge-system MCP server slugs, written by the SEPARATE
# demarkus-knowledge plugin's /knowledge-join (one slug per line). We read it
# (never write it) purely to detect that a knowledge endpoint exists — the
# reverse of demarkus-knowledge's read-only peek at this plugin's
# plugin-memory.conf. This mutual, file-only detection is what gates the promote
# bridge (soul → knowledge): with no endpoint there is nothing to promote to, so
# /promote stays dormant. We never touch the broker — detection only.
readonly KNOWLEDGE_REGISTRY="${PLUGIN_HOME}/knowledge-systems"

# Catalog of joined REMOTE souls (direct-QUIC demarkus servers reached over the
# network, e.g. soul.demarkus.io), written by /soul-join. Tab-separated, one row
# per soul: "<slug>\t<host>\t<insecure 0|1>\t<token-file|->". This is the managed
# replacement for hand-wiring a remote demarkus-mcp into .mcp.json: the token
# lives in a 0600 file (token-file column), never inline in the MCP config. The
# local plugin-managed soul stays in PLUGIN_CONFIG (tier "local"); these rows are
# tier "remote". soul_catalog() unions both for a full view.
readonly SOULS_REGISTRY="${PLUGIN_HOME}/souls"

# Per-project binding: which catalog soul a given repo writes to by default.
# Tab-separated "<project-dir>\t<slug>", one row per bound repo, written by
# /soul-join when run inside a repo. The routing key that disambiguates "which
# soul does THIS project use" when several are configured.
readonly PROJECT_SOULS="${PLUGIN_HOME}/project-souls"

# Soul dirs that setup.sh may choose.
readonly SHARED_SOUL_DIR="${PLUGIN_HOME}/soul"
readonly ISOLATED_SOUL_DIR="${PLUGIN_HOME}/plugin-soul"

readonly DEMARKUS_PROTOCOL_PORT=6309   # demarkus-server's built-in default when nothing else is set
readonly DEFAULT_PORT=6310             # plugin's first-choice port for its own managed server
readonly ISOLATED_PORT_START=16310
readonly ISOLATED_PORT_END=16509

readonly SERVER_VERSION="0.18.0"
readonly CLIENT_VERSION="0.13.1"
# demarkus-token moved from the server archive to its own tools/ release
# in §6.7.A. Pin separately so a tools-only release can be picked up.
readonly TOOLS_VERSION="0.2.0"

log()  { echo "[demarkus-memory] $*" >&2; }
warn() { echo "[demarkus-memory] warning: $*" >&2; }
die()  { echo "[demarkus-memory] error: $*" >&2; exit 1; }

# seed_doc SEED_FILE TARGET_FILE — copy SEED_FILE to TARGET_FILE if the target
# is absent and its directory is writable. Best-effort: a no-op (return 0) when
# the directory isn't writable, the seed is missing, or the target already
# exists — it never clobbers existing content, so a user's customized doc
# survives across sessions. Logs only on an actual copy.
seed_doc() {
  local seed_file="$1" target="$2"
  local dir
  dir="$(dirname "${target}")"
  [[ -d "${dir}" && -w "${dir}" ]] || return 0
  [[ -f "${target}" ]] && return 0
  [[ -f "${seed_file}" ]] || return 0
  # Best-effort: a cp failure is surfaced (never silently swallowed) but does not
  # propagate a non-zero status to a caller running under `set -e`.
  if cp "${seed_file}" "${target}" 2>/dev/null; then
    log "seeded ${target}"
  else
    warn "could not seed ${target} (cp failed); continuing"
  fi
  return 0
}

# json_escape — reads stdin and emits it as a single JSON string literal
# (quotes included). Pure awk so it works without jq on stock macOS/Linux.
# Escapes backslash, double-quote, and tab; strips CR; turns each input line
# into a \n-terminated chunk. Sufficient for our markdown context payloads
# (printable ASCII + newlines); not a general-purpose UTF-8 control encoder.
json_escape() {
  awk '
    BEGIN { ORS=""; print "\"" }
    { gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); gsub(/\t/, "\\t"); gsub(/\r/, ""); print $0 "\\n" }
    END { print "\"" }
  '
}

# configured_strictness — echoes the publish tag-gate strictness: warn|block|ask.
# Precedence: DEMARKUS_MEMORY_STRICTNESS env override, then PLUGIN_STRICTNESS_FILE,
# then the warn default. Never dies. Any unrecognized value falls back to warn so
# a corrupt file or typo can never silently escalate to block.
configured_strictness() {
  local s="${DEMARKUS_MEMORY_STRICTNESS:-}"
  if [[ -z "${s}" && -f "${PLUGIN_STRICTNESS_FILE}" ]]; then
    s="$(tr -d '[:space:]' < "${PLUGIN_STRICTNESS_FILE}" 2>/dev/null || true)"
  fi
  case "${s}" in
    warn|block|ask) echo "${s}" ;;
    *)              echo "warn" ;;
  esac
}

# configured_dest_strictness — destination-gate strictness: warn|block|ask.
# Precedence: DEMARKUS_MEMORY_DEST_STRICTNESS env override, then
# PLUGIN_DEST_STRICTNESS_FILE, then the block default. Unlike the tag gate (which
# floors at warn so a typo can't silently escalate), block IS the intended
# default here, so an unrecognized value falls back to block.
configured_dest_strictness() {
  local s="${DEMARKUS_MEMORY_DEST_STRICTNESS:-}"
  if [[ -z "${s}" && -f "${PLUGIN_DEST_STRICTNESS_FILE}" ]]; then
    s="$(tr -d '[:space:]' < "${PLUGIN_DEST_STRICTNESS_FILE}" 2>/dev/null || true)"
  fi
  case "${s}" in
    warn|block|ask) echo "${s}" ;;
    *)              echo "block" ;;
  esac
}

# knowledge_endpoints — emit each joined knowledge-system slug on its own line
# (empty when none). Read-only peek at the demarkus-knowledge registry; blank
# lines and surrounding whitespace are stripped. Mirrors that plugin's own
# list_knowledge_systems so both sides read the registry identically. The slug
# is the MCP server name (e.g. "knowledge"), the addressing handle the agent
# uses for mcp__<slug>__mark_* tools when running the promote cascade.
knowledge_endpoints() {
  [[ -f "${KNOWLEDGE_REGISTRY}" ]] || return 0
  awk 'NF { gsub(/^[ \t]+|[ \t]+$/, ""); if ($0 != "") print }' "${KNOWLEDGE_REGISTRY}" 2>/dev/null || true
}

# knowledge_present — true when at least one brokered knowledge endpoint is
# joined (via the demarkus-knowledge plugin's /knowledge-join).
knowledge_present() {
  [[ -n "$(knowledge_endpoints)" ]]
}

# --- remote soul catalog (written by /soul-join) -------------------------------
#
# Rows are tab-separated "<slug>\t<host>\t<insecure>\t<token-file>". All readers
# below skip blank lines and #-comments and split on tab only, so a host or path
# containing spaces survives intact.

# register_remote_soul SLUG HOST INSECURE TOKEN_FILE — idempotent upsert. Any
# existing row for SLUG is replaced (a re-join can change host/token), so the
# catalog never carries two rows for one slug. INSECURE is normalized to 0|1;
# TOKEN_FILE of "" is stored as "-" (no auth). Returns non-zero on empty slug.
register_remote_soul() {
  local slug="$1" host="$2" insecure="$3" token_file="$4"
  [[ -n "${slug}" && -n "${host}" ]] || return 1
  case "${insecure}" in 1|true|yes) insecure=1 ;; *) insecure=0 ;; esac
  [[ -n "${token_file}" ]] || token_file="-"
  mkdir -p "${PLUGIN_HOME}"
  touch "${SOULS_REGISTRY}"
  local tmp="${SOULS_REGISTRY}.tmp"
  # Drop any prior row for this slug, then append the new one.
  awk -F '\t' -v s="${slug}" 'NF && $1 !~ /^#/ && $1 != s' "${SOULS_REGISTRY}" > "${tmp}" 2>/dev/null || true
  printf '%s\t%s\t%s\t%s\n' "${slug}" "${host}" "${insecure}" "${token_file}" >> "${tmp}"
  mv "${tmp}" "${SOULS_REGISTRY}"
}

# remote_soul_fields SLUG — echo the row's "HOST\tINSECURE\tTOKEN_FILE" (tab-
# separated) for SLUG, empty when not registered. The caller reads the three
# fields with `IFS=$'\t' read -r host insecure token_file <<<"$(...)"`.
remote_soul_fields() {
  local slug="$1"
  [[ -n "${slug}" && -f "${SOULS_REGISTRY}" ]] || return 0
  awk -F '\t' -v s="${slug}" 'NF && $1 !~ /^#/ && $1 == s { print $2 "\t" $3 "\t" $4; exit }' \
    "${SOULS_REGISTRY}" 2>/dev/null || true
}

# list_remote_souls — emit each registered remote-soul slug on its own line
# (empty when none). Blank lines and #-comments are skipped.
list_remote_souls() {
  [[ -f "${SOULS_REGISTRY}" ]] || return 0
  awk -F '\t' 'NF && $1 !~ /^#/ { print $1 }' "${SOULS_REGISTRY}" 2>/dev/null || true
}

# is_registered_remote_soul SLUG — true if SLUG is in the catalog.
is_registered_remote_soul() {
  local slug="$1"
  [[ -n "${slug}" ]] || return 1
  list_remote_souls | grep -qxF "${slug}"
}

# local_soul_present — true when the local plugin-managed soul is configured
# (PLUGIN_CONFIG exists). Its canonical catalog id / binding slug is the reserved
# name "demarkus-memory" (what soul_target_id echoes for the local server).
local_soul_present() {
  [[ -f "${PLUGIN_CONFIG}" ]]
}

# soul_catalog — emit one row per soul this plugin routes, tab-separated
# "<id>\t<tier>\t<host>\t<insecure>", local first then remotes:
#   - local managed soul (when configured): "demarkus-memory\tlocal\t-\t-"
#   - each remote soul (from SOULS_REGISTRY): "<slug>\tremote\t<host>\t<insecure>"
# The <id> column is exactly the slug a project binding stores and the dest-gate
# compares against (see soul_target_id). Empty when nothing is joined. This is
# the union promised by the SOULS_REGISTRY header comment — the single place that
# knows the full set of bindable write targets.
soul_catalog() {
  if local_soul_present; then
    printf 'demarkus-memory\tlocal\t-\t-\n'
  fi
  # Absent registry → no remotes joined (return 0). But a present-yet-unreadable
  # registry is an error, not "empty": let it propagate (return 1) instead of
  # masking a read failure as "no souls" — the CLI's --list would otherwise tell
  # the user to run /soul-join when the real problem is a permissions fault.
  [[ -e "${SOULS_REGISTRY}" ]] || return 0
  [[ -r "${SOULS_REGISTRY}" ]] || return 1
  awk -F '\t' 'NF && $1 !~ /^#/ { print $1 "\tremote\t" $2 "\t" $3 }' \
    "${SOULS_REGISTRY}"
}

# is_catalog_soul SLUG — true when SLUG is a bindable write target: the local
# managed soul's reserved id, or a registered remote slug. The validation a
# default-binding change runs before it touches PROJECT_SOULS, so a binding can
# never point at a soul that was never joined.
is_catalog_soul() {
  local slug="$1"
  [[ -n "${slug}" ]] || return 1
  [[ "${slug}" == "demarkus-memory" ]] && { local_soul_present; return; }
  is_registered_remote_soul "${slug}"
}

# bind_project_soul DIR SLUG — record that repo DIR writes to catalog soul SLUG
# by default. Idempotent upsert keyed on DIR. Returns non-zero on empty input.
bind_project_soul() {
  local dir="$1" slug="$2"
  [[ -n "${dir}" && -n "${slug}" ]] || return 1
  mkdir -p "${PLUGIN_HOME}"
  touch "${PROJECT_SOULS}"
  local tmp="${PROJECT_SOULS}.tmp"
  awk -F '\t' -v d="${dir}" 'NF && $1 !~ /^#/ && $1 != d' "${PROJECT_SOULS}" > "${tmp}" 2>/dev/null || true
  printf '%s\t%s\n' "${dir}" "${slug}" >> "${tmp}"
  mv "${tmp}" "${PROJECT_SOULS}"
}

# project_soul_binding DIR — echo the catalog slug bound to repo DIR, empty when
# unbound. Exact directory match (no prefix/parent resolution).
project_soul_binding() {
  local dir="$1"
  [[ -n "${dir}" && -f "${PROJECT_SOULS}" ]] || return 0
  awk -F '\t' -v d="${dir}" 'NF && $1 !~ /^#/ && $1 == d { print $2; exit }' \
    "${PROJECT_SOULS}" 2>/dev/null || true
}

# Registry of plain remote promote targets — demarkus servers (no broker) you
# may promote to, one per line: "<mcp-slug> <write-path> [label]". A plain
# server has no directory to enumerate writable worlds from, so the write path
# is DECLARED here at registration time (the client-side counterpart to the
# broker's mark_worlds writable surface — no server/protocol change needed).
# Written by scripts/promote-target.sh; read here. Memory stays broker-unaware:
# a plain endpoint is just an MCP server slug + a path, no broker involved.
readonly PROMOTE_TARGETS="${PLUGIN_HOME}/promote-targets"

# promote_targets — emit each registered plain target on its own line
# ("<slug> <path> [label]"), empty when none. Skips blank lines and #-comments;
# trims surrounding whitespace.
promote_targets() {
  [[ -f "${PROMOTE_TARGETS}" ]] || return 0
  awk '
    { sub(/^[ \t]+/, ""); sub(/[ \t]+$/, "") }
    NF && $1 !~ /^#/ { print }
  ' "${PROMOTE_TARGETS}" 2>/dev/null || true
}

# promote_destination_present — true when ANY promote destination exists,
# brokered or plain. The real detection gate for the promote bridge and its
# triggers: with no destination of either kind, promote is dormant.
promote_destination_present() {
  knowledge_present || [[ -n "$(promote_targets)" ]]
}

# publish_gate_scope TOOLNAME — echoes "local" when the tool targets a soul this
# plugin owns (so the publish tag-gate fires), empty otherwise. The server is the
# segment between the leading "mcp__" and the trailing "__mark_publish". Two soul
# kinds qualify:
#   - the local managed soul: server name "demarkus-memory" or the plugin-
#     prefixed "..._demarkus-memory". Matched exactly (not as a
#     "*demarkus-memory*" substring) so an unrelated server whose name merely
#     contains that token isn't swept in.
#   - a remote soul joined via /soul-join: server name equals the registered
#     catalog slug. Slugs are sanitized to [a-z0-9-] (no underscores) by
#     soul-join.sh, so they can never collide with the "*_demarkus-memory" arm.
# Organizational knowledge systems are gated by the separate demarkus-knowledge
# plugin, never here.
publish_gate_scope() {
  local tool="$1" server
  server="${tool#mcp__}"
  server="${server%__mark_publish}"
  case "${server}" in
    demarkus-memory | *_demarkus-memory) echo "local"; return ;;
  esac
  if is_registered_remote_soul "${server}"; then
    echo "local"
  fi
}

# soul_target_id TOOLNAME — echoes the canonical id of the soul a mark_publish /
# mark_append tool writes to, for comparison against a project binding:
#   "demarkus-memory"  — the local managed soul (server "demarkus-memory" or the
#                        plugin-prefixed "..._demarkus-memory")
#   "<slug>"           — a remote soul joined via /soul-join (server == slug)
#   (empty)            — the tool targets something else (a knowledge system or an
#                        unrelated demarkus server): not a soul this plugin routes,
#                        so the destination gate leaves it alone.
# The server is the segment between "mcp__" and the trailing "__mark_<verb>", so
# this handles both publish and append.
soul_target_id() {
  local tool="$1" server
  server="${tool#mcp__}"
  server="${server%__mark_*}"
  case "${server}" in
    demarkus-memory | *_demarkus-memory) echo "demarkus-memory"; return ;;
  esac
  if is_registered_remote_soul "${server}"; then
    echo "${server}"
  fi
}

# publish_metadata_check — reads a Claude Code PreToolUse/PostToolUse hook
# payload (the full JSON object) on stdin and emits exactly five lines:
#   1: hook_event_name
#   2: tool_name
#   3: TAGS_OK  — "1" if tool_input.metadata.tags is a non-empty (trimmed) string, else "0"
#   4: IMP_OK   — "1" if metadata.importance is absent OR a number in [0,1], else "0"
#   5: url      — tool_input.url (or empty)
#   6: tags     — the decoded tags string, internal whitespace flattened to one
#                 line (for axis-presence checks; empty when tagless)
#
# Pure awk — NO jq, NO python, nothing outside coreutils. The plugin ships zero
# runtime dependencies; awk is already the parser behind json_escape. A naive
# grep for "tags" would false-match the arbitrary `body` value (which can
# contain literal "tags": text and braces), so this is a real character-level
# JSON scan: it tracks string boundaries (honoring backslash escapes, so a
# quote inside `body` can't end the string early) and the object-key path, and
# captures only values at the exact paths it cares about. MCP metadata values
# arrive as strings, so importance is range-checked from its string form;
# numeric form is handled too. Pure read — mutates nothing.
publish_metadata_check() {
  awk '
    { data = data $0 "\n" }
    END {
      n = split(data, ch, "")
      depth = 0; inStr = 0; esc = 0; buf = ""
      ev = ""; tool = ""; url = ""
      tags = ""; haveTags = 0; tagsStr = 0; imp = ""; haveImp = 0
      for (i = 1; i <= n; i++) {
        c = ch[i]
        if (inStr) {
          if (esc) { buf = buf c; esc = 0; continue }
          if (c == "\\") { buf = buf c; esc = 1; continue }
          if (c == "\"") { inStr = 0; handleString(buf); buf = ""; continue }
          buf = buf c; continue
        }
        if (c == "\"") { inStr = 1; buf = ""; continue }
        if (c == " " || c == "\t" || c == "\n" || c == "\r") continue
        if (c == "{") { depth++; ctype[depth] = "o"; expect[depth] = "key"; continue }
        if (c == "[") { depth++; ctype[depth] = "a"; expect[depth] = "val"; continue }
        if (c == "}" || c == "]") { depth--; continue }
        if (c == ":") { expect[depth] = "val"; continue }
        if (c == ",") { expect[depth] = (ctype[depth] == "o") ? "key" : "val"; continue }
        # scalar value (number/true/false/null) — read to the next delimiter.
        # is_string=0: a non-string scalar must not satisfy the tags check.
        sv = c
        while (i + 1 <= n) {
          d = ch[i+1]
          if (d == "," || d == "}" || d == "]" || d == " " || d == "\t" || d == "\n" || d == "\r") break
          sv = sv d; i++
        }
        recordValue(sv, 0)
      }
      # Decode JSON string escapes BEFORE validating, otherwise {"tags":"\n"}
      # is stored raw as backslash+n — non-empty — and a whitespace-only value
      # slips past the emptiness check. The gate is a policy boundary, so the
      # decoded value is what must be judged.
      tags_d = json_unescape(tags)
      imp_d = json_unescape(imp)
      url_d = json_unescape(url)
      # tags must be a JSON STRING (per the field contract) and non-empty after
      # decode/trim — a bare false/null/0 scalar must not satisfy the gate.
      tags_ok = (haveTags && tagsStr && trim(tags_d) != "") ? "1" : "0"
      # importance: absent is fine; otherwise a JSON number in [0,1], including
      # exponent form (e.g. 1e-1). MCP sends it as a string, raw number handled too.
      if (!haveImp) imp_ok = "1"
      else if (imp_d ~ /^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$/) { nimp = imp_d + 0; imp_ok = (nimp >= 0 && nimp <= 1) ? "1" : "0" }
      else imp_ok = "0"
      # Surface the decoded tags (flattened to one line) so the gate can
      # presence-check required axes. Only for a genuine JSON-string value.
      tags_emit = (tagsStr ? tags_d : "")
      gsub(/[\n\r\t]/, " ", tags_emit)
      print ev; print tool; print tags_ok; print imp_ok; print url_d; print tags_emit
    }
    function trim(s) { gsub(/^[ \t\r\n]+/, "", s); gsub(/[ \t\r\n]+$/, "", s); return s }
    # json_unescape — decode JSON string escapes (\n \t \r \b \f \/ \" \\ and
    # \uXXXX) to their actual characters. For \uXXXX, whitespace codepoints
    # become a space and all other codepoints a non-space marker — enough to
    # judge emptiness/numeric-ness correctly without full UTF-8 reconstruction.
    function json_unescape(s,   out, i, n2, c, d, hex, code) {
      out = ""; n2 = length(s); i = 1
      while (i <= n2) {
        c = substr(s, i, 1)
        if (c == "\\" && i < n2) {
          d = substr(s, i + 1, 1)
          if (d == "n") { out = out "\n"; i += 2; continue }
          if (d == "t") { out = out "\t"; i += 2; continue }
          if (d == "r") { out = out "\r"; i += 2; continue }
          if (d == "b") { out = out "\b"; i += 2; continue }
          if (d == "f") { out = out "\f"; i += 2; continue }
          if (d == "/") { out = out "/"; i += 2; continue }
          if (d == "\"") { out = out "\""; i += 2; continue }
          if (d == "\\") { out = out "\\"; i += 2; continue }
          if (d == "u" && i + 5 <= n2) {
            hex = substr(s, i + 2, 4)
            if (hex ~ /^[0-9a-fA-F][0-9a-fA-F][0-9a-fA-F][0-9a-fA-F]$/) {
              code = hexval(hex)
              out = out (is_ws_code(code) ? " " : "x")
              i += 6; continue
            }
          }
          # Unknown/invalid escape: drop the backslash, keep the next char.
          out = out d; i += 2; continue
        }
        out = out c; i++
      }
      return out
    }
    # hexval — parse a hex string to its integer value (no 0x literals; BSD awk).
    function hexval(h,   v, i, c, dgt) {
      v = 0
      for (i = 1; i <= length(h); i++) {
        c = tolower(substr(h, i, 1))
        if (c >= "0" && c <= "9") dgt = c + 0
        else if (c == "a") dgt = 10; else if (c == "b") dgt = 11
        else if (c == "c") dgt = 12; else if (c == "d") dgt = 13
        else if (c == "e") dgt = 14; else if (c == "f") dgt = 15
        else dgt = 0
        v = v * 16 + dgt
      }
      return v
    }
    # is_ws_code — true for ASCII + common Unicode whitespace codepoints.
    function is_ws_code(code) {
      return (code == 9 || code == 10 || code == 11 || code == 12 || code == 13 || code == 32 || code == 160 || (code >= 8192 && code <= 8202) || code == 8232 || code == 8233 || code == 8239 || code == 8287 || code == 12288)
    }
    # Object-key path of the current position, e.g. /tool_input/metadata/tags.
    # Array levels contribute "/#" so an array can never alias an object path.
    function curpath(   p, d2) {
      p = ""
      for (d2 = 1; d2 <= depth; d2++) p = p "/" ((ctype[d2] == "o") ? ckey[d2] : "#")
      return p
    }
    # A completed string is a key (object, expecting a key) or a value.
    # A value here is_string=1 so tags captured from a JSON string is honored.
    function handleString(s) {
      if (ctype[depth] == "o" && expect[depth] == "key") { ckey[depth] = s; return }
      recordValue(s, 1)
    }
    function recordValue(v, is_string,   p) {
      p = curpath()
      if (p == "/hook_event_name") ev = v
      else if (p == "/tool_name") tool = v
      else if (p == "/tool_input/url") url = v
      else if (p == "/tool_input/metadata/tags") { tags = v; haveTags = 1; tagsStr = is_string }
      else if (p == "/tool_input/metadata/importance") { imp = v; haveImp = 1 }
    }
  '
}

# stop_hook_fields — reads a Claude Code Stop hook payload (a flat JSON object)
# on stdin and emits exactly three lines:
#   1: session_id
#   2: transcript_path
#   3: stop_hook_active ("true"/"false"; "false" when absent)
# Same pure-awk, string-boundary-aware approach as publish_metadata_check, but
# the Stop payload is flat so it only captures top-level (depth 1) keys. No deps.
stop_hook_fields() {
  awk '
    { data = data $0 "\n" }
    END {
      n = split(data, ch, "")
      depth = 0; inStr = 0; esc = 0; buf = ""; curkey = ""
      sid = ""; tpath = ""; active = "false"
      for (i = 1; i <= n; i++) {
        c = ch[i]
        if (inStr) {
          if (esc) { buf = buf c; esc = 0; continue }
          if (c == "\\") { buf = buf c; esc = 1; continue }
          if (c == "\"") { inStr = 0; onstr(buf); buf = ""; continue }
          buf = buf c; continue
        }
        if (c == "\"") { inStr = 1; buf = ""; continue }
        if (c == " " || c == "\t" || c == "\n" || c == "\r") continue
        if (c == "{") { depth++; expect[depth] = "key"; continue }
        if (c == "[") { depth++; expect[depth] = "val"; continue }
        if (c == "}" || c == "]") { depth--; continue }
        if (c == ":") { expect[depth] = "val"; continue }
        if (c == ",") { expect[depth] = "key"; continue }
        # scalar value (bool/number/null) — read to the next delimiter.
        sv = c
        while (i + 1 <= n) {
          d = ch[i+1]
          if (d == "," || d == "}" || d == "]" || d == " " || d == "\t" || d == "\n" || d == "\r") break
          sv = sv d; i++
        }
        if (depth == 1 && curkey == "stop_hook_active") active = sv
        expect[depth] = "key"
      }
      print sid; print tpath; print active
    }
    # At depth 1, a string is a key (records curkey) or the value for curkey.
    # stop_hook_active is documented as a JSON boolean (handled on the scalar
    # path), but accept a quoted "true"/"false" too — a loop guard should not
    # silently fail on a nonconforming payload.
    function onstr(s) {
      if (depth == 1 && expect[depth] == "key") { curkey = s; return }
      if (depth == 1) {
        if (curkey == "session_id") sid = s
        else if (curkey == "transcript_path") tpath = s
        else if (curkey == "stop_hook_active") active = s
      }
    }
  '
}

# server_health_warning — echoes a one-line human warning when the configured
# memory server does not appear to be up, empty when it looks healthy. Relies
# on load_config having set SOUL_DIR/PORT/MODE. Best-effort and probe-only (no
# QUIC round-trip): for managed modes it trusts the .pid liveness that
# ensure_managed_server maintains; for reuse it scans for the adopted process.
# Deliberately avoids the port_is_free check as a health signal — that probe
# degrades to permissive without lsof/ss and would emit false "not listening"
# warnings on a perfectly healthy server.
server_health_warning() {
  case "${MODE:-}" in
    default|isolated)
      local pid_file="${SOUL_DIR}/.pid"
      local pid=""
      [[ -f "${pid_file}" ]] && pid="$(cat "${pid_file}" 2>/dev/null || true)"
      # Validate the recorded PID is a bare positive integer before probing —
      # an empty, partial, or option-like .pid (e.g. "-1") would otherwise make
      # kill -0 misread it and report a dead server as alive.
      if [[ -z "${pid}" || ! "${pid}" =~ ^[0-9]+$ ]] || ! kill -0 "${pid}" 2>/dev/null; then
        echo "the demarkus-memory server is not running (no live process for ${SOUL_DIR}). Memory tools (mark_fetch/mark_publish/mark_lookup/...) will fail until it restarts — run /soul-init to restart, or /soul-status to diagnose."
      fi
      ;;
    reuse)
      if [[ -z "$(pid_of_server_at_root "${SOUL_DIR}" 2>/dev/null)" ]]; then
        echo "demarkus-memory is configured to reuse a server rooted at ${SOUL_DIR}, but none is running. Memory tools will fail until it is started — start that server or run /soul-init to reconfigure."
      fi
      ;;
  esac
}

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

# _binary_version BIN ARGS... — echoes the version BIN reports via its version
# flag, or empty if BIN is missing, not executable, or too old to support the
# flag (a pre-0.17.15 binary errors on an unknown flag). stderr is discarded so
# an old binary's "flag provided but not defined" noise doesn't leak into the
# captured value.
_binary_version() {
  local bin="$1"; shift
  [[ -x "${bin}" ]] || return 0
  "${bin}" "$@" 2>/dev/null
}

# _installed_versions — the versions the on-disk binaries actually report,
# queried live via each binary's --version, in the same shape _desired_versions
# emits. The binaries are the source of truth: unlike a sidecar file it can't
# drift from what's really installed, and it also catches a corrupt or wrong-arch
# binary (which fails to run → empty field). A missing or pre-version binary
# yields an empty field, which never equals a desired version, so ensure_binaries
# re-downloads. demarkus-token accepts --version as well as its `version`
# subcommand, so one flag form covers all three.
_installed_versions() {
  printf 'server=%s,client=%s,tools=%s' \
    "$(_binary_version "${PLUGIN_BIN_DIR}/demarkus-server" --version)" \
    "$(_binary_version "${PLUGIN_BIN_DIR}/demarkus-mcp" --version)" \
    "$(_binary_version "${PLUGIN_BIN_DIR}/demarkus-token" --version)"
}

# _desired_versions — the version pin the plugin currently expects.
_desired_versions() {
  printf 'server=%s,client=%s,tools=%s' "${SERVER_VERSION}" "${CLIENT_VERSION}" "${TOOLS_VERSION}"
}

# ensure_binaries — download + install to PLUGIN_BIN_DIR when the installed
# binaries don't report exactly the pinned versions (queried live via --version,
# so a pin bump, a missing/corrupt binary, or a pre-version binary all trigger a
# re-download on the next session start). No sidecar to keep in sync: the
# binaries are the source of truth, and a partial/failed install simply reads as
# a mismatch and re-downloads next time.
# DEMARKUS_BINARIES_REPLACED is a plain (unexported) global ensure_binaries sets
# to 1 when it actually swapped the on-disk binary this run, 0 otherwise;
# restart_local_server_on_upgrade reads it (defaulting to 0 when never set) to
# decide whether to restart the running server onto the new binary. Unexported so
# it never leaks into the spawned server's environment.
ensure_binaries() {
  DEMARKUS_BINARIES_REPLACED=0
  local desired installed
  desired="$(_desired_versions)"
  installed="$(_installed_versions)"
  if [[ "${installed}" == "${desired}" ]]; then
    return 0
  fi

  # Distinguish a stale install (something is there, wrong version) from a fresh
  # one (nothing there) for the log line only — both re-download.
  if [[ -x "${PLUGIN_BIN_DIR}/demarkus-server" \
     || -x "${PLUGIN_BIN_DIR}/demarkus-mcp" \
     || -x "${PLUGIN_BIN_DIR}/demarkus-token" ]]; then
    log "binary version drift detected (installed=${installed}, desired=${desired}); re-downloading"
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
  # set -e. A failed install needs no cleanup: the next ensure_binaries call
  # re-queries the binaries via --version, sees the mismatch, and retries.
  local install_rc=$?
  if (( install_rc != 0 )); then
    return "${install_rc}"
  fi

  DEMARKUS_BINARIES_REPLACED=1
  log "binaries installed to ${PLUGIN_BIN_DIR}"
}

# restart_local_server_on_upgrade — when ensure_binaries swapped the binary this
# run, make the configured local server run on the new binary, using its OWN
# recorded config (SOUL_DIR/PORT from PLUGIN_CONFIG). No-op when no binary
# changed or no local soul is configured.
#
# Two cases, by ownership — and ownership is the MODE, not a .pid file:
#   - default/isolated: the server is ours. ensure_managed_server restarts it
#     onto the new binary (handling our .pid itself).
#   - reuse: the server is the user's, ALWAYS. A .pid is not proof of ownership
#     here — save_config never clears stale state, so a reuse config can carry an
#     old .pid (left by a prior managed config at this root, or written by our
#     own down-start below). Keying the managed path on .pid would let
#     ensure_managed_server stop that PID and kill a healthy external server —
#     the exact regression this guards against. So reuse never takes the managed
#     path: a running server is left alone (warn it's on the old binary), and
#     only a DOWN one is started on the new binary with the recorded config.
#
# Runs in a subshell so load_config can't clobber the caller's SOUL_DIR/PORT/MODE,
# and never fails the caller: a restart problem is warned, not propagated.
restart_local_server_on_upgrade() {
  [[ "${DEMARKUS_BINARIES_REPLACED:-0}" == "1" ]] || return 0
  (
    load_config 2>/dev/null || exit 0

    # Our own managed server — safe to restart onto the new binary. Gated on MODE
    # (not just .pid) so a stale .pid under a reuse config can't trigger this.
    if [[ "${MODE}" != "reuse" && -f "${SOUL_DIR}/.pid" ]]; then
      log "binary upgraded — restarting managed server (mode=${MODE}, soul=${SOUL_DIR}, port=${PORT}) on the new binary"
      ensure_managed_server "${SOUL_DIR}" "${PORT}"
      exit 0
    fi

    # Reuse mode, or a managed server that's down: a server we must not assume is
    # ours. Never kill a running one; only start it if it's down.
    local ext_pid
    ext_pid="$(pid_of_server_at_root "${SOUL_DIR}" 2>/dev/null || true)"
    if [[ -n "${ext_pid}" ]]; then
      # Healthy and not ours — do not touch it.
      warn "binary upgraded, but the server at ${SOUL_DIR} (pid=${ext_pid}, mode=${MODE}) is user-managed and still running the old binary; restart it yourself to pick up the new one"
      exit 0
    fi

    # Down — start it on the new binary with the recorded config.
    log "binary upgraded — starting down server (mode=${MODE}, soul=${SOUL_DIR}, port=${PORT}) on the new binary"
    ensure_managed_server "${SOUL_DIR}" "${PORT}"
  ) || warn "could not (re)start the local server after a binary upgrade; run /soul-init to recover"
  return 0
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

# managed_server_current PID_FILE VERSION_FILE — true (0) when the managed
# server recorded in PID_FILE is alive AND the binary version stamped in
# VERSION_FILE matches the current SERVER_VERSION pin. Any other case (no/garbage
# PID, dead process, missing or mismatched stamp) is false (1), telling
# ensure_managed_server to (re)spawn onto the pinned binary. The stamp is what
# lets a binary upgrade propagate to a still-running server: an unstamped server
# (older plugin) or a stale stamp both read as not-current → restart.
managed_server_current() {
  local pid_file="$1" version_file="$2"
  [[ -f "${pid_file}" ]] || return 1
  local pid; pid="$(cat "${pid_file}" 2>/dev/null || true)"
  [[ "${pid}" =~ ^[0-9]+$ ]] || return 1
  kill -0 "${pid}" 2>/dev/null || return 1
  local ver; ver="$(cat "${version_file}" 2>/dev/null || true)"
  [[ "${ver}" == "${SERVER_VERSION}" ]]
}

# ensure_managed_server SOUL_DIR PORT
# Spawns a demarkus-server for SOUL_DIR on PORT if ours isn't already running.
# Writes PID to ${SOUL_DIR}/.pid and the spawned binary's version to
# ${SOUL_DIR}/.server-version. For default/isolated modes only — do not call
# for reuse mode (the user's server is not ours to restart).
#
# A live managed server is reused only when its recorded version matches the
# current SERVER_VERSION pin. When ensure_binaries has upgraded the on-disk
# binary under a still-running server (the version stamp now lags the pin, or
# is absent because an older plugin spawned it), this restarts the server onto
# the new binary — otherwise the process would keep serving the stale binary
# from memory until it died on its own (reboot / manual kill), which is the
# "server binary behind after a plugin update" symptom. The soul is on-disk and
# versioned, so the restart never risks data; demarkus is QUIC (UDP), so the
# killed process frees its port on exit with no TIME_WAIT to wait out.
ensure_managed_server() {
  local soul_dir="$1" port="$2"
  local pid_file="${soul_dir}/.pid"
  local version_file="${soul_dir}/.server-version"
  local log_file="${soul_dir}/.log"

  # Note: the read+kill in managed_server_current is not atomic (classic
  # TOCTOU), but the PID-reuse window is microseconds and the worst-case failure
  # is "plugin thinks a stale PID is alive, skips spawn, MCP connections fail,
  # user reruns /soul-init" — not data loss. flock would add a macOS/Linux
  # portability burden that isn't worth closing this theoretical race.
  if managed_server_current "${pid_file}" "${version_file}"; then
    return 0
  fi
  # Not reusable. If a live-but-stale managed server is recorded (binary upgraded
  # under it, or an unstamped server left by an older plugin), stop it before we
  # respawn on the new binary — otherwise the old process keeps serving the stale
  # binary from memory. A dead/garbage PID just gets its files cleared.
  local running_pid; running_pid="$(cat "${pid_file}" 2>/dev/null || true)"
  if [[ "${running_pid}" =~ ^[0-9]+$ ]] && kill -0 "${running_pid}" 2>/dev/null; then
    log "restarting managed demarkus-server onto upgraded binary (target=${SERVER_VERSION})"
    kill "${running_pid}" 2>/dev/null || true
    # Bounded wait for exit (~3s), then escalate to SIGKILL so the respawn below
    # can bind the freed UDP port instead of dying with "port in use".
    local waited=0
    while (( waited < 30 )) && kill -0 "${running_pid}" 2>/dev/null; do
      sleep 0.1
      waited=$((waited + 1))
    done
    kill -0 "${running_pid}" 2>/dev/null && kill -9 "${running_pid}" 2>/dev/null || true
  fi
  rm -f "${pid_file}" "${version_file}"

  mkdir -p "${soul_dir}"

  nohup "${PLUGIN_BIN_DIR}/demarkus-server" \
        -root   "${soul_dir}" \
        -port   "${port}" \
        -tokens "${soul_dir}/tokens.toml" \
        >> "${log_file}" 2>&1 &
  local pid=$!
  echo "${pid}" > "${pid_file}"
  printf '%s\n' "${SERVER_VERSION}" > "${version_file}"
  disown || true

  # Bounded poll: fail fast if the process died (bind or startup error),
  # succeed once the port is observed as bound, or accept once we reach the
  # attempt cap with the process still alive (permissive when no lsof/ss is
  # available to probe the port).
  local attempts=0
  local max_attempts=20  # ~2s at 100ms intervals
  while (( attempts < max_attempts )); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      rm -f "${pid_file}" "${version_file}"
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
