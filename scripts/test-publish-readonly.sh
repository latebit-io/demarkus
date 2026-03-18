#!/bin/bash
# Test: publish documents with demarkus-publish, then serve them read-only.
# This validates the workflow where demarkus-publish populates a content
# directory and demarkus-server serves it in read-only mode.
#
# Also tests the "chroot" pattern: the server's -root points at a subdirectory
# that simulates a chroot jail, and demarkus-publish writes into it from
# outside — exactly how you'd publish into a chrooted server's content dir.
set -e

PUBLISH=${PUBLISH:-./demarkus-publish}
SERVER=${SERVER:-./demarkus-server}
CLIENT=${CLIENT:-./demarkus}
PORT=9741
JAIL=$(mktemp -d)
# The server only sees CONTENT_ROOT (the "chroot"). Publish writes into it
# from the outside, as a separate process would on a real system.
CONTENT_ROOT="$JAIL/srv/demarkus/content"
mkdir -p "$CONTENT_ROOT"

cleanup() {
  kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$JAIL"
}
trap cleanup EXIT

echo "=== jail: $JAIL ==="
echo "=== content root: $CONTENT_ROOT ==="

# 1. Publish documents into the chroot content dir from outside
echo ""
echo "--- publishing documents into chroot content dir ---"
$PUBLISH -root "$CONTENT_ROOT" -path /index.md -body "# Welcome"
$PUBLISH -root "$CONTENT_ROOT" -path /notes/hello.md -body "hello world"

# 2. Verify versioned layout was created
echo ""
echo "--- verifying layout ---"
ls -la "$CONTENT_ROOT"/index.md
ls -la "$CONTENT_ROOT"/versions/
ls -la "$CONTENT_ROOT"/notes/versions/

# 3. Publish an update to verify versioning works
echo ""
echo "--- publishing update to /index.md ---"
$PUBLISH -root "$CONTENT_ROOT" -path /index.md -body "# Welcome (updated)"

echo "--- verifying v2 created ---"
ls -la "$CONTENT_ROOT"/index.md
ls -la "$CONTENT_ROOT"/versions/

# 4. Republish same content — should report unchanged
echo ""
echo "--- republishing same content (should be unchanged) ---"
$PUBLISH -root "$CONTENT_ROOT" -path /index.md -body "# Welcome (updated)" 2>&1 | tee /dev/stderr | grep -q "unchanged"
echo "PASS: duplicate publish detected as unchanged"

# 5. Start server in read-only mode, rooted at the chroot content dir
echo ""
echo "--- starting read-only server on port $PORT ---"
$SERVER -read-only -root "$CONTENT_ROOT" -port "$PORT" &
SERVER_PID=$!
sleep 1

# 6. Fetch documents through the server
echo ""
echo "--- fetching /index.md (should be v2) ---"
OUTPUT=$($CLIENT -insecure mark://localhost:$PORT/index.md 2>/dev/null)
echo "$OUTPUT"
if echo "$OUTPUT" | grep -q "updated"; then
  echo "PASS: got updated content"
else
  echo "FAIL: expected updated content"
  exit 1
fi

echo ""
echo "--- fetching /notes/hello.md ---"
OUTPUT=$($CLIENT -insecure mark://localhost:$PORT/notes/hello.md 2>/dev/null)
echo "$OUTPUT"
if echo "$OUTPUT" | grep -q "hello world"; then
  echo "PASS: got nested document"
else
  echo "FAIL: expected hello world"
  exit 1
fi

# 7. Verify writes are rejected in read-only mode
echo ""
echo "--- verifying write is rejected ---"
OUTPUT=$($CLIENT -insecure -X PUBLISH -body "should fail" mark://localhost:$PORT/test.md 2>&1)
if echo "$OUTPUT" | grep -q "read-only"; then
  echo "PASS: write correctly rejected in read-only mode"
else
  echo "FAIL: expected read-only rejection, got: $OUTPUT"
  exit 1
fi

# 8. Publish new content while server is running (live update)
echo ""
echo "--- publishing new doc while server is running ---"
$PUBLISH -root "$CONTENT_ROOT" -path /live.md -body "published while serving"

echo "--- fetching /live.md ---"
OUTPUT=$($CLIENT -insecure -no-cache mark://localhost:$PORT/live.md 2>/dev/null)
echo "$OUTPUT"
if echo "$OUTPUT" | grep -q "published while serving"; then
  echo "PASS: live publish visible to read-only server"
else
  echo "FAIL: live-published doc not found"
  exit 1
fi

echo ""
echo "=== all tests passed ==="
