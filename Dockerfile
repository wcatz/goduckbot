# Stage 1: Build the Go binary
FROM golang:1.24-bookworm AS builder
WORKDIR /app
COPY . .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o goduckbot .

# Stage 2: Create a minimal runtime image
FROM debian:bookworm-slim
WORKDIR /app
COPY --from=builder /app/goduckbot /app/goduckbot
# Add runtime dependencies if needed
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
CMD ["/app/goduckbot"]