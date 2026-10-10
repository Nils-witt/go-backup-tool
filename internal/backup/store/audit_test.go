package store

import (
	"context"
	"testing"
	"time"
)

func TestSaveListAuditEventsNewestFirst(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	events := []AuditEvent{
		{At: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), Username: "erin", Action: "create", Resource: "receiver-configs", Target: "a", Method: "POST", Path: "/api/receiver-configs", Status: 201, Success: true, RemoteAddr: "10.0.0.1:1"},
		{At: time.Date(2026, 1, 1, 3, 1, 0, 0, time.UTC), Username: "erin", Action: "delete", Resource: "receiver-configs", Target: "b", Method: "DELETE", Path: "/api/receiver-configs/b", Status: 404, RemoteAddr: "10.0.0.1:1", Detail: "not found"},
	}

	for _, ev := range events {
		if err := db.SaveAuditEvent(ctx, ev); err != nil {
			t.Fatalf("SaveAuditEvent() error: %v", err)
		}
	}

	got, err := db.ListAuditEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ListAuditEvents() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("ListAuditEvents() returned %d events, want 2", len(got))
	}

	if !got[0].At.Equal(events[1].At) || got[0].Target != "b" || got[0].Success || got[0].Status != 404 || got[0].Detail != "not found" {
		t.Errorf("ListAuditEvents()[0] = %+v, want the most recently recorded event first", got[0])
	}

	if got[1].Action != "create" || got[1].Resource != "receiver-configs" || !got[1].Success {
		t.Errorf("ListAuditEvents()[1] = %+v", got[1])
	}
}

func TestPruneEventsRemovesOldAuditEvents(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	for _, at := range []time.Time{time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)} {
		if err := db.SaveAuditEvent(ctx, AuditEvent{At: at, Username: "erin", Action: "update", Resource: "report-config"}); err != nil {
			t.Fatalf("SaveAuditEvent() error: %v", err)
		}
	}

	n, err := db.PruneEvents(ctx, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("PruneEvents() error: %v", err)
	}

	if n != 1 {
		t.Errorf("PruneEvents() removed %d rows, want 1", n)
	}

	if got, _ := db.ListAuditEvents(ctx, 10); len(got) != 1 {
		t.Errorf("ListAuditEvents() after prune = %+v, want 1 event", got)
	}
}

func TestSaveListAuditEventChanges(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	for _, changes := range [][]AuditChange{{{Field: "path", New: "/srv/a"}}, nil} {
		if err := db.SaveAuditEvent(ctx, AuditEvent{At: time.Now(), Username: "erin", Action: "update", Resource: "receiver-configs", Changes: changes}); err != nil {
			t.Fatalf("SaveAuditEvent() error: %v", err)
		}
	}

	got, err := db.ListAuditEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ListAuditEvents() error: %v", err)
	}

	if got[0].Changes == nil || len(got[0].Changes) != 0 {
		t.Errorf("ListAuditEvents()[0].Changes = %#v, want empty, not nil", got[0].Changes)
	}

	if len(got[1].Changes) != 1 || got[1].Changes[0] != (AuditChange{Field: "path", New: "/srv/a"}) {
		t.Errorf("ListAuditEvents()[1].Changes = %+v", got[1].Changes)
	}
}
