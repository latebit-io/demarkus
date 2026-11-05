#!/bin/bash
# No compiled binary is ever tracked: builds go to <module>/bin/ or dist/, which
# are ignored. Catches one built without -o, whatever its name, before it lands.
set -euo pipefail

cd "$(dirname "$0")/.."

found=$(git ls-files -z | xargs -0 file --mime-type -N -- |
  grep -E ': application/(x-mach-binary|x-executable|x-pie-executable|x-sharedlib|x-dosexec)$' || true)
if [ -n "$found" ]; then
  echo "Compiled binaries are tracked; build with -o bin/<name> and git rm --cached them:" >&2
  echo "$found" >&2
  exit 1
fi
echo "✓ No tracked binaries"
