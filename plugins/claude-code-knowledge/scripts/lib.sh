#!/usr/bin/env bash
# Shared helpers for the demarkus-knowledge plugin scripts.
# Source this file; do not execute it directly.
# shellcheck disable=SC2034  # several constants are consumed only by sourcing scripts
#
# This plugin is the broker-fronted, organizational counterpart to demarkus-memory.
# It owns NO binaries and NO local server: a knowledge system is reached over
# HTTPS via `claude mcp add --transport http` and Claude Code's own MCP OAuth.
# So this lib is deliberately tiny — registry, strictness, an awk JSON parser for
# the publish gate, and read-only detection of a sibling soul for cross-linking.

# Requires Bash 3.2+ (macOS stock bash). No Bash 4+ features are used.
if [ -n "${BASH_VERSINFO:-}" ] && [ "${BASH_VERSINFO[0]}" -lt 3 ]; then
  echo "[demarkus-knowledge] error: bash 3.2+ required (got ${BASH_VERSION:-unknown})" >&2
  return 1 2>/dev/null || exit 1
fi

# ~/.demarkus is the shared home for both demarkus plugins. This plugin only
# writes its own files here (the registry + per-slug policy mirrors); it reads
# the soul's config file (written by demarkus-memory) but never modifies it.
readonly PLUGIN_HOME="${HOME}/.demarkus"

# Registry of joined knowledge-system MCP server slugs (one per line), recorded
# by /knowledge-join. The publish gate enforces tags on writes to a registered
# system; it never touches unrelated demarkus servers the user has configured
# (e.g. a personal soul served by the demarkus-memory plugin).
readonly PLUGIN_KNOWLEDGE_REGISTRY="${PLUGIN_HOME}/knowledge-systems"

# Per-slug strictness for the publish tag-gate: warn | block | ask. The agent
# mirrors a knowledge system's published policy into PLUGIN_STRICTNESS_FILE.<slug>
# (the gate cannot reach the broker, so the enforceable knobs travel as files).
# This plugin owns its own file namespace ("plugin-knowledge.*") — it never reads
# or writes the demarkus-memory plugin's files, so it stands alone.
readonly PLUGIN_STRICTNESS_FILE="${PLUGIN_HOME}/plugin-knowledge.strictness"

# Required tag axes a knowledge system declares in its policy (e.g. team, type).
# A publish satisfies an axis when its tags include an "axis:value" token. The
# agent mirrors require_tags into PLUGIN_REQUIRE_TAGS_FILE.<slug>; the gate then
# presence-checks each axis.
readonly PLUGIN_REQUIRE_TAGS_FILE="${PLUGIN_HOME}/plugin-knowledge.require-tags"

# Written by the demarkus-memory plugin when a local soul is configured. Read
# (never written) here so the joined-knowledge guidance can describe the
# soul↔knowledge-system relationship only when a soul actually exists.
readonly SOUL_CONFIG="${PLUGIN_HOME}/plugin-memory.conf"

log()  { echo "[demarkus-knowledge] $*" >&2; }
warn() { echo "[demarkus-knowledge] warning: $*" >&2; }
die()  { echo "[demarkus-knowledge] error: $*" >&2; exit 1; }

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

# soul_is_configured — true if the demarkus-memory plugin has a local soul set
# up. Used only to decide whether to surface the soul↔knowledge-system synergy.
soul_is_configured() { [[ -f "${SOUL_CONFIG}" ]]; }

# list_knowledge_systems — emit each registered slug on its own line (empty when
# none). Blank lines and surrounding whitespace are stripped.
list_knowledge_systems() {
  [[ -f "${PLUGIN_KNOWLEDGE_REGISTRY}" ]] || return 0
  awk 'NF { gsub(/^[ \t]+|[ \t]+$/, ""); if ($0 != "") print }' "${PLUGIN_KNOWLEDGE_REGISTRY}" 2>/dev/null || true
}

# configured_strictness [SLUG] — echoes the publish tag-gate strictness:
# warn|block|ask. Precedence: DEMARKUS_KNOWLEDGE_STRICTNESS env override, then a
# per-slug file (PLUGIN_STRICTNESS_FILE.<slug>), then the warn default. Never
# dies. Any unrecognized value falls back to warn so a corrupt file or typo can
# never silently escalate to block.
configured_strictness() {
  local slug="${1:-}"
  local s="${DEMARKUS_KNOWLEDGE_STRICTNESS:-}"
  if [[ -z "${s}" && -n "${slug}" && -f "${PLUGIN_STRICTNESS_FILE}.${slug}" ]]; then
    s="$(tr -d '[:space:]' < "${PLUGIN_STRICTNESS_FILE}.${slug}" 2>/dev/null || true)"
  fi
  case "${s}" in
    warn|block|ask) echo "${s}" ;;
    *)              echo "warn" ;;
  esac
}

