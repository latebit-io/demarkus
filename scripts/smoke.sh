#!/bin/bash
# Smoke test of built binaries over real QUIC: the file server on a temp root and
# the knowledge server against fake-gcs-server in Docker.
#
# usage: bash scripts/smoke.sh [file|knowledge|all]   (default all)
# build: make server knowledge-server client tools
set -u

cd "$(dirname "$0")/.." || exit 2
MODE=${1:-all}
FILE_PORT=${SMOKE_FILE_PORT:-16409}
KNOWLEDGE_PORT=${SMOKE_KNOWLEDGE_PORT:-16410}
HEALTH_PORT=${SMOKE_HEALTH_PORT:-18181}
GCS_PORT=${SMOKE_GCS_PORT:-14443}
GCS_IMAGE=${SMOKE_GCS_IMAGE:-fsouza/fake-gcs-server:latest}
GCS_NAME="demarkus-smoke-gcs-$$"
STEP_LIMIT=${SMOKE_STEP_LIMIT:-15}
case "$STEP_LIMIT" in
'' | *[!0-9]* | 0*)
  echo "SMOKE_STEP_LIMIT must be a positive whole number of seconds (got '$STEP_LIMIT')" >&2
  exit 2
  ;;
esac
WORLD_ID=52b471f7-8d38-4c89-b44a-6f4f8b1a4f48
POLICY=/.well-known/demarkus/policy.md

for bin in server/bin/demarkus-server server/bin/demarkus-knowledge-server client/bin/demarkus client/bin/demarkus-mcp tools/bin/demarkus-token; do
  if [ ! -x "$bin" ]; then
    echo "missing $bin: run make server knowledge-server client tools" >&2
    exit 2
  fi
done

WORK=$(mktemp -d)
SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  docker rm -f "$GCS_NAME" >/dev/null 2>&1
  if [ "${SMOKE_KEEP:-}" = 1 ]; then echo "kept $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

C=(client/bin/demarkus -insecure -no-cache -v)
pass=0
fail=0

# bounded command...: kill it after STEP_LIMIT seconds, so one stalled QUIC
# request or curl call cannot hang the run. macOS ships no timeout(1).
bounded() {
  local pid dog status
  "$@" &
  pid=$!
  # Detached from stdout, or a command substitution would wait out the sleep.
  (
    sleep "$STEP_LIMIT"
    kill "$pid" 2>/dev/null
  ) >/dev/null 2>&1 &
  dog=$!
  wait "$pid"
  status=$?
  kill "$dog" 2>/dev/null
  wait "$dog" 2>/dev/null
  return $status
}

# check <want|avoid> <label> <pattern> -- command...
# avoid also needs the command to succeed: a crash must not read as "absent".
check() {
  local mode=$1 label=$2 pattern=$3 out status found=avoid
  shift 4
  out=$(bounded "$@" 2>&1 </dev/null)
  status=$?
  if printf '%s' "$out" | grep -qE -- "$pattern"; then found=want; fi
  if [ "$found" = "$mode" ] && { [ "$mode" = want ] || [ "$status" -eq 0 ]; }; then
    pass=$((pass + 1))
    echo "PASS  $label"
  else
    fail=$((fail + 1))
    echo "FAIL  $label ($mode /$pattern/, exit $status)"
    printf '%s\n' "$out" | head -8 | sed 's/^/      /'
  fi
}
expect() { check want "$@"; }
refute() { check avoid "$@"; }

# mint <label> <paths> <ops>: appends to the tokens file, prints the raw token.
mint() {
  tools/bin/demarkus-token generate -label "$1" -paths "$2" -ops "$3" -tokens "$WORK/tokens.toml" 2>/dev/null </dev/null | tail -1
}

# retry <what> command...: up to ten seconds for a service to come up.
retry() {
  local what=$1 tries=0
  shift
  until bounded "$@" >/dev/null 2>&1 </dev/null; do
    tries=$((tries + 1))
    if [ "$tries" -ge 50 ]; then
      echo "FAIL  $what never came up"
      fail=$((fail + 1))
      return 1
    fi
    sleep 0.2
  done
}

