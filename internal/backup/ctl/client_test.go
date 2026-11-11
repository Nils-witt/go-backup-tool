package ctl

import (
	"os"
	"path/filepath"
	"testing"
)

// Not parallel: it sets the environment.
func TestResolveBaseURL(t *testing.T) {
	dir := t.TempDir()

	writeConfig := func(name, contents string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}

		return path
	}

	enabled := writeConfig("enabled.yaml", "webui:\n  enabled: true\n  listen: \":9090\"\n")
	disabled := writeConfig("disabled.yaml", "log-level: info\n")

	tests := []struct {
		name    string
		opts    options
		env     string
		want    string
		wantErr bool
	}{
		{name: "flag wins", opts: options{url: "http://flag:1", configPath: enabled}, env: "http://env:2", want: "http://flag:1"},
		{name: "env before config", opts: options{configPath: enabled}, env: "https://env:2/base", want: "https://env:2/base"},
		{name: "config listen", opts: options{configPath: enabled}, want: "http://localhost:9090"},
		{name: "web ui disabled", opts: options{configPath: disabled}, wantErr: true},
		{name: "missing default config", opts: options{configPath: filepath.Join(dir, "none.yaml")}, wantErr: true},
		{name: "missing explicit config", opts: options{configPath: filepath.Join(dir, "none.yaml"), configExplicit: true}, wantErr: true},
		{name: "not http", opts: options{url: "ftp://host"}, wantErr: true},
		{name: "no host", opts: options{url: "http://"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(urlEnv, tt.env)

			got, err := resolveBaseURL(tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveBaseURL() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err == nil && got.String() != tt.want {
				t.Errorf("resolveBaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestListenURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		listen  string
		want    string
		wantErr bool
	}{
		{listen: ":8080", want: "http://localhost:8080"},
		{listen: "0.0.0.0:8080", want: "http://localhost:8080"},
		{listen: "[::]:8080", want: "http://localhost:8080"},
		{listen: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{listen: "[::1]:8080", want: "http://[::1]:8080"},
		{listen: "backup.lan:8080", want: "http://backup.lan:8080"},
		{listen: "8080", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			t.Parallel()

			got, err := listenURL(tt.listen)
			if (err != nil) != tt.wantErr {
				t.Fatalf("listenURL() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err == nil && got.String() != tt.want {
				t.Errorf("listenURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