# configured_require_tags [SLUG] — echo the required tag axes (space-separated)
# for this system: a per-slug PLUGIN_REQUIRE_TAGS_FILE.<slug>, else empty. The
# file may list axes comma- or newline-separated; output is normalized to single
# spaces. Empty → no axis requirement.
configured_require_tags() {
  local slug="${1:-}" raw=""
  if [[ -n "${slug}" && -f "${PLUGIN_REQUIRE_TAGS_FILE}.${slug}" ]]; then
    raw="$(cat "${PLUGIN_REQUIRE_TAGS_FILE}.${slug}" 2>/dev/null || true)"
  fi
  printf '%s' "${raw}" | tr ',\r\n\t' '    ' | tr -s ' ' | sed 's/^ //;s/ $//'
}

# tags_have_axis TAGS AXIS — true if the comma-separated TAGS list contains a
# token of the form "AXIS:value". AXIS is matched as a LITERAL prefix (the case
# pattern quotes it), so an axis containing regex/glob metacharacters can't
# overmatch — unlike a `grep -E` interpolation would.
tags_have_axis() {
  local tags="$1" axis="$2" tok rc=1
  local oldifs="$IFS" reglob=""
  # Disable pathname expansion while splitting the unquoted list, so a tag with
  # glob metacharacters (*, ?, []) can't expand against the filesystem and make
  # matching nondeterministic. Restore -f only if we were the one to set it.
  case "$-" in *f*) ;; *) reglob=1; set -f ;; esac
  IFS=','
  for tok in ${tags}; do
    # trim surrounding whitespace from the token
    tok="${tok#"${tok%%[![:space:]]*}"}"
    tok="${tok%"${tok##*[![:space:]]}"}"
    case "${tok}" in
      "${axis}:"*) rc=0; break ;;
    esac
  done
  IFS="${oldifs}"
  [[ -n "${reglob}" ]] && set +f
  return "${rc}"
}

# register_knowledge_system SLUG — append SLUG to the knowledge-system registry
# if absent. Idempotent. Returns non-zero on empty input.
register_knowledge_system() {
  local slug="$1"
  [[ -n "${slug}" ]] || return 1
  mkdir -p "${PLUGIN_HOME}"
  touch "${PLUGIN_KNOWLEDGE_REGISTRY}"
  grep -qxF "${slug}" "${PLUGIN_KNOWLEDGE_REGISTRY}" 2>/dev/null && return 0
  printf '%s\n' "${slug}" >> "${PLUGIN_KNOWLEDGE_REGISTRY}"
}

# is_registered_knowledge_system SLUG — true if SLUG is in the registry.
is_registered_knowledge_system() {
  local slug="$1"
  [[ -n "${slug}" && -f "${PLUGIN_KNOWLEDGE_REGISTRY}" ]] || return 1
  grep -qxF "${slug}" "${PLUGIN_KNOWLEDGE_REGISTRY}" 2>/dev/null
}

# publish_gate_scope TOOLNAME — classifies an MCP mark_publish tool, echoing:
#   ks:<slug>      — a registered knowledge system (gate with per-slug strictness)
#   (empty)        — anything else (the local soul, or an unmanaged server)
# The server is the segment between the leading "mcp__" and the trailing
# "__mark_publish". The local demarkus-memory soul is explicitly NOT ours to
# gate here — that is the demarkus-memory plugin's publish gate.
publish_gate_scope() {
  local tool="$1" server
  server="${tool#mcp__}"
  server="${server%__mark_publish}"
  case "${server}" in
    *demarkus-memory*) return 0 ;;   # the local soul — handled by demarkus-memory
  esac
  if is_registered_knowledge_system "${server}"; then
    echo "ks:${server}"
  fi
}

# publish_metadata_check — reads a Claude Code PreToolUse/PostToolUse hook
# payload (the full JSON object) on stdin and emits exactly six lines:
#   1: hook_event_name
#   2: tool_name
#   3: TAGS_OK  — "1" if tool_input.metadata.tags is a non-empty (trimmed) string, else "0"
#   4: IMP_OK   — "1" if metadata.importance is absent OR a number in [0,1], else "0"
#   5: url      — tool_input.url (or empty)
#   6: tags     — the decoded tags string, internal whitespace flattened to one
#                 line (for axis-presence checks; empty when tagless)
#
# Pure awk — NO jq, NO python, nothing outside coreutils. A naive grep for
# "tags" would false-match the arbitrary `body` value (which can contain literal
# "tags": text and braces), so this is a real character-level JSON scan: it
# tracks string boundaries (honoring backslash escapes) and the object-key path,
# and captures only values at the exact paths it cares about. MCP metadata values
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