# setup_failed <step>: a setup step that fails must fail the run.
setup_failed() {
  fail=$((fail + 1))
  echo "FAIL  setup: $1"
  return 1
}

healthy() { "${C[@]}" "$1/health" 2>&1 | grep -q '^\[ok\]'; }

# start_server <url> <log> command...: background the server, wait for health.
start_server() {
  local url=$1 log=$2
  shift 2
  "$@" >>"$log" 2>&1 </dev/null &
  SERVER_PID=$!
  # A server that never answers must not outlive the run holding its port.
  retry "server at $url" healthy "$url" || { stop_server; return 1; }
}

stop_server() {
  [ -z "$SERVER_PID" ] && return 0
  kill -TERM "$SERVER_PID" 2>/dev/null
  wait "$SERVER_PID" 2>/dev/null
  local status=$?
  SERVER_PID=""
  return $status
}

# verbs <url>: every verb, auth, paging, archive and lookup through the CLI.
verbs() {
  local U=$1 n page1 page2 cursor hash
  expect "health" '^\[ok\]' -- "${C[@]}" "$U/health"
  expect "publish without token is unauthorized" 'unauthorized' -- "${C[@]}" -X PUBLISH -force -body "# A" "$U/docs/a.md"
  expect "publish with a read-only token is not permitted" 'not-permitted' -- "${C[@]}" -X PUBLISH -force -auth "$R" -body "# A" "$U/docs/a.md"
  expect "publish creates v1" 'created' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 0 -meta tags=smoke,alpha -body $'# A\none' "$U/docs/a.md"
  for n in b c d e; do
    expect "publish creates $n.md" 'created' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 0 -meta tags=smoke -body "# $n" "$U/docs/$n.md"
  done
  expect "stale expected-version conflicts" 'conflict' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 7 -body "# A2" "$U/docs/a.md"
  expect "create-only on an existing doc conflicts" 'conflict' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 0 -body "# A2" "$U/docs/a.md"
  expect "non .md path is bad-request" 'bad-request' -- "${C[@]}" -X PUBLISH -force -auth "$W" -body "x" "$U/docs/a.txt"
  expect "append makes v2" 'created' -- "${C[@]}" -X APPEND -auth "$W" -expected-version 1 -body "two" "$U/docs/a.md"
  expect "publish without a version is refused before it is sent" 'requires -expected-version' -- "${C[@]}" -X PUBLISH -auth "$W" -body "x" "$U/docs/a.md"
  expect "a stale publish offers a merge candidate" 'merge-candidate.*publish-at-version=2' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 1 -body $'# A\none\nmine' "$U/docs/a.md"
  expect "on-conflict fail reports the bare conflict" '^\[conflict\]' -- "${C[@]}" -X PUBLISH -on-conflict fail -auth "$W" -expected-version 1 -body "x" "$U/docs/a.md"
  expect "append resolves its own version" 'created' -- "${C[@]}" -X APPEND -auth "$W" -body "more" "$U/docs/b.md"
  expect "fetch returns the joined body" '^two$' -- "${C[@]}" "$U/docs/a.md"
  expect "fetch carries version 2" 'version=2' -- "${C[@]}" "$U/docs/a.md"
  expect "fetch of v1 omits the append" '^one$' -- "${C[@]}" "$U/docs/a.md/v1"
  expect "versions has a valid chain" 'chain-valid=true' -- "${C[@]}" -X VERSIONS "$U/docs/a.md"
  expect "versions counts two" 'total=2' -- "${C[@]}" -X VERSIONS "$U/docs/a.md"
  expect "fetch of a missing doc is not-found" 'not-found' -- "${C[@]}" "$U/docs/missing.md"
  expect "traversal is not-found" 'not-found' -- "${C[@]}" "$U/docs/../../etc/passwd"
  page1=$("${C[@]}" -X LIST -page-size 2 "$U/docs/" 2>&1 </dev/null)
  expect "list page 1 has two entries" 'entries=2' -- printf '%s' "$page1"
  expect "list page 1 is marked incomplete" 'complete=false' -- printf '%s' "$page1"
  cursor=$(printf '%s' "$page1" | grep -oE 'next-cursor=[^ ]+' | head -1 | cut -d= -f2)
  page2=$("${C[@]}" -X LIST -page-size 2 -cursor "$cursor" "$U/docs/" 2>&1 </dev/null)
  expect "list page 2 starts at c.md" 'c\.md' -- printf '%s' "$page2"
  refute "list page 2 does not repeat a.md" 'a\.md' -- printf '%s' "$page2"
  expect "unpaged list is complete with five" 'entries=5' -- "${C[@]}" -X LIST "$U/docs/"
  expect "directory fetch generates an index" 'a\.md' -- "${C[@]}" "$U/docs/"
  expect "lookup by tag finds a.md" '/docs/a\.md' -- client/bin/demarkus lookup -insecure -query alpha "$U/"
  expect "lookup body match finds the appended word" '/docs/a\.md' -- client/bin/demarkus lookup -insecure -query two -match body "$U/"
  expect "archive" '^\[ok\]' -- "${C[@]}" -X ARCHIVE -auth "$W" "$U/docs/e.md"
  expect "archived doc leaves the listing" 'entries=4' -- "${C[@]}" -X LIST "$U/docs/"
  expect "archived doc shows with include-archived" 'entries=5' -- "${C[@]}" -X LIST -include-archived "$U/docs/"
  expect "publish to an archived doc is refused" 'archived' -- "${C[@]}" -X PUBLISH -force -auth "$W" -body "# E2" "$U/docs/e.md"
  expect "unarchive with an empty body" '^\[ok\]' -- "${C[@]}" -X PUBLISH -force -auth "$W" -body "" "$U/docs/e.md"
  expect "unarchived doc is listed again" 'entries=5' -- "${C[@]}" -X LIST "$U/docs/"
  expect "publish creates the private doc" 'created' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 0 -body "# Secret" "$U/private/s.md"
  expect "private read without token is unauthorized" 'unauthorized' -- "${C[@]}" "$U/private/s.md"
  expect "private read with the reader token" '# Secret' -- "${C[@]}" -auth "$R" "$U/private/s.md"
  refute "private dir hidden from an anonymous root listing" 'private' -- "${C[@]}" -X LIST "$U/"
  expect "private dir visible with the reader token" 'private' -- "${C[@]}" -X LIST -auth "$R" "$U/"
  refute "lookup hides private rows from anonymous" 's\.md' -- client/bin/demarkus lookup -insecure -query Secret -match body "$U/"
  hash=$("${C[@]}" "$U/docs/b.md" 2>&1 </dev/null | grep -oE 'content-hash=sha256-[0-9a-f]+' | head -1 | cut -d= -f2)
  expect "fetch by content hash" '# b' -- "${C[@]}" "$U/$hash"
}

