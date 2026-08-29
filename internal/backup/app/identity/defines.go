// Package identity manages this instance's persistent identity: an RSA key
// pair and UUID, used to sign and verify requests to remote targets.
package identity

// Server identity file names and key parameters, all rooted under a
// configurable keys directory (see backup.RunConfig.KeysDir, defaulting to
// DefaultServerKeyDir).
const (
	DefaultServerKeyDir = "data/keys"

	ServerPrivateKeyFile = "server.key"
	ServerPublicKeyFile  = "server.pub"
	ServerKeyBits        = 4096
	ServerUUIDFile       = "server.uuid"
)
