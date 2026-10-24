# The upstream MinIO repository is archived and no longer publishes images,
# so the last release is built from source at a pinned tag (dev only).
FROM golang:1.24.8-alpine3.22 AS build
RUN apk add --no-cache git
ARG MINIO_TAG=RELEASE.2025-10-15T17-29-55Z
ARG MC_TAG=RELEASE.2025-08-13T08-35-41Z
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
RUN git clone --depth 1 --branch ${MINIO_TAG} https://github.com/minio/minio.git /src/minio \
 && cd /src/minio && go build -trimpath -ldflags "-s -w" -o /out/minio .
RUN git clone --depth 1 --branch ${MC_TAG} https://github.com/minio/mc.git /src/mc \
 && cd /src/mc && go build -trimpath -ldflags "-s -w" -o /out/mc .

FROM alpine:3.22.2
RUN apk add --no-cache ca-certificates curl
COPY --from=build /out/minio /out/mc /usr/local/bin/
EXPOSE 9000 9001
CMD ["minio", "server", "/data", "--console-address", ":9001"]