# policy <url>: the write policy as the knowledge server enforces it.
policy() {
  local U=$1
  expect "seeded policy is version 1" 'version=1' -- "${C[@]}" "$U$POLICY"
  expect "seeded policy carries the seed marker" 'agent=demarkus-knowledge-server' -- "${C[@]}" "$U$POLICY"
  expect "replace with a blocking policy" 'created' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 1 -meta tags=category:governance -meta type=Policy -body $'# Write Policy\n\nstrictness: block\nrequire_tags: domain\n' "$U$POLICY"
  expect "untagged publish is bad-request" 'bad-request' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 0 -body "# U" "$U/gated/u.md"
  expect "untagged publish names the violation" 'missing-tags' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 0 -body "# U" "$U/gated/u.md"
  expect "tagged publish is created" 'created' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 0 -meta tags=domain:smoke -body "# T" "$U/gated/t.md"
  expect "append inheriting tags is created" 'created' -- "${C[@]}" -X APPEND -auth "$W" -expected-version 1 -body "more" "$U/gated/t.md"
  expect "append replacing tags is bad-request" 'bad-request' -- "${C[@]}" -X APPEND -auth "$W" -expected-version 2 -meta tags=other:x -body "more" "$U/gated/t.md"
  expect "unenforceable candidate policy is bad-request" 'bad-request' -- "${C[@]}" -X PUBLISH -auth "$W" -expected-version 2 -meta tags=category:governance -body $'strictness: nonsense\n' "$U$POLICY"
  expect "archiving the required policy is bad-request" 'bad-request' -- "${C[@]}" -X ARCHIVE -auth "$W" "$U$POLICY"
  expect "the policy is still live" 'version=2' -- "${C[@]}" "$U$POLICY"
}

