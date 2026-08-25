//go:build windows

package backup

import (
	"reflect"
	"testing"
)

func TestExtractServiceCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantCmd  string
		wantRest []string
		wantOK   bool
	}{
		{
			name:     "no service flag",
			args:     []string{"-config", "config.yaml"},
			wantCmd:  "",
			wantRest: []string{"-config", "config.yaml"},
			wantOK:   false,
		},
		{
			name:     "install with config passed through",
			args:     []string{"-service=install", "-config", "config.yaml"},
			wantCmd:  "install",
			wantRest: []string{"-config", "config.yaml"},
			wantOK:   true,
		},
		{
			name:     "stop alone",
			args:     []string{"-service=stop"},
			wantCmd:  "stop",
			wantRest: []string{},
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd, rest, ok := extractServiceCommand(tt.args)
			if cmd != tt.wantCmd || ok != tt.wantOK || !reflect.DeepEqual(rest, tt.wantRest) {
				t.Errorf("extractServiceCommand(%v) = (%q, %v, %v), want (%q, %v, %v)",
					tt.args, cmd, rest, ok, tt.wantCmd, tt.wantRest, tt.wantOK)
			}
		})
	}
}

func TestEventLogWriterDropsBlankLines(t *testing.T) {
	t.Parallel()

	// eventLogWriter dispatches non-blank lines by prefix to its
	// *eventlog.Log, which needs a live Event Log connection to exercise;
	// this only checks the blank-line short circuit, which doesn't touch
	// w.log at all.
	w := &eventLogWriter{}

	for _, msg := range []string{"", "\n"} {
		if _, err := w.Write([]byte(msg)); err != nil {
			t.Errorf("Write(%q) = %v, want nil", msg, err)
		}
	}
}
