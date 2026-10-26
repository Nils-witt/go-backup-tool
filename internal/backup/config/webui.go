package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"nilswitt.dev/go-backup-tool/internal/backup/permission"
)

// OIDCSettings is runConfig's resolved form of the config file's
// webui.oidc: entry (see fileWebUIOIDC). The web UI's SPA logs in as the
// public client ClientID (authorization code + PKCE, no client secret), and
// the backend verifies every request's access token against Issuer (see
// internal/backup/webui/auth.go). Nothing about users is stored (beyond
// recent back-channel logouts, see internal/backup/webui/backchannel.go): a
// request's permissions are DefaultPermissions plus whatever its token's
// groups (read from GroupsClaim) are mapped to in GroupPermissions, worked
// out afresh on every request.
type OIDCSettings struct {
	Enabled     bool
	Issuer      string
	ClientID    string
	Scopes      []string
	ButtonLabel string
	GroupsClaim string

	// DefaultPermissions is granted to every signed-in user, on top of
	// whatever their groups grant. Defaults to
	// permission.PermissionView|permission.PermissionDownload when
	// webui.oidc.default-permissions: is unset.
	DefaultPermissions permission.Permission

	// GroupPermissions maps a group name (as it appears in the token's
	// GroupsClaim claim) to the permissions its members get on top of
	// DefaultPermissions.
	GroupPermissions map[string]permission.Permission
}

const (
	// defaultOIDCButtonLabel is used when webui.oidc.button-label is unset.
	defaultOIDCButtonLabel = "Sign in with SSO"
	// defaultOIDCGroupsClaim is used when webui.oidc.groups-claim is unset.
	defaultOIDCGroupsClaim = "groups"
)

// webUISettings is what ParseFlags derives from the config file's webui:
// entry beyond a plain field copy (see resolveWebUISettings).
type webUISettings struct {
	listen           string
	oidc             OIDCSettings
	receiversBaseDir string
	eventLogLimit    int
	corsGetOrigins   []string
	rateLimit        RateLimitSettings
}

// resolveWebUISettings resolves cfg (the config file's webui: entry) into
// its listen address (see resolveWebUIListen), its SSO settings (see
// resolveOIDCSettings), and its receivers base dir (see
// resolveReceiversBaseDir) — the pieces of runConfig ParseFlags derives
// from webui: that need validation beyond a plain field copy.
func resolveWebUISettings(cfg fileWebUI) (webUISettings, error) {
	listen, err := resolveWebUIListen(cfg)
	if err != nil {
		return webUISettings{}, err
	}

	if cfg.Username != "" || cfg.Password != "" {
		return webUISettings{}, errors.New("webui.username/webui.password are no longer supported: the web UI only supports SSO login, configure webui.oidc instead")
	}

	oidc, err := resolveOIDCSettings(cfg.OIDC, listen)
	if err != nil {
		return webUISettings{}, err
	}

	baseDir, err := resolveReceiversBaseDir(cfg.ReceiversBaseDir)
	if err != nil {
		return webUISettings{}, err
	}

	limit, err := resolveEventLogLimit(cfg.EventLogLimit)
	if err != nil {
		return webUISettings{}, err
	}

	corsGetOrigins, err := resolveCORSGetOrigins(cfg.CORSGetOrigins)
	if err != nil {
		return webUISettings{}, err
	}

	rateLimit, err := resolveRateLimit(cfg.RateLimit)
	if err != nil {
		return webUISettings{}, err
	}

	return webUISettings{listen: listen, oidc: oidc, receiversBaseDir: baseDir, eventLogLimit: limit, corsGetOrigins: corsGetOrigins, rateLimit: rateLimit}, nil
}

// resolveCORSGetOrigins validates webui.cors-get-origins: (see
// fileWebUI.CORSGetOrigins): each entry must be "*" or a bare origin —
// http(s)://host[:port] with no path, query, fragment or userinfo, since a
// browser's Origin header never carries any of those and so could never
// match. Entries are trimmed, a trailing "/" is dropped, and the scheme and
// host lowercased to match how browsers send Origin.
func resolveCORSGetOrigins(origins []string) ([]string, error) {
	if len(origins) == 0 {
		return nil, nil
	}

	resolved := make([]string, 0, len(origins))

	for _, o := range origins {
		o = strings.TrimSuffix(strings.TrimSpace(o), "/")
		if o == "*" {
			resolved = append(resolved, o)
			continue
		}

		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return nil, fmt.Errorf("webui.cors-get-origins: %q is not \"*\" or an origin like https://example.com[:port]", o)
		}

		resolved = append(resolved, strings.ToLower(u.Scheme)+"://"+strings.ToLower(u.Host))
	}

	return resolved, nil
}

// maxEventLogLimit bounds webui.event-log-limit:, since every entry it
// allows is read, sent, and rendered by the dashboard on each poll.
const maxEventLogLimit = 10000