# mcp_call <url> <tool> <json arguments>: one tools/call through the built
# demarkus-mcp over stdio. HOME is private so the run never touches the user's
# graph store, cache or tokens.
mcp_call() {
  local url=$1 tool=$2 arguments=$3
  mkdir -p "$WORK/mcphome"
  printf '%s\n' \
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
    '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
    "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$arguments}}" |
    HOME="$WORK/mcphome" client/bin/demarkus-mcp -host "$url" -token "$W" -insecure -no-cache -profile full 2>>"$WORK/mcp.log" | tail -1
}

# mcp_tools <url>: the MCP surface over real QUIC, after verbs has filled /docs.
mcp_tools() {
  local U=$1
  expect "mcp list shows a.md" 'a\.md' -- mcp_call "$U" mark_list '{"url":"/docs/"}'
  expect "mcp versions reports the current version" 'current: 2' -- mcp_call "$U" mark_versions '{"url":"/docs/a.md"}'
  expect "mcp fetch returns the body" 'two' -- mcp_call "$U" mark_fetch '{"url":"/docs/a.md"}'
  expect "mcp lookup body match finds a.md" '/docs/a\.md' -- mcp_call "$U" mark_lookup '{"url":"/","query":"two","match":"body"}'
  expect "mcp explore orients on a.md" 'a\.md' -- mcp_call "$U" mark_explore '{"url":"/docs/a.md"}'
  expect "mcp publish creates a document" 'created' -- mcp_call "$U" mark_publish '{"url":"/mcp/new.md","body":"# New\\n\\nfirst\\n","expected_version":0,"metadata":{"tags":"smoke,mcp","importance":"0.1"}}'
  expect "mcp publish at a stale version offers a merge" 'merge' -- mcp_call "$U" mark_publish '{"url":"/docs/a.md","body":"# A\\nmine\\n","expected_version":1}'
  expect "mcp append resolves the version itself" 'version' -- mcp_call "$U" mark_append '{"url":"/mcp/new.md","body":"second"}'
  expect "mcp fetch sees the append" 'second' -- mcp_call "$U" mark_fetch '{"url":"/mcp/new.md"}'
  expect "mcp index dry run names the source by identity" "from mark://localhost:$FILE_PORT" -- mcp_call "$U" mark_index "{\"source\":\"$U\",\"target\":\"/index/smoke.md\",\"dry_run\":true,\"force\":true}"
  expect "mcp archive archives the document" 'archived' -- mcp_call "$U" mark_archive '{"url":"/mcp/new.md"}'
  refute "mcp stderr has no panic" 'panic|fatal error' -- cat "$WORK/mcp.log"
}

no_errors() {
  # awk prints only the offending lines and exits 0. A peer closing with code 0
  # mid response is a client leaving, as the short lived MCP process does; the
  # server still logs that at ERROR (finding S37).
  refute "$1 log has no ERROR lines" 'level=ERROR' -- \
    awk '/level=ERROR/ && !/write response failed.*Application error 0x0 \(remote\)/' "$2"
}

