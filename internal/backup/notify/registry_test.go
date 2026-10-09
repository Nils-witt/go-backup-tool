package notify

import (
	"log/slog"
	"slices"
	"testing"
)

func TestRegistryResolveSkipsUnknownIDs(t *testing.T) {
	t.Parallel()

	r := NewRegistry(map[string]Notification{"a": {ID: "a"}, "b": {ID: "b"}})

	got := r.Resolve([]string{"b", "missing", "a"}, slog.New(slog.DiscardHandler))
	if len(got) != 2 || got[0].ID != "b" || got[1].ID != "a" {
		t.Errorf("Resolve() = %+v, want b then a", got)
	}

	r.Delete("a")
	r.Put(Notification{ID: "c"})

	if ids := r.IDs(); !slices.Equal(ids, []string{"b", "c"}) {
		t.Errorf("IDs() = %v, want [b c]", ids)
	}
}

func TestNilRegistryResolvesNothing(t *testing.T) {
	t.Parallel()

	var r *Registry

	if got := r.Resolve([]string{"a"}, slog.New(slog.DiscardHandler)); len(got) != 0 {
		t.Errorf("nil Resolve() = %+v, want none", got)
	}

	if ids := r.IDs(); ids != nil {
		t.Errorf("nil IDs() = %v, want nil", ids)
	}
}

func TestResolveNotification(t *testing.T) {
	t.Parallel()

	smtp := SMTPSettings{Host: "smtp.example.com", Port: 587, Username: "backups@example.com"}

	for name, tc := range map[string]struct {
		fn      FileNotification
		smtp    SMTPSettings
		wantErr bool
	}{
		"webhook":          {fn: FileNotification{ID: "a", Webhook: &FileWebhook{URL: "https://x"}}},
		"email":            {fn: FileNotification{ID: "a", Email: &FileEmail{To: []string{"o@example.com"}}}, smtp: smtp},
		"blank id":         {fn: FileNotification{ID: " ", Webhook: &FileWebhook{URL: "https://x"}}, wantErr: true},
		"no channel":       {fn: FileNotification{ID: "a"}, wantErr: true},
		"email no smtp":    {fn: FileNotification{ID: "a", Email: &FileEmail{To: []string{"o@example.com"}}}, wantErr: true},
		"webhook no url":   {fn: FileNotification{ID: "a", Webhook: &FileWebhook{}}, wantErr: true},
		"email no address": {fn: FileNotification{ID: "a", Email: &FileEmail{}}, smtp: smtp, wantErr: true},
	} {
		_, err := ResolveNotification(tc.fn, tc.smtp, GPGSettings{})
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: ResolveNotification() error = %v, wantErr %v", name, err, tc.wantErr)
		}
	}
}
