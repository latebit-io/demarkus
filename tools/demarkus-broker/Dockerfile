# demarkus-broker image. Single binary; the broker is HTTP-only and uses
# HTTP /healthz + /readyz probes, so the CLI is not bundled.
#
# Build context = repo root so the protocol/ module is reachable via the
# tools/ replace directive:
#   docker build -f tools/demarkus-broker/Dockerfile -t demarkus-broker:dev .
#
# Smoke-tested end-to-end on kind via deploy/kind/up.sh --with-broker,
# which discovers against an in-cluster mock OIDC issuer.

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY protocol/go.mod protocol/go.sum protocol/
COPY tools/go.mod    tools/go.sum    tools/

COPY protocol/ protocol/
COPY tools/    tools/

ARG VERSION=dev
ENV CGO_ENABLED=0

RUN go build -C tools -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-broker ./demarkus-broker

FROM scratch
# Trust store for outbound TLS. The broker calls the OIDC issuer's
# discovery (.well-known/openid-configuration) and token endpoints over
# HTTPS; without /etc/ssl/certs/ca-certificates.crt, Go's TLS handshake
# fails certificate validation against the IdP.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/demarkus-broker /demarkus-broker
# Match the chart's podSecurityContext.runAsUser so the image runs
# non-root even when deployed outside k8s. Defense-in-depth on top of
# the chart's runAsNonRoot: true.
USER 65532:65532
ENTRYPOINT ["/demarkus-broker"]
