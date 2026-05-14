# demarkus-server image. Bundles:
#   /demarkus-server  — the QUIC world server (ENTRYPOINT)
#   /demarkus         — CLI; the helm chart's exec probes invoke it
#                       against the local server's agent-manifest path
#
# Pre-built binaries are staged into dist/docker/<arch>/ by the release
# workflow (and by `make image-server` for local dev). The Dockerfile is
# COPY-only — no `RUN` step — so a buildx multi-arch build under QEMU
# completes in seconds instead of minutes (no Go compile under emulation).
#
# Build context = repo root. Staged paths the Dockerfile expects:
#   dist/docker/amd64/{demarkus-server,demarkus}
#   dist/docker/arm64/{demarkus-server,demarkus}
#   dist/docker/armv7/{demarkus-server,demarkus}
#
# Admin CLIs (demarkus-token, demarkus-publish) are NOT bundled — they
# ship as standalone binaries via tools/.goreleaser.yml so the running
# server pod's blast radius stays minimal.

FROM scratch
ARG TARGETARCH
ARG TARGETVARIANT
COPY dist/docker/${TARGETARCH}${TARGETVARIANT}/demarkus-server /demarkus-server
COPY dist/docker/${TARGETARCH}${TARGETVARIANT}/demarkus        /demarkus
# Match the chart's podSecurityContext.runAsUser so the image runs
# non-root even when deployed outside k8s (plain `docker run`, CI
# runners, etc.). Defense-in-depth on top of the chart's runAsNonRoot.
USER 65532:65532
EXPOSE 6309/udp
ENTRYPOINT ["/demarkus-server"]
