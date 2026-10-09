package gpgkeys

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleColons is `gpg --with-colons --fixed-list-mode --list-keys` output
// for one usable ed25519/cv25519 key and one revoked RSA key.
const sampleColons = `tru::1:1760000000:0:3:1:5
pub:u:255:22:AAAABBBBCCCCDDDD:1760000000:1860000000::u:::scESC:::::ed25519:::0:
fpr:::::::::0123456789ABCDEF0123456789ABCDEFAAAABBBB:
uid:u::::1760000000::HASH::Backup Ops \x3cops@example.com\x3e::::::::::0:
uid:r::::1760000000::HASH::Old <old@example.com>::::::::::0:
sub:u:255:18:EEEEFFFF00001111:1760000000::::::e:::::cv25519::
fpr:::::::::FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF:
pub:r:4096:1:1111222233334444:1500000000:::-:::sc:::::::::0:
fpr:::::::::9999999999999999999999999999999911112222:
uid:r::::1500000000::HASH::Gone <gone@example.com>::::::::::0:
`

func TestParseColons(t *testing.T) {
	t.Parallel()

	keys := parseColons([]byte(sampleColons))
	expires := time.Unix(1860000000, 0).UTC()
	want := []Key{
		{
			Fingerprint: "0123456789ABCDEF0123456789ABCDEFAAAABBBB", KeyID: "AAAABBBBCCCCDDDD", Algorithm: "EdDSA ed25519",
			Length: 255, Created: time.Unix(1760000000, 0).UTC(), Expires: &expires,
			UserIDs: []string{"Backup Ops <ops@example.com>"}, CanEncrypt: true,
		},
		{
			Fingerprint: "9999999999999999999999999999999911112222", KeyID: "1111222233334444", Algorithm: "RSA",
			Length: 4096, Created: time.Unix(1500000000, 0).UTC(), UserIDs: []string{}, Revoked: true,
		},
	}

	if !reflect.DeepEqual(keys, want) {
		t.Errorf("parseColons() = %+v, want %+v", keys, want)
	}
}

func TestEmailOf(t *testing.T) {
	t.Parallel()

	for uid, want := range map[string]string{
		"Ops <Ops@Example.com>": "ops@example.com",
		"me@example.com":        "me@example.com",
		"No Email":              "",
	} {
		if got := emailOf(uid); got != want {
			t.Errorf("emailOf(%q) = %q, want %q", uid, got, want)
		}
	}
}

// genKey creates a fresh keyring holding one no-passphrase key for uid and
// returns it with that key's armored public key.
func genKey(t *testing.T, uid string) (*Keyring, string) {
	t.Helper()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	// Short path: gpg-agent's socket path must fit the kernel's limit.
	k := &Keyring{Bin: "gpg", Homedir: shortTempDir(t)}
	ctx := t.Context()

	if _, err := k.run(ctx, nil, "--passphrase", "", "--quick-gen-key", uid, "future-default", "default", "never"); err != nil {
		t.Skipf("generating a test key: %v", err)
	}

	armored, err := k.run(ctx, nil, "--armor", "--export", uid)
	if err != nil {
		t.Fatalf("exporting test key: %v", err)
	}

	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "gpgconf", "--homedir", k.Homedir, "--kill", "all").Run() //nolint:gosec // k.Homedir is this test's own temp dir
	})

	return k, string(armored)
}

func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := makeShortTempDir()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// makeShortTempDir creates a directory with a short path under the OS temp
// directory: gpg-agent's socket (homedir + "/S.gpg-agent") must fit the
// kernel's sun_path limit (~104 bytes on macOS), which a t.TempDir() path
// can exceed.
func makeShortTempDir() (string, error) {
	return os.MkdirTemp("", "gbt-k-")
}

func TestImportAndList(t *testing.T) {
	t.Parallel()

	_, armored := genKey(t, "Ops <ops@example.com>")
	_, other := genKey(t, "Impostor <ops@example.com>")

	target := &Keyring{Bin: "gpg", Homedir: shortTempDir(t)}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if keys, err := target.List(ctx); err != nil || len(keys) != 0 {
		t.Fatalf("List() on an empty keyring = %+v, %v", keys, err)
	}

	res, err := target.Import(ctx, armored)
	if err != nil || len(res.Fingerprints) != 1 || len(res.Warnings) != 0 {
		t.Fatalf("Import() = %+v, %v; want one key, no warnings", res, err)
	}

	keys, err := target.List(ctx)
	if err != nil || len(keys) != 1 || keys[0].Fingerprint != res.Fingerprints[0] || !keys[0].CanEncrypt {
		t.Fatalf("List() = %+v, %v", keys, err)
	}

	res, err = target.Import(ctx, other)
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "ops@example.com") {
		t.Errorf("Import(same email) = %+v, %v; want an ambiguity warning", res, err)
	}
}

// privateKeyBlock looks like an armored private key (split so it isn't
// mistaken for a real one by secret scanners).
const privateKeyBlock = "-----BEGIN PGP PRIVATE" + " KEY BLOCK-----\nx\n-----END PGP PRIVATE" + " KEY BLOCK-----"

func TestImportRejects(t *testing.T) {
	t.Parallel()

	k := &Keyring{Bin: "gpg", Homedir: t.TempDir()}

	for name, in := range map[string]string{
		"empty":   "  ",
		"private": privateKeyBlock,
		"garbage": "ssh-ed25519 AAAA...",
	} {
		if _, err := k.Import(t.Context(), in); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s: Import() error = %v, want ErrInvalidKey", name, err)
		}
	}
}
