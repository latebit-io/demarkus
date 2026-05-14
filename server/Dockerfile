# demarkus-server image. Bundles:
#   /demarkus-server  — the QUIC world server (ENTRYPOINT)
#   /demarkus         — CLI; the helm chart's exec probes invoke it
#                       against the local server's agent-manifest path
#
# Admin CLIs (demarkus-token, demarkus-publish) are NOT bundled —
# they ship as standalone binaries via tools/.goreleaser.yml so the
# running server pod's blast radius stays minimal. Operators run them
# locally or in dedicated Job pods, not via kubectl exec.
#
# Build context = repo root so the multi-module source tree
# (protocol/ + server/ + client/) is reachable from one Dockerfile:
#   docker build -f server/Dockerfile -t demarkus-server:dev .
#
# Smoke-tested end-to-end via deploy/kind/up.sh.

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY protocol/go.mod protocol/go.sum protocol/
COPY server/go.mod   server/go.sum   server/
COPY client/go.mod   client/go.sum   client/

COPY protocol/ protocol/
COPY server/   server/
COPY client/   client/

ARG VERSION=dev
ENV CGO_ENABLED=0

RUN go build -C server -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-server ./cmd/demarkus-server
RUN go build -C client -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus        ./cmd/demarkus

FROM scratch
COPY --from=build /out/demarkus-server /demarkus-server
COPY --from=build /out/demarkus        /demarkus
# Match the chart's podSecurityContext.runAsUser so the image runs
# non-root even when deployed outside k8s (plain `docker run`, CI
# runners, etc.). The chart enforces this via runAsNonRoot: true; the
# Dockerfile-level USER is defense-in-depth.
USER 65532:65532
EXPOSE 6309/udp
ENTRYPOINT ["/demarkus-server"]
