// Package backup implements go-backup-tool's pipeline: run a shell command,
// encrypt its stdout with gpg, stage the ciphertext to a local temp file,
// then upload that file to one or more targets: a directory on the local
// filesystem, or another go-backup-tool instance's receiver API. Each
// target uploads independently from the staged file, so one target's
// trouble never requires re-running the backup command or gpg, and never
// affects any other target.
package backup
