# demarkus-memory-broker image. Single binary; the broker is HTTP-only and uses
# HTTP /healthz + /readyz probes, so the CLI is not bundled.
#
# Pre-built binary is staged into dist/docker/<arch>/ by the release
# workflow (and by `make image-memory-broker` for local dev). The Dockerfile is
# COPY-only — no `RUN` step — so a buildx multi-arch build under QEMU
# completes in seconds instead of minutes.
#
# Base is distroless/static:nonroot — it ships /etc/ssl/certs/ca-certificates.crt
# for the broker's outbound HTTPS to the OIDC issuer (discovery + token
# exchange), and runs as UID 65532 by default. Defense-in-depth USER
# directive matches the chart's podSecurityContext.runAsUser.
#
# Build context = repo root. Staged paths the Dockerfile expects:
#   dist/docker/amd64/demarkus-memory-broker
#   dist/docker/arm64/demarkus-memory-broker
#   dist/docker/armv7/demarkus-memory-broker

FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
ARG TARGETVARIANT
COPY dist/docker/${TARGETARCH}${TARGETVARIANT}/demarkus-memory-broker /demarkus-memory-broker
USER 65532:65532
ENTRYPOINT ["/demarkus-memory-broker"]
