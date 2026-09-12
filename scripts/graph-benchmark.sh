#!/bin/bash
# Run from this checkout. Output directories are immutable run artifacts.
set -euo pipefail

if [ "$#" -ne 1 ]; then
    echo "usage: bash scripts/graph-benchmark.sh NEW_OUTPUT_DIRECTORY" >&2
    exit 2
fi

root=$(git rev-parse --show-toplevel)
output=$1
count=${BENCH_COUNT:-6}
benchtime=${BENCH_TIME:-200ms}
if [[ ! "$count" =~ ^[1-9][0-9]*$ ]]; then
    echo "BENCH_COUNT must be a positive integer" >&2
    exit 2
fi
if [ ! -d "$(dirname "$output")" ]; then
    echo "output parent directory must exist: $(dirname "$output")" >&2
    exit 2
fi
# Check before creating run artifacts; paths alone cannot reproduce new inputs.
untracked=$(git -C "$root" ls-files --others --exclude-standard)
if [ -n "$untracked" ]; then
    echo "untracked files must be tracked or excluded before benchmarking:" >&2
    echo "$untracked" >&2
    exit 2
fi
mkdir "$output"

generated=$(date -u +%Y-%m-%dT%H:%M:%SZ)
revision=$(git -C "$root" rev-parse HEAD)
branch=$(git -C "$root" branch --show-current)
{
    echo "suite: graph-baseline-v1"
    echo "generated: $generated"
    echo "revision: $revision"
    echo "branch: $branch"
    echo "count: $count"
    echo "benchtime: $benchtime"
    echo "cpu-flag: 1"
    go version
    go env -json GOOS GOARCH GOAMD64 GOARM64 CGO_ENABLED GOEXPERIMENT GOFLAGS
    echo "working-tree:"
    git -C "$root" status --short
    echo "tracked-diff-hash:"
    git -C "$root" diff HEAD --binary | git hash-object --stdin
    echo "harness-and-dependency-hashes:"
    for file in \
        client/graphstore/graph_baseline_test.go \
        client/internal/fedcrawl/graph_baseline_test.go \
        tools/internal/broker/graph_baseline_test.go \
        tools/internal/retrievalbench/graph_baseline_test.go \
        tools/internal/retrievalbench/questions/graph-context-v1.json \
        scripts/graph-benchmark.sh \
        protocol/go.mod protocol/go.sum client/go.mod client/go.sum tools/go.mod tools/go.sum; do
        hash=$(git hash-object "$root/$file")
        echo "$file $hash"
    done
} > "$output/manifest.txt"

go -C "$root/client" test ./graphstore ./internal/fedcrawl \
    -run '^$' -bench '^BenchmarkGraphBaseline' -benchmem \
    -benchtime "$benchtime" -count "$count" -cpu 1 -timeout 10m 2>&1 \
    | tee "$output/client.txt"
go -C "$root/tools" test ./internal/broker ./internal/retrievalbench \
    -run '^$' -bench '^BenchmarkGraphBaseline' -benchmem \
    -benchtime "$benchtime" -count "$count" -cpu 1 -timeout 10m 2>&1 \
    | tee "$output/tools.txt"
echo "complete" > "$output/status.txt"
