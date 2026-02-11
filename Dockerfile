# Stage 1: Build the Go binary
FROM golang:1.24-bookworm AS builder

# Build arguments for version metadata
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

WORKDIR /app
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo \
    -ldflags "-X main.version=${VERSION} -X main.commitSHA=${COMMIT_SHA} -X main.buildDate=${BUILD_DATE}" \
    -o goduckbot .

# Stage 2: Create a minimal runtime image
FROM debian:bookworm-slim

# Build arguments for labels (passed from build stage)
ARG VERSION=dev
ARG COMMIT_SHA=unknown
ARG BUILD_DATE=unknown

# Labels for image metadata
LABEL org.opencontainers.image.title="goduckbot" \
      org.opencontainers.image.description="duckbot using Go and a local node" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT_SHA}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/wcatz/goduckbot"

WORKDIR /app
COPY --from=builder /app/goduckbot /app/goduckbot
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /app/data /ipc /keys
CMD ["/app/goduckbot"]
