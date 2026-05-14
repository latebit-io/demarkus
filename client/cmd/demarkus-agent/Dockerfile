# demarkus-agent image. The agent is a daemon crawler with no probes
# today (per chart values default), so the CLI is not bundled.
#
# Pre-built binary is staged into dist/docker/<arch>/ by the release
# workflow (and by `make image-agent` for local dev). The Dockerfile is
# COPY-only — no `RUN` step — so a buildx multi-arch build under QEMU
# completes in seconds instead of minutes.
#
# Base is distroless/static:nonroot — it ships /etc/ssl/certs/ca-certificates.crt
# so the agent's outbound mark:// crawls validate peer TLS when the
# chart's `insecure: false` production default is on. Runs as UID 65532
# by default; defense-in-depth USER matches the chart's
# podSecurityContext.runAsUser.
#
# Build context = repo root. Staged paths the Dockerfile expects:
#   dist/docker/amd64/demarkus-agent
#   dist/docker/arm64/demarkus-agent
#   dist/docker/armv7/demarkus-agent

FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
ARG TARGETVARIANT
COPY dist/docker/${TARGETARCH}${TARGETVARIANT}/demarkus-agent /demarkus-agent
USER 65532:65532
ENTRYPOINT ["/demarkus-agent"]
