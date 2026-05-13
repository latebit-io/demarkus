# demarkus-agent image. The agent is a daemon crawler with no probes
# today (per chart values default), so the CLI is not bundled. If
# probes are added later, this Dockerfile can grow to bundle /demarkus
# alongside the agent.
#
# Build context = repo root so the protocol/ module is reachable via
# the client/ replace directive:
#   docker build -f client/cmd/demarkus-agent/Dockerfile -t demarkus-agent:dev .

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY protocol/go.mod protocol/go.sum protocol/
COPY client/go.mod   client/go.sum   client/

COPY protocol/ protocol/
COPY client/   client/

ARG VERSION=dev
ENV CGO_ENABLED=0

RUN go build -C client -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-agent ./cmd/demarkus-agent

FROM scratch
# Trust store for outbound TLS. The agent crawls remote mark:// servers
# whose QUIC TLS certificates require validation when `insecure: false`
# (the chart's production default). Without this bundle, the agent's
# fetch handshakes fail against any non-self-signed peer.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/demarkus-agent /demarkus-agent
# Match the chart's podSecurityContext.runAsUser so the image runs
# non-root even when deployed outside k8s. Defense-in-depth on top of
# the chart's runAsNonRoot: true.
USER 65532:65532
ENTRYPOINT ["/demarkus-agent"]
