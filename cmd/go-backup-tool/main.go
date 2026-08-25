// Command go-backup-tool runs a shell command, encrypts its output with gpg,
// and streams the ciphertext to one or more targets: an S3 (or
// S3-compatible) bucket, or a directory on the local filesystem.
package main

import (
	"go-backup-tool/internal/backup"
	"os"
)

func main() {
	os.Exit(backup.Main(os.Args[1:], os.Stderr))
}
