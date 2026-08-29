# syntax=docker/dockerfile:1
FROM golang:alpine AS builder

WORKDIR /src

ARG VERSION=dev
ARG COMMIT=unknown

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# CGO is off on purpose: the sqlite driver (modernc.org/sqlite) is pure Go,
# so the binary builds static with no libc/gcc dependency in the final image.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X go-backup-tool/internal/backup.version=${VERSION} -X go-backup-tool/internal/backup.commit=${COMMIT}" -o /out/go-backup-tool ./cmd/go-backup-tool

FROM alpine:latest

# ca-certificates: TLS to S3-compatible/remote endpoints.
# gnupg: go-backup-tool shells out to the "gpg" binary to encrypt every backup.
RUN apk add --no-cache ca-certificates gnupg tzdata \
    && addgroup -S backup && adduser -S backup -G backup

COPY --from=builder /out/go-backup-tool /usr/local/bin/go-backup-tool
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# The config file's directory also holds the sqlite state/retention
# databases go-backup-tool writes next to it, so mount both a config.yaml
# and a persistent volume at /data (default -config path is config.yaml,
# resolved relative to this working directory).
WORKDIR /data
RUN chown backup:backup /data
USER backup

# Only used when the config file sets listen: (disabled by default).
EXPOSE 8080

# docker-entrypoint.sh imports every public key file under GPG_KEYS_DIR
# (default /data/keys) into GNUPGHOME (default /data/.gnupg, persisted on
# the same volume as /data) before exec'ing go-backup-tool, so recipients:
# in config.yaml can reference keys dropped into that directory instead of
# requiring a pre-built image/keychain.
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
