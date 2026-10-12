package webui

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestGzipStatic(t *testing.T) {
	t.Parallel()

	bigJS := strings.Repeat("console.log('compress me');\n", 500)
	fsys := fstest.MapFS{
		"app.js":   {Data: []byte(bigJS)},
		"tiny.css": {Data: []byte("a{}")},
		"logo.png": {Data: []byte(strings.Repeat("\x89PNG", 500))},
	}
	h := gzipStatic(fsys, http.FileServerFS(fsys))

	tests := []struct {
		name           string
		method         string
		path           string
		acceptEncoding string
		wantGzip       bool
		wantVary       bool
	}{
		{name: "gzip accepted", method: http.MethodGet, path: "/app.js", acceptEncoding: "gzip, deflate, br", wantGzip: true, wantVary: true},
		{name: "gzip with quality", method: http.MethodGet, path: "/app.js", acceptEncoding: "br;q=1.0, gzip;q=0.8", wantGzip: true, wantVary: true},
		{name: "gzip refused with q=0", method: http.MethodGet, path: "/app.js", acceptEncoding: "gzip;q=0", wantVary: true},
		{name: "no Accept-Encoding", method: http.MethodGet, path: "/app.js", wantVary: true},
		{name: "HEAD gets gzip headers", method: http.MethodHead, path: "/app.js", acceptEncoding: "gzip", wantGzip: true, wantVary: true},
		{name: "too small to gain", method: http.MethodGet, path: "/tiny.css", acceptEncoding: "gzip", wantVary: true},
		{name: "not a compressible type", method: http.MethodGet, path: "/logo.png", acceptEncoding: "gzip"},
		{name: "unknown file", method: http.MethodGet, path: "/missing.js", acceptEncoding: "gzip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			gotGzip := rec.Header().Get("Content-Encoding") == "gzip"
			if gotGzip != tt.wantGzip {
				t.Fatalf("Content-Encoding = %q, want gzip: %v", rec.Header().Get("Content-Encoding"), tt.wantGzip)
			}

			if gotVary := rec.Header().Get("Vary") == "Accept-Encoding"; gotVary != tt.wantVary {
				t.Errorf("Vary = %q, want Accept-Encoding: %v", rec.Header().Get("Vary"), tt.wantVary)
			}

			if !gotGzip || tt.method == http.MethodHead {
				return
			}

			if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
				t.Errorf("Content-Type = %q, want text/javascript", ct)
			}

			if cl := rec.Header().Get("Content-Length"); cl == "" || rec.Body.Len() >= len(bigJS) {
				t.Errorf("Content-Length = %q, body %d bytes, want a set length smaller than %d", cl, rec.Body.Len(), len(bigJS))
			}

			zr, err := gzip.NewReader(rec.Body)
			if err != nil {
				t.Fatalf("gzip.NewReader: %v", err)
			}

			got, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("decompressing: %v", err)
			}

			if string(got) != bigJS {
				t.Error("decompressed body differs from the original file")
			}
		})
	}
}

func TestGzipStaticHEADHasNoBody(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{"app.js": {Data: []byte(strings.Repeat("x = 1;\n", 500))}}
	h := gzipStatic(fsys, http.FileServerFS(fsys))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes, want none", rec.Body.Len())
	}
}

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values []string
		want   bool
	}{
		{name: "absent", want: false},
		{name: "plain gzip", values: []string{"gzip"}, want: true},
		{name: "among others", values: []string{"deflate, gzip, br"}, want: true},
		{name: "uppercase", values: []string{"GZIP"}, want: true},
		{name: "nonzero quality", values: []string{"gzip;q=0.5"}, want: true},
		{name: "spaced quality", values: []string{"gzip ; q = 0.5"}, want: true},
		{name: "zero quality", values: []string{"gzip;q=0"}, want: false},
		{name: "zero quality with decimals", values: []string{"gzip;q=0.000"}, want: false},
		{name: "only other codings", values: []string{"br, deflate"}, want: false},
		{name: "x-gzip is not gzip", values: []string{"x-gzip"}, want: false},
		{name: "second header line", values: []string{"br", "gzip"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			for _, v := range tt.values {
				req.Header.Add("Accept-Encoding", v)
			}

			if got := acceptsGzip(req); got != tt.want {
				t.Errorf("acceptsGzip(%q) = %v, want %v", tt.values, got, tt.want)
			}
		})
	}
}
