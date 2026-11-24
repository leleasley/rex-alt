# syntax=docker/dockerfile:1

# Build stage: Go toolchain never ships in the runtime image.
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/rex-alt ./cmd/rex-alt

# Runtime stage: only the static binary, CA roots and ffprobe.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates ffmpeg \
    && addgroup -g 1000 -S rex-alt \
    && adduser -u 1000 -S -G rex-alt -h /data rex-alt \
    && mkdir -p /data \
    && chown 1000:1000 /data
COPY --from=builder /out/rex-alt /usr/local/bin/rex-alt

USER 1000:1000
ENV APP_HOST=0.0.0.0 \
    APP_PORT=9982 \
    DATABASE_PATH=/data/rex-alt.db
VOLUME ["/data"]
EXPOSE 9982
ENTRYPOINT ["/usr/local/bin/rex-alt"]
