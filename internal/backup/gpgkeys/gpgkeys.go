// Package gpgkeys lists and imports the public keys in the GPG keyring jobs
// encrypt backups (and notification emails) to, for the web UI's GPG keys
// page. It drives the gpg binary itself — the same gpg-bin/gpg-homedir every
// job falls back to — rather than reimplementing OpenPGP.
package gpgkeys

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"nilswitt.dev/go-backup-tool/internal/backup/notify"
)

// ErrInvalidKey wraps every reason Import refuses its input, so the web UI
// can answer 400 with the message.
var ErrInvalidKey = errors.New("invalid key")

// Key is one public key in the keyring.
type Key struct {
	Fingerprint string     `json:"fingerprint"`
	KeyID       string     `json:"key_id"`
	Algorithm   string     `json:"algorithm"`
	Length      int        `json:"length"`
	Created     time.Time  `json:"created"`
	Expires     *time.Time `json:"expires,omitempty"`
	UserIDs     []string   `json:"user_ids"`
	Revoked     bool       `json:"revoked"`
	Expired     bool       `json:"expired"`
	Disabled    bool       `json:"disabled"`

	// CanEncrypt reports whether the key (or one of its subkeys) can
	// currently encrypt — what a job's recipient needs.
	CanEncrypt bool `json:"can_encrypt"`
}

// Keyring is the GPG keyring at Homedir ("" for gpg's own default, e.g.
// $GNUPGHOME or ~/.gnupg), driven through the gpg binary Bin.
type Keyring struct {
	Bin     string
	Homedir string
}

// New returns the keyring gpg resolves to.
func New(gpg notify.GPGSettings) *Keyring {
	return &Keyring{Bin: gpg.Bin, Homedir: gpg.Homedir}
}

// List returns every public key in the keyring, in gpg's order.
func (k *Keyring) List(ctx context.Context) ([]Key, error) {
	out, err := k.run(ctx, nil, "--with-colons", "--fixed-list-mode", "--list-keys")
	if err != nil {
		// gpg exits 2 listing a homedir that doesn't exist yet: no keys.
		if strings.Contains(err.Error(), "No such file or directory") {
			return []Key{}, nil
		}

		return nil, err
	}

	return parseColons(out), nil
}

// ImportResult is what Import imported.
type ImportResult struct {
	// Fingerprints are the keys imported or updated (gpg's IMPORT_OK);
	// importing a key that's already present unchanged still lists it.
	Fingerprints []string `json:"fingerprints"`

	// Warnings name every email address now on more than one usable key:
	// a job encrypting to that address can't tell which one is meant.
	Warnings []string `json:"warnings"`
}

// maxArmoredKey bounds Import's input: a public key with a few user IDs and
// signatures is a few kilobytes.
const maxArmoredKey = 256 << 10

// Import adds the ASCII-armored public key(s) in armored to the keyring.
// Private keys are refused, as is input with no public key in it.
func (k *Keyring) Import(ctx context.Context, armored string) (ImportResult, error) {
	armored = strings.TrimSpace(armored)

	switch {
	case armored == "":
		return ImportResult{}, fmt.Errorf("%w: paste an ASCII-armored public key", ErrInvalidKey)
	case len(armored) > maxArmoredKey:
		return ImportResult{}, fmt.Errorf("%w: too large", ErrInvalidKey)
	case strings.Contains(armored, "PRIVATE KEY BLOCK"):
		return ImportResult{}, fmt.Errorf("%w: that's a private key; only public keys can be added", ErrInvalidKey)
	case !strings.Contains(armored, "-----BEGIN PGP PUBLIC KEY BLOCK-----"):
		return ImportResult{}, fmt.Errorf("%w: expected an ASCII-armored public key (-----BEGIN PGP PUBLIC KEY BLOCK-----)", ErrInvalidKey)
	}

	// import-minimal drops third-party signatures; --status-fd reports
	// what was imported in a machine-readable form on stdout.
	out, err := k.run(ctx, strings.NewReader(armored+"\n"), "--status-fd", "1", "--import-options", "import-minimal", "--import")

	res := ImportResult{Fingerprints: importedFingerprints(out), Warnings: []string{}}
	if len(res.Fingerprints) == 0 {
		if err != nil {
			return ImportResult{}, fmt.Errorf("%w: %w", ErrInvalidKey, err)
		}

		return ImportResult{}, fmt.Errorf("%w: no public key found", ErrInvalidKey)
	}

	keys, err := k.List(ctx)
	if err != nil {
		return res, nil //nolint:nilerr // the import itself succeeded; the ambiguity check is best-effort
	}

	res.Warnings = ambiguousEmails(keys, res.Fingerprints)

	return res, nil
}

