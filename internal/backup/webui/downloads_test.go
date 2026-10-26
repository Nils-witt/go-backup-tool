package webui

import (
	"errors"
	"testing"
	"time"
)

// TestDownloadTicketStoreSweepsAndCaps checks that minting a ticket drops
// expired ones and that one user can't hold more than
// maxOpenDownloadTickets at once, while another still can mint.
func TestDownloadTicketStoreSweepsAndCaps(t *testing.T) {
	t.Parallel()

	s := newDownloadTicketStore()
	s.byID["stale"] = downloadTicketEntry{receiverID: "a", key: "k", username: "alice", expires: time.Now().Add(-time.Second)}

	for i := range maxOpenDownloadTickets {
		if _, err := s.create("a", "k", "alice"); err != nil {
			t.Fatalf("create() #%d error: %v", i, err)
		}
	}

	if _, ok := s.byID["stale"]; ok {
		t.Error("expired ticket still stored after create()")
	}

	if _, err := s.create("a", "k", "alice"); !errors.Is(err, errTooManyDownloadTickets) {
		t.Errorf("create() over the cap error = %v, want errTooManyDownloadTickets", err)
	}

	if _, err := s.create("a", "k", "bob"); err != nil {
		t.Errorf("create() for another user error = %v, want nil", err)
	}

	if got := len(s.byID); got != maxOpenDownloadTickets+1 {
		t.Errorf("stored tickets = %d, want %d", got, maxOpenDownloadTickets+1)
	}
}
