package receiver

import (
	"testing"
	"time"
)

func TestParseCreationTime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		raw     string
		want    time.Time
		wantErr bool
	}{
		{name: "empty", raw: "", want: time.Time{}},
		{name: "valid past time", raw: "2026-01-01T11:00:00Z", want: now.Add(-time.Hour)},
		{name: "malformed", raw: "not-a-time", wantErr: true},
		{name: "equal to now", raw: "2026-01-01T12:00:00Z", wantErr: true},
		{name: "after now", raw: "2026-01-01T13:00:00Z", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseCreationTime(tt.raw, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCreationTime(%q) error = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}

			if err == nil && !got.Equal(tt.want) {
				t.Errorf("parseCreationTime(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
