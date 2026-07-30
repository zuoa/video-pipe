# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src

# Copy the module + sources, resolve dependencies, and build a static binary.
# modernc.org/sqlite is pure-Go, so CGO is disabled for a static, single-binary image.
COPY . .
RUN go mod tidy \
 && CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/video-pipe ./cmd/video-pipe

# ---- runtime stage ----
FROM alpine:3.20
# ffmpeg is required (the supervisor spawns one per stream); ca-certificates for
# the MediaMTX API client (and TLS sources); tzdata for sane timestamps.
RUN apk add --no-cache ffmpeg ca-certificates tzdata

WORKDIR /app
COPY --from=build /out/video-pipe /app/video-pipe

EXPOSE 8080
ENTRYPOINT ["/app/video-pipe"]
