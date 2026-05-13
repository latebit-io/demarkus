# Unified demarkus image. Bakes every deployable binary into a single
# scratch-based container; each chart picks which binary runs by setting
# the pod spec's `command:`. Bundled binaries:
#
#   /demarkus-server   — world server (UDP/QUIC)
#   /demarkus-broker   — OIDC token broker (HTTP)
#   /demarkus-agent    — federation crawler / hub aggregator
#   /demarkus          — CLI (used by server chart's exec probes)
#   /demarkus-token    — token-mint admin CLI (used by broker integration)
#   /demarkus-publish  — direct content-store publisher
#
# Build from the repo root: `docker build -t ghcr.io/latebit-io/demarkus:dev .`

FROM golang:1.26-alpine AS build
WORKDIR /src

# Copy module manifests first to take advantage of layer caching: the
# go.mod / go.sum files change less often than source.
COPY protocol/go.mod protocol/go.sum protocol/
COPY server/go.mod server/go.sum server/
COPY client/go.mod client/go.sum client/
COPY tools/go.mod tools/go.sum tools/

COPY protocol/ protocol/
COPY server/ server/
COPY client/ client/
COPY tools/ tools/

ARG VERSION=dev
ENV CGO_ENABLED=0

RUN cd server && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-server  ./cmd/demarkus-server
RUN cd client && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus         ./cmd/demarkus
RUN cd client && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-agent   ./cmd/demarkus-agent
RUN cd tools  && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-broker  ./demarkus-broker
RUN cd tools  && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-token   ./demarkus-token
RUN cd tools  && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-publish ./demarkus-publish

FROM scratch
COPY --from=build /out/demarkus-server  /demarkus-server
COPY --from=build /out/demarkus-broker  /demarkus-broker
COPY --from=build /out/demarkus-agent   /demarkus-agent
COPY --from=build /out/demarkus         /demarkus
COPY --from=build /out/demarkus-token   /demarkus-token
COPY --from=build /out/demarkus-publish /demarkus-publish

# No ENTRYPOINT — charts must set `command:` explicitly to pick a binary.
# This is deliberate: a default ENTRYPOINT would make running the wrong
# binary an unobservable misconfiguration ("why is my broker pod serving
# QUIC?").
EXPOSE 6309/udp
