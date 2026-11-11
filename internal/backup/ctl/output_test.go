package ctl

import (
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/status"
)

func TestNeedsAttention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		report statusReport
		want   bool
	}{
		{name: "empty", report: statusReport{}, want: false},
		{name: "ok job", report: statusReport{Jobs: []status.JobSnapshot{{State: status.StateOK}}}, want: false},
		{name: "running job", report: statusReport{Jobs: []status.JobSnapshot{{State: status.StateRunning}}}, want: false},
		{name: "failed job", report: statusReport{Jobs: []status.JobSnapshot{{State: status.StateFailed}}}, want: true},
		{name: "incomplete job", report: statusReport{Jobs: []status.JobSnapshot{{State: status.StateIncomplete}}}, want: true},
		{name: "failed target", report: statusReport{Jobs: []status.JobSnapshot{{State: status.StateRunning, Targets: []status.TargetSnapshot{{State: status.StateFailed}}}}}, want: true},
		{name: "stale receiver", report: statusReport{Receivers: []status.ReceiverSnapshot{{State: status.StateOK, Stale: true}}}, want: true},
		{name: "failed receiver", report: statusReport{Receivers: []status.ReceiverSnapshot{{State: status.StateFailed}}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := needsAttention(tt.report); got != tt.want {
				t.Errorf("needsAttention() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	t.Parallel()

	if got, want := sanitize("a\tb\nc\x1b[31md"), "a b c [31md"; got != want {
		t.Errorf("sanitize() = %q, want %q", got, want)
	}
}
