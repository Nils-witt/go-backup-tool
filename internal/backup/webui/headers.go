package webui

import (
	"net/http"
	"net/url"
	"strings"
)

// hstsValue is the Strict-Transport-Security header securityHeaders sends
// on HTTPS requests: one year, without includeSubDomains, since other hosts
// under the same domain aren't this instance's to decide for.
const hstsValue = "max-age=31536000"

// contentSecurityPolicy builds the dashboard's Content-Security-Policy.
// Scripts, styles, images and fonts come only from this origin: the SPA is
// served from its own go:embed build, with no CDN. style-src also allows
// 'unsafe-inline', because MUI (emotion) injects its styles as <style>
// elements at runtime. connect-src allows this origin (the JSON API and the
// /api/live WebSocket) plus the OIDC issuer's origin, which the SPA talks
// to directly for discovery, token exchange and refresh (see
// frontend/src/auth/oidc.ts); an empty or unparsable issuer adds nothing.
// frame-ancestors 'none' replaces X-Frame-Options for clickjacking.
func contentSecurityPolicy(issuer string) string {
	connect := "'self'"
	if origin, ok := urlOrigin(issuer); ok {
		connect += " " + origin
	}

	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self' data:",
		"connect-src " + connect,
		"frame-ancestors 'none'",
		"form-action 'self'",
		"base-uri 'none'",
		"object-src 'none'",
	}, "; ")
}

// urlOrigin returns raw's scheme://host, reporting false unless raw is an
// absolute http(s) URL.
func urlOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}

	return u.Scheme + "://" + u.Host, true
}

// securityHeaders wraps next, adding the response headers CON.10.A14 /
// APP.3.1.A21 ask for to every response: a Content-Security-Policy (see
// contentSecurityPolicy), X-Content-Type-Options, X-Frame-Options (for
// browsers without frame-ancestors), a Referrer-Policy that keeps the
// download and live tickets in query strings out of Referer headers, and
// Cross-Origin-Opener-Policy. API responses get Cache-Control: no-store, so
// audit logs or a freshly issued API token never linger in a browser or
// proxy cache. Strict-Transport-Security is only sent on a request that
// reached this instance over HTTPS: directly over TLS, or — with
// trustProxyHeaders — as reported by the reverse proxy in front of it (see
// requestIsHTTPS); a browser ignores it over plain HTTP anyway.
func securityHeaders(issuer string, trustProxyHeaders bool, next http.Handler) http.Handler {
	csp := contentSecurityPolicy(issuer)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")

		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}

		if requestIsHTTPS(r, trustProxyHeaders) {
			h.Set("Strict-Transport-Security", hstsValue)
		}

		next.ServeHTTP(w, r)
	})
}

// requestIsHTTPS reports whether r reached the client over HTTPS: r arrived
// over TLS itself, or — only when trustProxyHeaders is set, like
// clientAddr — the reverse proxy in front reports proto=https in its
// Forwarded header (RFC 7239) or X-Forwarded-Proto.
func requestIsHTTPS(r *http.Request, trustProxyHeaders bool) bool {
	if r.TLS != nil {
		return true
	}

	if !trustProxyHeaders {
		return false
	}

	if fwd := r.Header.Get("Forwarded"); fwd != "" {
		if proto, ok := forwardedParam(fwd, "proto"); ok {
			return strings.EqualFold(proto, "https")
		}
	}

	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")

	return strings.EqualFold(strings.TrimSpace(proto), "https")
}
