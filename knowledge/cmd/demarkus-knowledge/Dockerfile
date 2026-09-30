# The knowledge server and the broker in one process. GCS access needs the
# CA bundle distroless/static ships. Build context is the repo root; the
# binary is staged by the release workflow or `make image-knowledge`.
FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
ARG TARGETVARIANT
COPY dist/docker/${TARGETARCH}${TARGETVARIANT}/demarkus-knowledge /demarkus-knowledge
USER 65532:65532
EXPOSE 6309/udp
EXPOSE 8080/tcp
EXPOSE 8081/tcp
ENTRYPOINT ["/demarkus-knowledge"]
