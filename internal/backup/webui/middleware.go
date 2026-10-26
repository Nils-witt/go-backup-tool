package webui

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// logRequests wraps next, logging every request it handles at debug level
// once it completes: method, path, remote address, response status, and
// how long it took — the same per-request detail an operator would reach
// for a proper HTTP access log, without pulling in a logging middleware
// dependency for a handful of routes. A request whose handler noted an
// internal error (see noteResponseError) is also logged at warn level with
// that error, since the response itself only carries a generic message.
func logRequests(log *slog.Logger, next http.Handler, trustProxyHeaders bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, req)

		if sw.err != nil {
			log.Warn("web UI: request failed", "method", req.Method, "path", req.URL.Path, "status", sw.status, "err", sw.err)
		}

		log.Debug("web UI request",
			"method", req.Method,
			"path", req.URL.Path,
			"remote", clientAddr(req, trustProxyHeaders),
			"status", sw.status,
			"duration", time.Since(start),
		)
	})
}

// corsMiddleware adds permissive CORS response headers to every request
// next handles, and answers a CORS preflight (an OPTIONS request) itself
// rather than passing it on — mux has no route registered for OPTIONS on
// any path, so without this a preflight would otherwise 404/405 before next
// ever saw it. Only wired in when webui.dev-mode: is set (see StartWebUI):
// it lets a frontend dev server running on its own origin (e.g. `npm run
// dev`) call this instance's API directly, at the cost of letting any
// website's JavaScript read API responses from a browser that also holds a
// valid bearer token for this instance — never enable it against a
// production instance.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+idTokenHeader)
		w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// getCORSMiddleware enables CORS for read-only API access only
// (webui.cors-get-origins:, see StartWebUI): a GET/HEAD request to /api/...
// whose Origin is in allowed — or any Origin, if allowed holds "*" — gets
// an Access-Control-Allow-Origin header, and a preflight (OPTIONS) for such
// a request is answered here allowing GET/HEAD with an Authorization header,
// since mux has no OPTIONS routes. Any other method, path or origin passes
// through without CORS headers, so a browser keeps refusing cross-origin
// writes. No Access-Control-Allow-Credentials is sent: the API authorizes by
// bearer token, never by cookie.
func getCORSMiddleware(allowed []string, next http.Handler) http.Handler {
	anyOrigin := slices.Contains(allowed, "*")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Add("Vary", "Origin")

		origin := r.Header.Get("Origin")
		if origin == "" || (!anyOrigin && !slices.Contains(allowed, origin)) {
			next.ServeHTTP(w, r)
			return
		}

		allowOrigin := origin
		if anyOrigin {
			allowOrigin = "*"
		}

		switch r.Method {
		case http.MethodGet, http.MethodHead:
			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
		case http.MethodOptions:
			reqMethod := r.Header.Get("Access-Control-Request-Method")
			if reqMethod != http.MethodGet && reqMethod != http.MethodHead {
				break
			}

			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// ClientAddr is clientAddr for other packages sharing the web UI's HTTP
// server, e.g. the receiver API logging a rejected request, so
// webui.trust-proxy-headers applies to them the same way.
func ClientAddr(req *http.Request, trustProxyHeaders bool) string {
	return clientAddr(req, trustProxyHeaders)
}

// clientAddr returns the address to record as a request's origin: req's raw
// TCP peer (req.RemoteAddr) normally, or — when trustProxyHeaders is set,
// because this instance sits behind a reverse proxy — the original client's
// address as reported by that proxy, so logs reflect the real client rather
// than the proxy's own address. Only enable trustProxyHeaders when a proxy
// that itself sets these headers (and strips any client-supplied copies
// first) is guaranteed to be in front of every request; otherwise a client
// can spoof its own logged address by sending these headers itself.
//
// The standard Forwarded header (RFC 7239) is preferred, since its for=
// parameter can carry the client's port alongside its IP; the de facto
// X-Forwarded-For and X-Real-Ip headers only ever carry the IP.
func clientAddr(req *http.Request, trustProxyHeaders bool) string {
	if !trustProxyHeaders {
		return req.RemoteAddr
	}

	if fwd := req.Header.Get("Forwarded"); fwd != "" {
		if addr, ok := forwardedParam(fwd, "for"); ok {
			return addr
		}
	}

	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if ip := strings.TrimSpace(strings.Split(xff, ",")[0]); ip != "" {
			return ip
		}
	}

	if ip := strings.TrimSpace(req.Header.Get("X-Real-Ip")); ip != "" {
		return ip
	}

	return req.RemoteAddr
}

// forwardedHost returns the Host the client originally requested, as
// reported by a reverse proxy that rewrites the Host header on its way to
// this instance (e.g. to 127.0.0.1:8086): the standard Forwarded header's
// host= parameter (RFC 7239), else X-Forwarded-Host. Like clientAddr, only
// call it when trust-proxy-headers is set, since a client could otherwise
// send these headers itself.
func forwardedHost(req *http.Request) (string, bool) {
	if fwd := req.Header.Get("Forwarded"); fwd != "" {
		if host, ok := forwardedParam(fwd, "host"); ok {
			return host, true
		}
	}

	if xfh := req.Header.Get("X-Forwarded-Host"); xfh != "" {
		if host := strings.TrimSpace(strings.Split(xfh, ",")[0]); host != "" {
			return host, true
		}
	}

	return "", false
}

// forwardedParam extracts parameter key (e.g. "for", naming the originating
// client, or "host") from the first (nearest-client) element of a Forwarded
// header value, e.g. "for=192.0.2.60:4711;proto=http" or, quoted as RFC 7239
// requires whenever the value itself contains reserved characters like an
// IPv6 literal's brackets and colons, `for="[2001:db8:cafe::17]:4711"`.
// Further elements after a comma, if any, were each prepended by the proxy
// in front of it, so the first is the one closest to the original client.
func forwardedParam(header, key string) (string, bool) {
	first, _, _ := strings.Cut(header, ",")

	for part := range strings.SplitSeq(first, ";") {
		k, value, ok := strings.Cut(part, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), key) {
			continue
		}

		value = strings.Trim(strings.TrimSpace(value), `"`)
		if value == "" {
			return "", false
		}

		return value, true
	}

	return "", false
}

// statusWriter wraps an http.ResponseWriter to capture the status code
// written to it, since http.ResponseWriter doesn't expose that itself once
// WriteHeader has been called.
type statusWriter struct {
	http.ResponseWriter
	status int

	// err is the internal error noteResponseError recorded, if any.
	err error
}

// noteResponseError records err, the internal cause of an error response
// whose body doesn't name it, on every statusWriter wrapping w, for
// logRequests to log.
func noteResponseError(w http.ResponseWriter, err error) {
	for w != nil {
		if sw, ok := w.(*statusWriter); ok {
			sw.err = err
		}

		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}

		w = u.Unwrap()
	}
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap exposes the wrapped ResponseWriter, so http.ResponseController and
// the WebSocket upgrade in handleLive (which needs http.Hijacker) can reach
// it through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
