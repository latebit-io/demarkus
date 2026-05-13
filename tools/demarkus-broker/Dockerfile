# demarkus-broker image. Single binary; the broker is HTTP-only and uses
# HTTP /healthz + /readyz probes, so the CLI is not bundled.
#
# Build context = repo root so the protocol/ module is reachable via the
# tools/ replace directive:
#   docker build -f tools/demarkus-broker/Dockerfile -t demarkus-broker:dev .

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY protocol/go.mod protocol/go.sum protocol/
COPY tools/go.mod    tools/go.sum    tools/

COPY protocol/ protocol/
COPY tools/    tools/

ARG VERSION=dev
ENV CGO_ENABLED=0

RUN cd tools && go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/demarkus-broker ./demarkus-broker

FROM scratch
COPY --from=build /out/demarkus-broker /demarkus-broker
ENTRYPOINT ["/demarkus-broker"]
