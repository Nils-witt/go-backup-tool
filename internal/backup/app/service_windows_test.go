//go:build windows

package app

import (
	"path/filepath"
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

func TestResolveConfigArgAbs(t *testing.T) {
	t.Parallel()

	wantAbs := func(t *testing.T, rel string) string {
		t.Helper()

		abs, err := filepath.Abs(rel)
		if err != nil {
			t.Fatalf("filepath.Abs(%q): %v", rel, err)
		}

		return abs
	}

	tests := []struct {
		name    string
		args    []string
		want    func(t *testing.T) []string
		wantErr bool
	}{
		{
			name: "no -config flag: one is appended pointing at the default",
			args: []string{"-job", "db"},
			want: func(t *testing.T) []string {
				return []string{"-job", "db", "-config=" + wantAbs(t, defaultConfigPath)}
			},
		},
		{
			name: "-config=value form is resolved in place",
			args: []string{"-config=config.yaml", "-job", "db"},
			want: func(t *testing.T) []string {
				return []string{"-config=" + wantAbs(t, "config.yaml"), "-job", "db"}
			},
		},
		{
			name: "-config value (separate token) form is resolved in place",
			args: []string{"-job", "db", "-config", "config.yaml"},
			want: func(t *testing.T) []string {
				return []string{"-job", "db", "-config", wantAbs(t, "config.yaml")}
			},
		},
		{
			name:    "-config with no following value is an error",
			args:    []string{"-job", "db", "-config"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveConfigArgAbs(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveConfigArgAbs(%v) = %v, want error", tt.args, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveConfigArgAbs(%v) unexpected error: %v", tt.args, err)
			}

			if want := tt.want(t); !reflect.DeepEqual(got, want) {
				t.Errorf("resolveConfigArgAbs(%v) = %v, want %v", tt.args, got, want)
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
