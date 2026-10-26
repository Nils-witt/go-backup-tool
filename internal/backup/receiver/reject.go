package receiver

import (
	"fmt"
	"log/slog"
	"net/http"

	"nilswitt.dev/go-backup-tool/internal/backup/remoteauth"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// ClientAddrFunc returns the address a receiver API request came from, as
// logged and recorded for a rejected request; the composition root passes
// the web UI's own (see webui.ClientAddr), so webui.trust-proxy-headers
// applies here too. Nil uses the request's raw TCP peer address.
type ClientAddrFunc func(*http.Request) string

// rejecter answers receiver API requests that fail authentication or
// request binding with 401, after logging each one as a warning and
// recording it in the receiver's event log as a store.ReceiverEventReject
// — so a misconfigured sender, a stolen or replayed token, or someone
// probing the API shows up in the dashboard's receiver log and the
// periodic report rather than only at debug level. Their volume is bounded
// by the web UI's rate limit on failed authentications (see
// webui.rate-limit).
type rejecter struct {
	log        *slog.Logger
	db         store.Store
	clientAddr ClientAddrFunc
}

// reject answers r with 401, logging and recording why (reason, kept out
// of the response) against receiverID and key, either of which may be
// unknown (""): an unknown receiver id isn't recorded, since there's no
// receiver to record it for. The issuer the token claims is logged
// unverified, as a hint at which sender it came from.
func (rj rejecter) reject(w http.ResponseWriter, r *http.Request, receiverID, key string, reason error) {
	client := r.RemoteAddr
	if rj.clientAddr != nil {
		client = rj.clientAddr(r)
	}

	var claimedIssuer string
	if token, ok := bearerToken(r); ok {
		claimedIssuer, _ = remoteauth.UnverifiedIssuer(token)
	}

	rj.log.Warn("receiver: rejected request", "id", receiverID, "key", key, "client", client, "claimed_issuer", claimedIssuer, "err", reason)

	if receiverID != "" {
		recordReceiverEventBestEffort(r.Context(), rj.db, rj.log, receiverID, store.ReceiverEventReject, key, 0,
			fmt.Errorf("rejected request from %s: %w", client, reason))
	}

	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
