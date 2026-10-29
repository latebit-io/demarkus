#!/usr/bin/env bash
# WATCH on the MCP binding across two replicas (ADR 0047, SPEC §6.8.1),
# against a cluster `up.sh --with-knowledge` brought up. From an ephemeral
# curl pod: listen on replica A, publish through replica B's MCP gateway,
# expect the notification on A, then resume on B from A's cursor.
#
# The bearer is the mock IdP's id_token for alice, which the gateway verifies
# against the issuer's JWKS. Runs standalone or from up.sh --with-mcp-smoke.
set -euo pipefail

CLUSTER="${CLUSTER:-knowledge-system}"
# Pinned, never the current context: a cloud context may be current.
KUBE_CONTEXT="${KUBE_CONTEXT:-kind-$CLUSTER}"
NAMESPACE="${NAMESPACE:-demarkus}"
KNOWLEDGE_RELEASE="${KNOWLEDGE_RELEASE:-knowledge}"
MINT_CURL_IMAGE="${MINT_CURL_IMAGE:-curlimages/curl:8.11.1}"
WORLD="${WATCH_SMOKE_WORLD:-world-default}"
OIDC_CLIENT_ID="${OIDC_CLIENT_ID:-demarkus-knowledge-kind}"

kube() { kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" "$@"; }

SELECTOR="app.kubernetes.io/instance=$KNOWLEDGE_RELEASE,app.kubernetes.io/name=demarkus-knowledge-server"
# Running and not terminating: a pod on its way out still answers briefly.
read -r -a PODS <<<"$(kube get pods -l "$SELECTOR" --field-selector=status.phase=Running \
  -o jsonpath='{range .items[*]}{.metadata.deletionTimestamp}{"|"}{.status.podIP}{"\n"}{end}' | sed -n 's/^|//p' | tr '\n' ' ')"
if [[ ${#PODS[@]} -lt 2 ]]; then
  echo "watch-smoke: need two running knowledge replicas, found ${#PODS[@]}" >&2
  kube get pods -l "$SELECTOR" >&2
  exit 1
fi
REPLICA_A="${PODS[0]}"
REPLICA_B="${PODS[1]}"
RUN="$(date +%s)"

echo "--- WATCH smoke: listen on $REPLICA_A, publish through $REPLICA_B (world $WORLD)"
kube run "watch-smoke-$RUN" --rm -i --restart=Never --image="$MINT_CURL_IMAGE" --command -- sh -c '
set -eu
A='"$REPLICA_A"'
B='"$REPLICA_B"'
WORLD='"$WORLD"'
RUN='"$RUN"'
IDP=http://mock-oauth2-server.'"$NAMESPACE"'.svc.cluster.local:8080/default
CLIENT_ID='"$OIDC_CLIENT_ID"'
CURL="curl -sS --connect-timeout 5 --max-time 15"
SUB="mark://$WORLD/watch-smoke-$RUN/"
# fail <what> [<file to show>]
fail() { echo "FAIL: $1"; [ -z "${2:-}" ] || cat "$2"; exit 1; }

# 1. Alice'"'"'s id_token from the mock IdP (interactiveLogin=false answers
#    /authorize with a code at once). Never echoed.
LOC=$($CURL -D - -o /dev/null -G --data-urlencode "client_id=$CLIENT_ID" --data-urlencode "response_type=code" \
  --data-urlencode "redirect_uri=http://localhost/cb" --data-urlencode "scope=openid email" \
  --data-urlencode "state=watch" --data-urlencode "nonce=watch" "$IDP/authorize" | awk "/^[Ll]ocation:/{print \$2}" | tr -d "\r")
CODE=$(printf "%s" "$LOC" | sed -n "s/.*[?&]code=\([^&]*\).*/\1/p")
[ -n "$CODE" ] || fail "no code from the mock IdP: $LOC"
$CURL -o /tmp/tok.json -X POST "$IDP/token" --data-urlencode "grant_type=authorization_code" --data-urlencode "code=$CODE" \
  --data-urlencode "client_id=$CLIENT_ID" --data-urlencode "client_secret=unused" --data-urlencode "redirect_uri=http://localhost/cb"
# The issuer pretty-prints; the answer is never echoed, only its keys.
TOKEN=$(tr -d "\n" < /tmp/tok.json | sed -n "s/.*\"id_token\" *: *\"\([^\"]*\)\".*/\1/p")
[ -n "$TOKEN" ] || fail "no id_token from the mock IdP; keys: $(grep -o "\"[a-z_]*\" *:" /tmp/tok.json | tr -d "\n")"
echo "OK: id_token for alice"

# listen <replica> <out file> [<since cursor>]: a subscriptions/listen
# stream in the background, its SSE lines in the file.
listen() {
  SINCE=""
  [ -z "${3:-}" ] || SINCE=",\"demarkus.io/since\":{\"$SUB\":\"$3\"}"
  curl -sS -N --max-time 120 -X POST "http://$1:8080/mcp" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
    -H "Mcp-Protocol-Version: 2026-07-28" -H "Mcp-Method: subscriptions/listen" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":\"watch-smoke\",\"method\":\"subscriptions/listen\",\"params\":{\"_meta\":{\"io.modelcontextprotocol/protocolVersion\":\"2026-07-28\",\"io.modelcontextprotocol/clientCapabilities\":{}$SINCE},\"notifications\":{\"resourceSubscriptions\":[\"$SUB\"]}}}" \
    > "$2" 2>&1 &
  LISTENER=$!
}
# await <file> <pattern> <label>: the pattern appears within 15 s.
await() {
  for attempt in $(seq 1 75); do
    grep -q "$2" "$1" && { echo "OK: $3"; return 0; }
    sleep 0.2
  done
  fail "$3" "$1"
}

# tool <replica> <name> <arguments json>: a tools/call on a fresh session
# of that replica; the answer lands in /tmp/rpc.out.
tool() {
  printf "Authorization: Bearer %s\nContent-Type: application/json\nAccept: application/json, text/event-stream\n" "$TOKEN" > /tmp/hdrs
  $CURL -X POST "http://$1:8080/mcp" -H @/tmp/hdrs -D /tmp/rpc.h -o /tmp/rpc.out \
    -d "{\"jsonrpc\":\"2.0\",\"id\":0,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-03-26\",\"capabilities\":{},\"clientInfo\":{\"name\":\"watch-smoke\",\"version\":\"0\"}}}"
  SESSION=$(awk "tolower(\$1)==\"mcp-session-id:\"{print \$2}" /tmp/rpc.h | tr -d "\r")
  [ -n "$SESSION" ] || fail "no session from $1" /tmp/rpc.out
  printf "Mcp-Session-Id: %s\n" "$SESSION" >> /tmp/hdrs
  $CURL -X POST "http://$1:8080/mcp" -H @/tmp/hdrs -o /dev/null -d "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}"
  $CURL -X POST "http://$1:8080/mcp" -H @/tmp/hdrs -o /tmp/rpc.out \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$2\",\"arguments\":$3}}"
  ! grep -q "\"isError\":true" /tmp/rpc.out || fail "$2 on $1" /tmp/rpc.out
}
publish() {
  tool "$1" mark_publish "{\"url\":\"$SUB$2\",\"body\":\"# Watch smoke\\n\\nA hint for the listen.\\n\",\"expected_version\":0,\"metadata\":{\"tags\":\"smoke\"}}"
  grep -q "version: 1" /tmp/rpc.out || fail "publish $2 on $1" /tmp/rpc.out
}

# 2. Listen on A; the acknowledgement comes first.
listen "$A" /tmp/a.sse
await /tmp/a.sse "notifications/subscriptions/acknowledged" "replica A acknowledged the listen"

# 3. Publish through B; A notifies, with the event in _meta.
publish "$B" one.md
await /tmp/a.sse "\"uri\":\"${SUB}one.md\"" "a publish through replica B notified the listen on replica A"
EVENT=$(grep "${SUB}one.md" /tmp/a.sse | head -1)
for want in "\"demarkus.io/subscription\":\"$SUB\"" "\"demarkus.io/op\":\"publish\"" "\"demarkus.io/version\":1" "\"demarkus.io/user\":\"alice@example.com\""; do
  printf "%s" "$EVENT" | grep -q "$want" || fail "event lacks $want: $EVENT"
done
CURSOR=$(printf "%s" "$EVENT" | sed -n "s/.*\"demarkus.io\/cursor\":\"\([^\"]*\)\".*/\1/p")
[ -n "$CURSOR" ] || fail "event has no cursor: $EVENT"
echo "OK: the event carries subscription, op, version, user and cursor $CURSOR"
kill "$LISTENER"

# 4. A write while nobody listens, then resume on B from A'"'"'s cursor.
publish "$B" two.md
listen "$B" /tmp/b.sse "$CURSOR"
await /tmp/b.sse "notifications/subscriptions/acknowledged" "replica B acknowledged the resumed listen"
await /tmp/b.sse "\"uri\":\"${SUB}two.md\"" "replica B replayed the missed write from replica A'"'"'s cursor"
! grep -q "demarkus.io/resync" /tmp/b.sse || fail "the resume resynced" /tmp/b.sse
! grep -q "${SUB}one.md" /tmp/b.sse || fail "the resume repeated an event before its cursor" /tmp/b.sse
kill "$LISTENER"

# 5. Leave nothing behind.
for doc in one.md two.md; do
  tool "$A" mark_archive "{\"url\":\"$SUB$doc\"}"
done
echo "OK: WATCH on the MCP binding across two replicas"
'
echo "--- WATCH smoke passed"
