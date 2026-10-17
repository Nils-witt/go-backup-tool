package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ServerKind distinguishes a servers: entry's destination type. There is no
// default: every servers: entry must set an explicit type:.
type ServerKind string

// The ServerKind values a servers: entry's type: can resolve to.
const (
	ServerKindLocal  ServerKind = "local"  // type: local
	ServerKindRemote ServerKind = "remote" // type: remote
)

// parseServerKind validates a FileServer's Type field.
func parseServerKind(t string) (ServerKind, error) {
	switch strings.TrimSpace(t) {
	case string(ServerKindLocal):
		return ServerKindLocal, nil
	case string(ServerKindRemote):
		return ServerKindRemote, nil
	case "":
		return "", fmt.Errorf("type is required (want %q or %q)", ServerKindLocal, ServerKindRemote)
	default:
		return "", fmt.Errorf("unknown type %q (want %q or %q)", t, ServerKindLocal, ServerKindRemote)
	}
}

// buildServers resolves fileServers into a name -> ResolvedServer map,
// validating that every entry has a non-empty, unique name and an explicit,
// valid type:.
func buildServers(fileServers []FileServer) (map[string]ResolvedServer, error) {
	servers := make(map[string]ResolvedServer, len(fileServers))

	for i, fs := range fileServers {
		name := strings.TrimSpace(fs.Name)
		if name == "" {
			return nil, fmt.Errorf("servers[%d]: name is required", i)
		}

		if _, exists := servers[name]; exists {
			return nil, fmt.Errorf("servers[%d]: duplicate server name %q", i, name)
		}

		server, err := ResolveServer(fs)
		if err != nil {
			return nil, err
		}

		servers[name] = server
	}

	return servers, nil
}

// ResolveServer validates one servers: entry — from the config file or
// stored in the state db (see jobs.Manager) — into a ResolvedServer jobs'
// targets can be resolved against: a non-empty name and an explicit, valid
// type: with only the fields that type uses.
func ResolveServer(fs FileServer) (ResolvedServer, error) {
	name := strings.TrimSpace(fs.Name)
	if name == "" {
		return ResolvedServer{}, errors.New("name is required")
	}

	kind, err := parseServerKind(fs.Type)
	if err != nil {
		return ResolvedServer{}, fmt.Errorf("server %q: %w", name, err)
	}

	if kind == ServerKindLocal {
		return buildLocalServer(name, &fs)
	}

	return buildRemoteServer(name, &fs)
}

// buildLocalServer validates and builds a ResolvedServer for a type: local
// servers: entry, which uses only path and retention.
func buildLocalServer(name string, fs *FileServer) (ResolvedServer, error) {
	if strings.TrimSpace(fs.Path) == "" {
		return ResolvedServer{}, fmt.Errorf("server %q: path is required for type: local", name)
	}

	if fs.Endpoint != "" {
		return ResolvedServer{}, fmt.Errorf("server %q: endpoint is not valid for type: local", name)
	}

	if fs.ServerUUID != "" {
		return ResolvedServer{}, fmt.Errorf("server %q: server-uuid is not valid for type: local", name)
	}

	if !fileServerTLS(fs).IsZero() {
		return ResolvedServer{}, fmt.Errorf("server %q: tls-* settings are not valid for type: local", name)
	}

	retention, err := parseRetention(fs.Retention)
	if err != nil {
		return ResolvedServer{}, fmt.Errorf("server %q: %w", name, err)
	}

	return ResolvedServer{name: name, kind: ServerKindLocal, path: fs.Path, retention: retention}, nil
}

// buildRemoteServer validates and builds a ResolvedServer for a type: remote
// servers: entry, which uses only endpoint, the optional server-uuid and
// the optional tls-* settings — auth is this instance's own identity (see
// loadServerIdentity), not a config field.
func buildRemoteServer(name string, fs *FileServer) (ResolvedServer, error) {
	if strings.TrimSpace(fs.Endpoint) == "" {
		return ResolvedServer{}, fmt.Errorf("server %q: endpoint is required for type: remote", name)
	}

	if fs.Path != "" || fs.Retention != "" {
		return ResolvedServer{}, fmt.Errorf("server %q: path/retention are not valid for type: remote", name)
	}

	serverUUID := strings.TrimSpace(fs.ServerUUID)
	if serverUUID != "" {
		if _, err := uuid.Parse(serverUUID); err != nil {
			return ResolvedServer{}, fmt.Errorf("server %q: server-uuid %q is not a valid UUID", name, serverUUID)
		}
	}

	serverTLS, err := resolveServerTLS(fs.Endpoint, fileServerTLS(fs))
	if err != nil {
		return ResolvedServer{}, fmt.Errorf("server %q: %w", name, err)
	}

	return ResolvedServer{name: name, kind: ServerKindRemote, endpoint: fs.Endpoint, serverUUID: serverUUID, tls: serverTLS}, nil
}

// ResolvedServer is one servers: entry, ready to be combined with a job's
// targetRef bucket into a target.
type ResolvedServer struct {
	name       string
	kind       ServerKind
	endpoint   string
	serverUUID string        // remote only: the destination instance's server UUID, "" if not given
	path       string        // local only: root directory backups are written under
	retention  time.Duration // local only: 0 means no automatic expiry
	tls        TargetTLS     // remote only: certificate verification settings
}

// Kind is s's type:.
func (s ResolvedServer) Kind() ServerKind { return s.kind }
