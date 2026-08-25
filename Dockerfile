# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# CGO is off on purpose: the sqlite driver (modernc.org/sqlite) is pure Go,
# so the binary builds static with no libc/gcc dependency in the final image.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/go-backup-tool ./cmd/go-backup-tool

FROM alpine:3.22

# ca-certificates: TLS to S3-compatible/remote endpoints.
# gnupg: go-backup-tool shells out to the "gpg" binary to encrypt every backup.
RUN apk add --no-cache ca-certificates gnupg tzdata \
    && addgroup -S backup && adduser -S backup -G backup

COPY --from=builder /out/go-backup-tool /usr/local/bin/go-backup-tool

# The config file's directory also holds the sqlite state/retention
# databases go-backup-tool writes next to it, so mount both a config.yaml
# and a persistent volume at /data (default -config path is config.yaml,
# resolved relative to this working directory).
WORKDIR /data
RUN chown backup:backup /data
USER backup

# Only used when the config file sets listen: (disabled by default).
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/go-backup-tool"]
