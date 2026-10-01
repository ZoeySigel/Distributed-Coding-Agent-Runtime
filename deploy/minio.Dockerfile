ARG BASE_REGISTRY=docker.io/library
FROM ${BASE_REGISTRY}/golang:1.24.13-bookworm AS build
# RELEASE.2025-04-22T22-12-26Z, pinned to its upstream commit.
ARG MINIO_COMMIT=0d7408fc9969caf07de6a8c3a84f9fbb10a6739e
RUN CGO_ENABLED=0 GOBIN=/out go install github.com/minio/minio@${MINIO_COMMIT} && \
    mkdir -p /out/licenses && \
    cp /go/pkg/mod/github.com/minio/minio@*/LICENSE /out/licenses/LICENSE && \
    cp /go/pkg/mod/github.com/minio/minio@*/NOTICE /out/licenses/NOTICE

FROM ${BASE_REGISTRY}/debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/* && mkdir /data && chown 1000:1000 /data
COPY --from=build /out/minio /usr/local/bin/minio
COPY --from=build /out/licenses/ /usr/share/licenses/minio/
USER 1000:1000
EXPOSE 9000
ENTRYPOINT ["minio"]
CMD ["server", "/data"]
