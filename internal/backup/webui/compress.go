package webui

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// gzipContentTypes are the asset types gzipStatic compresses, by extension,
// with the Content-Type each is served as. Everything else (images, fonts)
// is already compressed and is passed through untouched.
var gzipContentTypes = map[string]string{
	".css":  "text/css; charset=utf-8",
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".json": "application/json",
	".map":  "application/json",
	".mjs":  "text/javascript; charset=utf-8",
	".svg":  "image/svg+xml",
	".txt":  "text/plain; charset=utf-8",
}

// gzipStatic serves fsys's compressible files (see gzipContentTypes)
// gzip-encoded to clients that accept it, and hands every other request to
// next (normally a file server over the same fsys). Each file is
// compressed once, on its first gzip request, and kept for the life of the
// process: fsys is the embedded dashboard build, so its content never
// changes, and the set of cacheable names is fixed up front to the files
// fsys actually holds, so requests for made-up paths can't grow it.
func gzipStatic(fsys fs.FS, next http.Handler) http.Handler {
	compressed := map[string]func() []byte{}

	_ = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is just left uncompressed; next serves (or fails) it as usual
		}

		if _, ok := gzipContentTypes[path.Ext(name)]; ok {
			compressed[name] = sync.OnceValue(func() []byte { return gzipFile(fsys, name) })
		}

		return nil
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")

		gz, ok := compressed[name]
		if !ok {
			next.ServeHTTP(w, r)
			return
		}

		// The response now depends on Accept-Encoding either way, so
		// shared caches must key on it.
		w.Header().Add("Vary", "Accept-Encoding")

		var body []byte
		if acceptsGzip(r) {
			body = gz()
		}

		if body == nil {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", gzipContentTypes[path.Ext(name)])
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))

		if r.Method == http.MethodHead {
			return
		}

		_, _ = w.Write(body)
	})
}

// gzipFile returns name's content from fsys gzip-compressed, or nil if it
// can't be read or compressing wouldn't make it smaller — either way the
// caller then serves it uncompressed.
func gzipFile(fsys fs.FS, name string) []byte {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil
	}

	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil
	}

	if _, err := zw.Write(raw); err != nil {
		return nil
	}

	if err := zw.Close(); err != nil {
		return nil
	}

	if buf.Len() >= len(raw) {
		return nil
	}

	return buf.Bytes()
}

// acceptsGzip reports whether r's Accept-Encoding lists gzip with a nonzero
// quality value.
func acceptsGzip(r *http.Request) bool {
	for _, field := range r.Header.Values("Accept-Encoding") {
		for part := range strings.SplitSeq(field, ",") {
			coding, params, _ := strings.Cut(part, ";")
			if strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				return !zeroQuality(params)
			}
		}
	}

	return false
}

// zeroQuality reports whether params (an Accept-Encoding entry's
// parameters, e.g. " q=0") set q to zero, which means "not acceptable".
func zeroQuality(params string) bool {
	for param := range strings.SplitSeq(params, ";") {
		key, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}

		q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)

		// Exact comparison is intended: q values are decimal literals with
		// at most three digits, and only a literal zero means refusal.
		return err == nil && q == 0
	}

	return false
}
