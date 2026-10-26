// Package peerstatus lets instances show up on another instance's
// dashboard: each one pushes its own job and receiver status (RunPusher) to
// a central instance, which keeps the last push of each (Hub) and shows them
// read-only next to its own.
//
// A push is POST remoteclient.StatusPath with a JSON Payload, authenticated
// the same way as a receiver API upload: a short-lived JWT signed by the
// sender's instance key (see the remoteauth package), scoped to
// remoteclient.StatusAudience and bound to the body's SHA-256. The central
// instance accepts it from any of its trusted servers (see the trust
// package); unlike an upload, no receiver has to allow the sender. The
// central instance never connects to the pushing ones, so a sender behind
// NAT or a firewall can still report in, and the dashboard's
// Content-Security-Policy still only allows its own origin.
package peerstatus

import (
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

// Version is the Payload format this package writes and accepts.
const Version = 1

// maxBody bounds a push's body. Even hundreds of jobs and receivers stay
// far below it.
const maxBody = 4 << 20

// maxInterval bounds the push interval a Payload may claim, so a sender
// can't keep itself shown online for days after it stopped pushing.
const maxInterval = 24 * time.Hour

// Payload is one push: the sender's status as its own /api/status and
// /api/receivers serve it.
type Payload struct {
	Version int `json:"version"`

	// InstanceName is the sender's server-name:, "" if unset.
	InstanceName string `json:"instance_name,omitempty"`

	// IntervalSeconds is how long the sender goes without a push at the
	// longest, after which the central instance shows it offline once a few
	// of them pass (see status.PeerSnapshot.Offline).
	IntervalSeconds int `json:"interval_seconds"`

	SentAt    time.Time                 `json:"sent_at"`
	Jobs      []status.JobSnapshot      `json:"jobs"`
	Receivers []status.ReceiverSnapshot `json:"receivers"`
}

// interval returns p's push interval, clamped to (0, maxInterval].
func (p Payload) interval() time.Duration {
	d := time.Duration(p.IntervalSeconds) * time.Second
	if d <= 0 || d > maxInterval {
		return maxInterval
	}

	return d
}

// report returns p as serverID's status, received at receivedAt.
func (p Payload) report(serverID string, receivedAt time.Time) status.PeerReport {
	return status.PeerReport{
		ID:           serverID,
		InstanceName: p.InstanceName,
		Interval:     p.interval(),
		ReceivedAt:   receivedAt,
		Jobs:         p.Jobs,
		Receivers:    p.Receivers,
	}
}
