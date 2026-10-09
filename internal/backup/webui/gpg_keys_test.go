package webui

import (
	"net/http"
	"os/exec"
	"testing"

	"nilswitt.dev/go-backup-tool/internal/backup/gpgkeys"
)

func TestGPGKeyAPI(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	idp := newTestIDP(t)
	keyring := &gpgkeys.Keyring{Bin: "gpg", Homedir: testGPGKeyring(t)}
	readOnly, _ := startJobsWebUIWithKeyring(t, idp, false, keyring)
	editable, _ := startJobsWebUIWithKeyring(t, idp, true, keyring)

	admin := idp.token(t, "erin", map[string]any{"preferred_username": "erin", "groups": []string{"admins"}})
	viewer := idp.token(t, "viewer", nil)
	// Split so it isn't mistaken for a real private key by secret scanners.
	private := map[string]string{"armored": "-----BEGIN PGP PRIVATE" + " KEY BLOCK-----\nx\n-----END PGP PRIVATE" + " KEY BLOCK-----"}

	for _, c := range []receiverConfigCall{
		{"non-admin list", http.MethodGet, "/api/gpg-keys", viewer, nil, http.StatusForbidden},
		{"import without editing", http.MethodPost, "/api/gpg-keys", admin, map[string]string{"armored": "x"}, http.StatusForbidden},
	} {
		c.run(t, readOnly)
	}

	(receiverConfigCall{"import private key", http.MethodPost, "/api/gpg-keys", admin, private, http.StatusBadRequest}).run(t, editable)

	var list gpgKeyListJSON
	if code := doJSON(t, readOnly, http.MethodGet, "/api/gpg-keys", admin, &list); code != http.StatusOK {
		t.Fatalf("admin list = %d", code)
	}

	if list.Editing || list.Homedir != keyring.Homedir || len(list.Keys) != 1 || !list.Keys[0].CanEncrypt {
		t.Errorf("list = %+v, want the shared test key", list)
	}
}
