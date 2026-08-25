#!/bin/sh
# Imports every public key file found in GPG_KEYS_DIR into the gpg keychain
# at GNUPGHOME before handing off to go-backup-tool. This lets recipients:
# in config.yaml reference keys that were never `gpg --import`-ed by hand —
# just bind-mount exported public keys (e.g. `gpg --export --armor
# me@example.com > keys/me.asc`) into GPG_KEYS_DIR.
set -eu

: "${GNUPGHOME:=/data/.gnupg}"
: "${GPG_KEYS_DIR:=/data/keys}"
export GNUPGHOME

mkdir -p "$GNUPGHOME"
chmod 700 "$GNUPGHOME"

if [ -d "$GPG_KEYS_DIR" ]; then
	for key in "$GPG_KEYS_DIR"/*; do
		[ -f "$key" ] || continue
		echo "docker-entrypoint: importing gpg key $key" >&2
		gpg --batch --import "$key" || echo "docker-entrypoint: warning: failed to import $key" >&2
	done
fi

exec /usr/local/bin/go-backup-tool "$@"
