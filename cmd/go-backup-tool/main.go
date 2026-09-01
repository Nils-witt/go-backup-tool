// Command go-backup-tool runs a shell command, encrypts its output with gpg,
// and streams the ciphertext to one or more targets: a directory on the
// local filesystem, or another go-backup-tool instance's receiver API.
package main

import (
	"os"

	"nilswitt.dev/go-backup-tool/internal/backup/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:], os.Stderr))
}
