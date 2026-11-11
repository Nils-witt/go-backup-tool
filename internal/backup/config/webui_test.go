package config

import (
	"path/filepath"
	"testing"
)

func TestWebUIListen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		contents string
		want     string
		wantErr  bool
	}{
		{name: "enabled", contents: "webui:\n  enabled: true\n  listen: \":9090\"\n", want: ":9090"},
		{name: "disabled", contents: "webui:\n  listen: \":9090\"\n", want: ""},
		{name: "no webui section", contents: "log-level: info\n", want: ""},
		// Other sections aren't validated: jobs referencing an unknown
		// server would fail ParseFlags, but not this.
		{name: "invalid jobs ignored", contents: "webui:\n  enabled: true\n  listen: \":8080\"\njobs:\n  - name: x\n    targets: [{server: nope}]\n", want: ":8080"},
		{name: "enabled without listen", contents: "webui:\n  enabled: true\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := WebUIListen(writeConfigFile(t, tt.contents), true)
			if (err != nil) != tt.wantErr {
				t.Fatalf("WebUIListen() error = %v, wantErr %v", err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("WebUIListen() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWebUIListenMissingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.yaml")

	if got, err := WebUIListen(path, false); err != nil || got != "" {
		t.Errorf("WebUIListen(missing, implicit) = %q, %v, want \"\", nil", got, err)
	}

	if _, err := WebUIListen(path, true); err == nil {
		t.Error("WebUIListen(missing, explicit) error = nil, want one")
	}
}