// resolveEventLogLimit validates webui.event-log-limit: (see
// fileWebUI.EventLogLimit), returning DefaultEventLogLimit when
// it's unset.
func resolveEventLogLimit(n int) (int, error) {
	if n == 0 {
		return DefaultEventLogLimit, nil
	}

	if n < 1 || n > maxEventLogLimit {
		return 0, fmt.Errorf("webui.event-log-limit %d must be between 1 and %d", n, maxEventLogLimit)
	}

	return n, nil
}

// resolveReceiversBaseDir validates webui.receivers-base-dir: (see
// fileWebUI.ReceiversBaseDir), requiring an absolute path when set, and
// returns it cleaned. Unset returns "".
func resolveReceiversBaseDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}

	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("webui.receivers-base-dir %q must be an absolute path", dir)
	}

	return filepath.Clean(dir), nil
}

// resolveOIDCSettings validates cfg (the config file's webui.oidc: entry)
// against listen (the web UI's resolved listen address, empty if the web UI
// itself is disabled — see resolveWebUIListen) and returns runConfig's
// resolved OIDCSettings. An unset/false cfg.Enabled returns the zero value:
// since SSO is the web UI's only login method, that leaves the dashboard
// locked (nobody can sign in) while the receiver API, served by the same
// HTTP server, keeps working. It's an error to enable OIDC without the web
// UI itself, or without issuer and client-id set.
func resolveOIDCSettings(cfg fileWebUIOIDC, listen string) (OIDCSettings, error) {
	if cfg.ClientSecret != "" || cfg.RedirectURL != "" {
		return OIDCSettings{}, errors.New("webui.oidc.client-secret/webui.oidc.redirect-url are no longer supported: the web UI logs in as a public client (redirect URI <origin>/login/sso/callback), remove them")
	}

	if !cfg.Enabled {
		return OIDCSettings{}, nil
	}

	if listen == "" {
		return OIDCSettings{}, errors.New("webui.oidc.enabled is true but webui.enabled is not")
	}

	issuer := strings.TrimSpace(cfg.Issuer)
	clientID := strings.TrimSpace(cfg.ClientID)

	required := []struct{ name, val string }{
		{"issuer", issuer},
		{"client-id", clientID},
	}

	for _, r := range required {
		if r.val == "" {
			return OIDCSettings{}, fmt.Errorf("webui.oidc.enabled is true but webui.oidc.%s is not set", r.name)
		}
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}

	defaultPerm := permission.PermissionView | permission.PermissionDownload

	if len(cfg.DefaultPermissions) > 0 {
		parsed, err := permission.ParsePermissions(cfg.DefaultPermissions)
		if err != nil {
			return OIDCSettings{}, fmt.Errorf("webui.oidc.default-permissions: %w", err)
		}

		defaultPerm = parsed
	}

	groupPerms, err := parseOIDCGroupPermissions(cfg.GroupPermissions)
	if err != nil {
		return OIDCSettings{}, err
	}

	buttonLabel := strings.TrimSpace(cfg.ButtonLabel)
	if buttonLabel == "" {
		buttonLabel = defaultOIDCButtonLabel
	}

	groupsClaim := strings.TrimSpace(cfg.GroupsClaim)
	if groupsClaim == "" {
		groupsClaim = defaultOIDCGroupsClaim
	}

	return OIDCSettings{
		Enabled:            true,
		Issuer:             issuer,
		ClientID:           clientID,
		Scopes:             scopes,
		ButtonLabel:        buttonLabel,
		GroupsClaim:        groupsClaim,
		DefaultPermissions: defaultPerm,
		GroupPermissions:   groupPerms,
	}, nil
}

// parseOIDCGroupPermissions parses webui.oidc.group-permissions: (group
// name -> permission names) into group name -> permission bitmask,
// rejecting an empty group name or an unknown permission name.
func parseOIDCGroupPermissions(cfg map[string][]string) (map[string]permission.Permission, error) {
	groupPerms := make(map[string]permission.Permission, len(cfg))

	for group, names := range cfg {
		if strings.TrimSpace(group) == "" {
			return nil, errors.New("webui.oidc.group-permissions: group name must not be empty")
		}

		parsed, err := permission.ParsePermissions(names)
		if err != nil {
			return nil, fmt.Errorf("webui.oidc.group-permissions[%q]: %w", group, err)
		}

		groupPerms[group] = parsed
	}

	return groupPerms, nil
}

// resolveWebUIListen returns the effective web UI listen address from the
// config file's webui: entry: empty disables the web UI entirely, whether
// because webui.enabled is false/unset or webui: wasn't given at all. It's
// an error to set webui.enabled: true without also giving webui.listen a
// value, since there would then be no address to bind.
func resolveWebUIListen(cfg fileWebUI) (string, error) {
	if !cfg.Enabled {
		return "", nil
	}

	listen := strings.TrimSpace(cfg.Listen)
	if listen == "" {
		return "", errors.New("webui.enabled is true but webui.listen is not set")
	}

	return listen, nil
}