// run runs gpg in batch mode against k's homedir with args and stdin,
// returning its stdout; a failure's error carries gpg's stderr.
func (k *Keyring) run(ctx context.Context, stdin *strings.Reader, args ...string) ([]byte, error) {
	full := []string{"--batch", "--no-tty"}
	if k.Homedir != "" {
		full = append(full, "--homedir", k.Homedir)
	}

	cmd := exec.CommandContext(ctx, k.Bin, append(full, args...)...) //nolint:gosec // k.Bin/Homedir are operator-supplied config; args are fixed flags
	if stdin != nil {
		cmd.Stdin = stdin
	}

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("running %s: %w: %s", k.Bin, err, strings.TrimSpace(stderr.String()))
	}

	return out, nil
}

// importedFingerprints extracts every IMPORT_OK fingerprint from gpg's
// --status-fd output, without duplicates.
func importedFingerprints(status []byte) []string {
	var out []string

	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 4 && f[0] == "[GNUPG:]" && f[1] == "IMPORT_OK" && !slices.Contains(out, f[3]) {
			out = append(out, f[3])
		}
	}

	return out
}

// ambiguousEmails returns every email address that one of the keys in
// fingerprints carries and that another usable key carries too.
func ambiguousEmails(keys []Key, fingerprints []string) []string {
	owners := make(map[string][]string)

	for _, key := range keys {
		if !key.CanEncrypt {
			continue
		}

		for _, uid := range key.UserIDs {
			if email := emailOf(uid); email != "" && !slices.Contains(owners[email], key.Fingerprint) {
				owners[email] = append(owners[email], key.Fingerprint)
			}
		}
	}

	var warnings []string

	for _, email := range slices.Sorted(maps.Keys(owners)) {
		fprs := owners[email]
		if len(fprs) > 1 && slices.ContainsFunc(fprs, func(f string) bool { return slices.Contains(fingerprints, f) }) {
			warnings = append(warnings, fmt.Sprintf("%s is now on %d keys (%s): a job encrypting to %s may use either; name the recipient by fingerprint instead",
				email, len(fprs), strings.Join(fprs, ", "), email))
		}
	}

	return warnings
}

// emailOf returns the lowercased address in a user ID like
// "Name <me@example.com>", or the whole ID if it's a bare address.
func emailOf(uid string) string {
	if i, j := strings.LastIndexByte(uid, '<'), strings.LastIndexByte(uid, '>'); i >= 0 && j > i {
		return strings.ToLower(uid[i+1 : j])
	}

	if strings.Contains(uid, "@") && !strings.ContainsAny(uid, " ") {
		return strings.ToLower(uid)
	}

	return ""
}

// algorithms names gpg's numeric public key algorithm ids (RFC 4880/9580).
var algorithms = map[string]string{
	"1": "RSA", "2": "RSA", "3": "RSA", "16": "Elgamal", "17": "DSA",
	"18": "ECDH", "19": "ECDSA", "22": "EdDSA", "25": "X25519", "27": "Ed25519",
}

// parseColons parses `gpg --with-colons --fixed-list-mode --list-keys`
// output (see gpg's doc/DETAILS) into one Key per pub record.
func parseColons(out []byte) []Key {
	keys := []Key{}

	var cur *Key

	// fprForPub is whether the next fpr record follows a pub (the Key's own
	// fingerprint) rather than a sub (a subkey's, ignored).
	fprForPub := false

	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 10 {
			continue
		}

		switch f[0] {
		case "pub":
			keys = append(keys, keyFromPub(f))
			cur, fprForPub = &keys[len(keys)-1], true
		case "fpr":
			if cur != nil && fprForPub {
				cur.Fingerprint = f[9]
			}

			fprForPub = false
		case "uid":
			if cur != nil && f[1] != "r" {
				cur.UserIDs = append(cur.UserIDs, unescapeColons(f[9]))
			}
		case "sub":
			fprForPub = false
		}
	}

	return keys
}

func keyFromPub(f []string) Key {
	key := Key{
		KeyID: f[4], Algorithm: algorithms[f[3]], UserIDs: []string{},
		Revoked: f[1] == "r", Expired: f[1] == "e",
	}

	if key.Algorithm == "" {
		key.Algorithm = "algorithm " + f[3]
	}

	if len(f) > 16 && f[16] != "" {
		key.Algorithm += " " + f[16] // the ECC curve, e.g. "cv25519"
	}

	key.Length, _ = strconv.Atoi(f[2])

	if t, ok := epoch(f[5]); ok {
		key.Created = t
	}

	if t, ok := epoch(f[6]); ok {
		key.Expires = &t
	}

	if len(f) > 11 {
		// The pub record's capabilities field lists, in upper case, what
		// the whole key (primary plus subkeys) can still do; "D" marks a
		// disabled key.
		key.CanEncrypt = strings.Contains(f[11], "E") && !strings.Contains(f[11], "D")
		key.Disabled = strings.Contains(f[11], "D")
	}

	return key
}

func epoch(s string) (time.Time, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}

	return time.Unix(n, 0).UTC(), true
}

// unescapeColons undoes gpg's C-style escaping of user IDs in --with-colons
// output (e.g. "\x3a" for ':').
func unescapeColons(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))

				i += 3

				continue
			}
		}

		b.WriteByte(s[i])
	}

	return b.String()
}
