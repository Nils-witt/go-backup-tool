package store

import (
	"context"
	"testing"
	"time"
)

func TestSaveListLoginEventsNewestFirst(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	events := []LoginEvent{
		{At: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), Username: "admin", Method: "password", Success: true, RemoteAddr: "10.0.0.1:1"},
		{At: time.Date(2026, 1, 1, 3, 1, 0, 0, time.UTC), Username: "admin", Method: "password", Success: false, RemoteAddr: "10.0.0.2:1", Detail: "incorrect username or password"},
		{At: time.Date(2026, 1, 1, 3, 2, 0, 0, time.UTC), Username: "person@example.com", Method: "oidc", Success: true, RemoteAddr: "10.0.0.3:1"},
	}

	for _, ev := range events {
		if err := db.SaveLoginEvent(ctx, ev); err != nil {
			t.Fatalf("SaveLoginEvent() error: %v", err)
		}
	}

	got, err := db.ListLoginEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ListLoginEvents() error: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("ListLoginEvents() returned %d events, want 3", len(got))
	}

	if !got[0].At.Equal(events[2].At) || got[0].Username != "person@example.com" || got[0].Method != "oidc" || !got[0].Success {
		t.Errorf("ListLoginEvents()[0] = %+v, want the most recently recorded event first", got[0])
	}

	if got[1].Success || got[1].Detail != "incorrect username or password" {
		t.Errorf("ListLoginEvents()[1] = %+v, want the failed password attempt with its detail", got[1])
	}
}

func TestListLoginEventsRespectsLimit(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	for i := range 5 {
		ev := LoginEvent{At: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC), Username: "admin", Method: "password", Success: true, RemoteAddr: "10.0.0.1:1"}
		if err := db.SaveLoginEvent(ctx, ev); err != nil {
			t.Fatalf("SaveLoginEvent() error: %v", err)
		}
	}

	got, err := db.ListLoginEvents(ctx, 2)
	if err != nil {
		t.Fatalf("ListLoginEvents() error: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("ListLoginEvents(limit=2) returned %d events, want 2", len(got))
	}
}

func TestListLoginEventsEmpty(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	got, err := db.ListLoginEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListLoginEvents() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListLoginEvents() on an empty log = %+v, want none", got)
	}
}

func TestSaveListDownloadEventsNewestFirst(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	events := []DownloadEvent{
		{At: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), Username: "admin", ReceiverID: "a", Key: "backup.gpg", Success: true, RemoteAddr: "10.0.0.1:1"},
		{At: time.Date(2026, 1, 1, 3, 1, 0, 0, time.UTC), Username: "admin", ReceiverID: "a", Key: "missing.gpg", Success: false, RemoteAddr: "10.0.0.2:1", Detail: "not found"},
	}

	for _, ev := range events {
		if err := db.SaveDownloadEvent(ctx, ev); err != nil {
			t.Fatalf("SaveDownloadEvent() error: %v", err)
		}
	}

	got, err := db.ListDownloadEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ListDownloadEvents() error: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("ListDownloadEvents() returned %d events, want 2", len(got))
	}

	if got[0].Success || got[0].Key != "missing.gpg" || got[0].Detail != "not found" {
		t.Errorf("ListDownloadEvents()[0] = %+v, want the most recently recorded (failed) attempt first", got[0])
	}

	if !got[1].At.Equal(events[0].At) || got[1].Username != "admin" || got[1].ReceiverID != "a" || !got[1].Success {
		t.Errorf("ListDownloadEvents()[1] = %+v, want the earlier successful download", got[1])
	}
}

func TestListDownloadEventsRespectsLimit(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	for i := range 5 {
		ev := DownloadEvent{At: time.Date(2026, 1, 1, 0, i, 0, 0, time.UTC), Username: "admin", ReceiverID: "a", Key: "backup.gpg", Success: true, RemoteAddr: "10.0.0.1:1"}
		if err := db.SaveDownloadEvent(ctx, ev); err != nil {
			t.Fatalf("SaveDownloadEvent() error: %v", err)
		}
	}

	got, err := db.ListDownloadEvents(ctx, 2)
	if err != nil {
		t.Fatalf("ListDownloadEvents() error: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("ListDownloadEvents(limit=2) returned %d events, want 2", len(got))
	}
}

func TestListDownloadEventsEmpty(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	got, err := db.ListDownloadEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListDownloadEvents() error: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("ListDownloadEvents() on an empty log = %+v, want none", got)
	}
}

func TestGetLastReceiverEventReturnsMostRecent(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)
	ctx := context.Background()

	older := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	events := []ReceiverEvent{
		{At: older, ReceiverID: "recv-a", Kind: ReceiverEventReceive, Key: "a1.gpg", Size: 100, Success: true},
		{At: newer, ReceiverID: "recv-a", Kind: ReceiverEventDelete, Key: "a2.gpg", Success: false, Error: "disk full"},
		{At: newer, ReceiverID: "recv-b", Kind: ReceiverEventReceive, Key: "b1.gpg", Size: 50, Success: true},
	}

	for _, ev := range events {
		if err := db.SaveReceiverEvent(ctx, ev); err != nil {
			t.Fatalf("SaveReceiverEvent() error: %v", err)
		}
	}

	got, ok, err := db.GetLastReceiverEvent(ctx, "recv-a")
	if err != nil {
		t.Fatalf("GetLastReceiverEvent() error: %v", err)
	}

	if !ok {
		t.Fatal("GetLastReceiverEvent() ok = false, want true")
	}

	want := events[1]
	if !got.At.Equal(want.At) || got.Kind != want.Kind || got.Key != want.Key || got.Success != want.Success || got.Error != want.Error {
		t.Errorf("GetLastReceiverEvent() = %+v, want %+v", got, want)
	}
}

func TestGetLastReceiverEventUnknownReceiver(t *testing.T) {
	t.Parallel()

	db := openTestStore(t)

	_, ok, err := db.GetLastReceiverEvent(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("GetLastReceiverEvent() error: %v", err)
	}

	if ok {
		t.Error("GetLastReceiverEvent() ok = true, want false for a receiver with no events")
	}
}
