package store

import (
	"testing"
	"time"
)

func TestPeerStatuses(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if got, err := db.ListPeerStatuses(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ListPeerStatuses() on an empty db = (%+v, %v), want (empty, nil)", got, err)
	}

	for _, p := range []PeerStatus{
		{ServerID: "b", ReceivedAt: at, Payload: []byte(`{"v":1}`)},
		{ServerID: "a", ReceivedAt: at, Payload: []byte(`{"v":1}`)},
		{ServerID: "b", ReceivedAt: at.Add(time.Minute), Payload: []byte(`{"v":2}`)},
	} {
		if err := db.SavePeerStatus(ctx, p); err != nil {
			t.Fatalf("SavePeerStatus(%s) error: %v", p.ServerID, err)
		}
	}

	got, err := db.ListPeerStatuses(ctx)
	if err != nil {
		t.Fatalf("ListPeerStatuses() error: %v", err)
	}

	if len(got) != 2 || got[0].ServerID != "a" || got[1].ServerID != "b" {
		t.Fatalf("ListPeerStatuses() = %+v, want a then b", got)
	}

	if b := got[1]; string(b.Payload) != `{"v":2}` || !b.ReceivedAt.Equal(at.Add(time.Minute)) {
		t.Errorf("ListPeerStatuses()[1] = %+v, want the second save's payload and time", b)
	}
}

func TestDeletePeerStatus(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := t.Context()

	for _, id := range []string{"a", "b"} {
		if err := db.SavePeerStatus(ctx, PeerStatus{ServerID: id, ReceivedAt: time.Now(), Payload: []byte(`{}`)}); err != nil {
			t.Fatalf("SavePeerStatus(%s) error: %v", id, err)
		}
	}

	if err := db.DeletePeerStatus(ctx, "a"); err != nil {
		t.Fatalf("DeletePeerStatus(a) error: %v", err)
	}

	if err := db.DeletePeerStatus(ctx, "missing"); err != nil {
		t.Errorf("DeletePeerStatus(missing) error: %v, want nil", err)
	}

	if got, err := db.ListPeerStatuses(ctx); err != nil || len(got) != 1 || got[0].ServerID != "b" {
		t.Errorf("ListPeerStatuses() after delete = (%+v, %v), want only b", got, err)
	}
}