smoke_file() {
  local U="mark://localhost:$FILE_PORT"
  echo "== file server"
  mkdir -p "$WORK/root"
  start_file() {
    start_server "$U" "$WORK/file.log" env DEMARKUS_ROOT="$WORK/root" DEMARKUS_PORT="$FILE_PORT" \
      DEMARKUS_TOKENS="$WORK/tokens.toml" server/bin/demarkus-server
  }
  start_file || return 1
  verbs "$U"
  mcp_tools "$U"
  stop_server
  start_file || return 1
  expect "restart rebuilds the lookup catalog" 'lookup catalog built.*entries=[1-9]' -- tail -n 8 "$WORK/file.log"
  expect "restart serves the same data" 'version=2' -- "${C[@]}" "$U/docs/a.md"
  expect "restart keeps a valid chain" 'chain-valid=true' -- "${C[@]}" -X VERSIONS "$U/docs/a.md"
  stop_server
  no_errors "file server" "$WORK/file.log"
}

smoke_knowledge() {
  local U="mark://localhost:$KNOWLEDGE_PORT" first_lines status
  echo "== knowledge server"
  docker info >/dev/null 2>&1 || { setup_failed "docker is not available"; return 1; }
  docker run -d --rm --name "$GCS_NAME" -p "$GCS_PORT:4443" "$GCS_IMAGE" \
    -scheme http -public-host "localhost:$GCS_PORT" >/dev/null || { setup_failed "start fake-gcs-server"; return 1; }
  retry "fake-gcs-server bucket" curl -fsS -X POST -H 'Content-Type: application/json' \
    -d '{"name":"smoke-world"}' "http://localhost:$GCS_PORT/storage/v1/b" || return 1
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost \
    -addext subjectAltName=DNS:localhost \
    -keyout "$WORK/key.pem" -out "$WORK/cert.pem" >/dev/null 2>&1 || { setup_failed "generate the localhost certificate"; return 1; }
  cat >"$WORK/knowledge.yaml" <<EOF
version: 1
listen:
  address: ":$KNOWLEDGE_PORT"
health:
  address: ":$HEALTH_PORT"
tls:
  certFile: $WORK/cert.pem
  keyFile: $WORK/key.pem
worlds:
  - name: smoke
    authorities: [localhost]
    bucket:
      url: gs://smoke-world
      worldID: $WORLD_ID
    auth:
      tokensFile: $WORK/tokens.toml
    limits:
      requestTimeout: 10s
EOF
  start_knowledge() {
    start_server "$U" "$WORK/knowledge.log" env STORAGE_EMULATOR_HOST="localhost:$GCS_PORT" \
      server/bin/demarkus-knowledge-server -config "$WORK/knowledge.yaml"
  }
  start_knowledge || return 1
  expect "genesis in the empty bucket is logged" 'created a new world' -- cat "$WORK/knowledge.log"
  expect "the policy seed is logged" 'seeded the initial write policy' -- cat "$WORK/knowledge.log"
  verbs "$U"
  policy "$U"
  stop_server
  status=$?
  expect "SIGTERM exits cleanly" '^0$' -- echo "$status"
  first_lines=$(wc -l <"$WORK/knowledge.log")
  start_knowledge || return 1
  refute "restart does not reseed" 'seeded the initial write policy' -- tail -n "+$((first_lines + 1))" "$WORK/knowledge.log"
  expect "restart serves the curated policy" 'require_tags: domain' -- "${C[@]}" "$U$POLICY"
  expect "restart keeps a valid chain" 'chain-valid=true' -- "${C[@]}" -X VERSIONS "$U/docs/a.md"
  stop_server
  no_errors "knowledge server" "$WORK/knowledge.log"
}

: >"$WORK/tokens.toml"
chmod 600 "$WORK/tokens.toml"
W=$(mint writer '/**' publish)
R=$(mint reader '/private/**' read)
if [ -z "$W" ] || [ -z "$R" ]; then
  echo "FAIL  setup: mint tokens" >&2
  exit 1
fi

case "$MODE" in
file) smoke_file ;;
knowledge) smoke_knowledge ;;
all)
  smoke_file
  smoke_knowledge
  ;;
*)
  echo "usage: bash scripts/smoke.sh [file|knowledge|all]" >&2
  exit 2
  ;;
esac

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
