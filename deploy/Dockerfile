# Builds one Go service: docker build --build-arg SERVICE=search-api -f deploy/Dockerfile .
# Services are built with cgo and ship ONNX Runtime: search-api scores with in-process ONNX
# models and control-api dry-runs uploaded ones (onnxruntime_go loads the library with dlopen).
FROM golang:1.27.3-bookworm AS build
ARG ONNXRUNTIME_VERSION=1.31.0
RUN curl -fsSL https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-x64-${ONNXRUNTIME_VERSION}.tgz \
      | tar -xz -C /opt && mv /opt/onnxruntime-linux-x64-${ONNXRUNTIME_VERSION} /opt/onnxruntime
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SERVICE
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o /out/app ./cmd/${SERVICE}

FROM gcr.io/distroless/cc-debian12:nonroot
ARG ONNXRUNTIME_VERSION=1.31.0
COPY --from=build /opt/onnxruntime/lib/libonnxruntime.so.${ONNXRUNTIME_VERSION} /usr/local/lib/libonnxruntime.so
COPY --from=build /out/app /app
ENV ONNXRUNTIME_LIB=/usr/local/lib/libonnxruntime.so
USER nonroot:nonroot
ENTRYPOINT ["/app"]
