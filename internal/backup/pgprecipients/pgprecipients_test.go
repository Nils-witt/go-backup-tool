package pgprecipients

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	keyA = "0123456789ABCDEF"
	keyB = "FEDCBA9876543210"
)

// seipd is a stand-in for the encrypted data packet that follows the
// session key packets; its contents don't matter.
var seipd = newPacket(18, []byte{1, 0xAA, 0xBB})

// newPacket encodes an OpenPGP-format packet, choosing the shortest length
// encoding for body. The byte conversions below take one octet of a
// length at a time, so truncating is intended.
//
//nolint:gosec // G115: see above
func newPacket(tag byte, body []byte) []byte {
	out := []byte{0xC0 | tag}

	switch n := len(body); {
	case n < 192:
		out = append(out, byte(n))
	case n < 8384:
		n -= 192
		out = append(out, byte(n>>8)+192, byte(n))
	default:
		out = append(out, 255, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}

	return append(out, body...)
}

// oldPacket encodes a legacy-format packet with a two-octet length.
//
//nolint:gosec // G115: takes one octet of the length at a time
func oldPacket(tag byte, body []byte) []byte {
	n := len(body)

	return append([]byte{0x80 | tag<<2 | 1, byte(n >> 8), byte(n)}, body...)
}

// pkeskV3 is a version 3 PKESK body for keyID with an RSA session key
// field of sessionKeyLen filler octets (512 for RSA-4096).
func pkeskV3(t *testing.T, keyID string, sessionKeyLen int) []byte {
	t.Helper()

	id, err := hex.DecodeString(keyID)
	if err != nil || len(id) != 8 {
		t.Fatalf("bad key ID %q", keyID)
	}

	body := append([]byte{3}, id...)
	body = append(body, 1) // RSA

	return append(body, make([]byte, sessionKeyLen)...)
}

// pkeskV6 is a version 6 PKESK body for a key of keyVersion with
// fingerprint fpr (none for an anonymous recipient).
func pkeskV6(keyVersion byte, fpr []byte) []byte {
	if fpr == nil {
		return []byte{6, 0, 25, 0xAA}
	}

	body := []byte{6, byte(1 + len(fpr)), keyVersion} //nolint:gosec // G115: a fingerprint is 20 or 32 octets
	body = append(body, fpr...)

	return append(body, 25, 0xAA) // X25519, filler
}

// armor wraps message in ASCII armor the way gpg --armor writes it.
func armor(message []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(message)

	var b strings.Builder

	b.WriteString("-----BEGIN PGP MESSAGE-----\nComment: test\n\n")

	for len(enc) > 64 {
		b.WriteString(enc[:64])
		b.WriteByte('\n')

		enc = enc[64:]
	}

	b.WriteString(enc)
	b.WriteString("\n=abcd\n-----END PGP MESSAGE-----\n")

	return []byte(b.String())
}

func concat(parts ...[]byte) []byte {
	return slices.Concat(parts...)
}

// oneRecipient is a message encrypted to keyA in gpg's legacy packet
// format.
func oneRecipient(t *testing.T) []byte {
	t.Helper()

	return concat(oldPacket(tagPKESK, pkeskV3(t, keyA, 256)), seipd)
}

func TestKeyIDs(t *testing.T) {
	t.Parallel()

	fpr4 := bytes.Repeat([]byte{0x11}, 12)
	fpr4 = append(fpr4, 0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF)
	fpr6 := append([]byte{0xFE, 0xDC, 0xBA, 0x98, 0x76, 0x54, 0x32, 0x10}, bytes.Repeat([]byte{0x22}, 24)...)

	tests := []struct {
		name    string
		message []byte
		want    []string
	}{
		{
			name:    "one recipient, legacy format",
			message: oneRecipient(t),
			want:    []string{keyA},
		},
		{
			name: "two recipients, two-octet lengths",
			message: concat(
				newPacket(tagPKESK, pkeskV3(t, keyA, 512)),
				newPacket(tagPKESK, pkeskV3(t, keyB, 512)),
				seipd,
			),
			want: []string{keyA, keyB},
		},
		{
			name: "five-octet length",
			message: concat(
				[]byte{0xC0 | tagPKESK, 255, 0, 0, 0, 10},
				pkeskV3(t, keyB, 1),
				seipd,
			),
			want: []string{keyB},
		},
		{
			name: "passphrase and marker packets in between",
			message: concat(
				newPacket(tagMarker, []byte("PGP")),
				newPacket(tagSKESK, []byte{4, 9, 3, 0}),
				newPacket(tagPKESK, pkeskV3(t, keyA, 64)),
				seipd,
			),
			want: []string{keyA},
		},
		{
			name:    "hidden recipient, version 3",
			message: concat(newPacket(tagPKESK, pkeskV3(t, HiddenKeyID, 64)), seipd),
			want:    []string{HiddenKeyID},
		},
		{
			name: "version 6 packets",
			message: concat(
				newPacket(tagPKESK, pkeskV6(4, fpr4)),
				newPacket(tagPKESK, pkeskV6(6, fpr6)),
				newPacket(tagPKESK, pkeskV6(0, nil)),
				seipd,
			),
			want: []string{keyA, keyB, HiddenKeyID},
		},
		{
			name:    "armored",
			message: armor(oneRecipient(t)),
			want:    []string{keyA},
		},
		{
			name:    "armored after leading whitespace",
			message: append([]byte("\r\n  "), armor(oneRecipient(t))...),
			want:    []string{keyA},
		},
		{
			name: "truncated inside the second recipient",
			message: concat(
				newPacket(tagPKESK, pkeskV3(t, keyA, 64)),
				newPacket(tagPKESK, pkeskV3(t, keyB, 64))[:30],
			),
			want: []string{keyA},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := KeyIDs(bytes.NewReader(tt.message))
			if err != nil {
				t.Fatalf("KeyIDs() error: %v", err)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("KeyIDs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKeyIDsNotEncrypted(t *testing.T) {
	t.Parallel()

	message := oneRecipient(t)

	tests := []struct {
		name    string
		message []byte
	}{
		{
			name:    "message with no recipients before the data",
			message: seipd,
		},
		{
			name:    "passphrase only",
			message: concat(newPacket(tagSKESK, []byte{4, 9, 3, 0}), seipd),
		},
		{
			name:    "plain text",
			message: []byte("hello, world\n"),
		},
		{
			name:    "empty",
			message: nil,
		},
		{
			name:    "truncated inside the first recipient",
			message: message[:20],
		},
		{
			name:    "length beyond any session key packet",
			message: []byte{0xC0 | tagPKESK, 255, 0xFF, 0xFF, 0xFF, 0xFF, 3},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := KeyIDs(bytes.NewReader(tt.message))
			if !errors.Is(err, ErrNotEncrypted) {
				t.Errorf("KeyIDs() = %v, %v, want ErrNotEncrypted", got, err)
			}
		})
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n

	return n, err
}

func TestKeyIDsReadsOnlyTheHeader(t *testing.T) {
	t.Parallel()

	// The data packet's length (two octets) runs past the first read: its
	// tag alone must end the search.
	data := newPacket(18, make([]byte, 8000))
	r := &countingReader{r: bytes.NewReader(concat(newPacket(tagPKESK, pkeskV3(t, keyA, 512)), data, make([]byte, 1<<20)))}

	got, err := KeyIDs(r)
	if err != nil {
		t.Fatalf("KeyIDs() error: %v", err)
	}

	if !slices.Equal(got, []string{keyA}) {
		t.Errorf("KeyIDs() = %v, want [%s]", got, keyA)
	}

	if r.n > firstRead {
		t.Errorf("KeyIDs() read %d bytes, want at most %d", r.n, firstRead)
	}
}

func TestKeyIDsReadsOnWhileRecipientsContinue(t *testing.T) {
	t.Parallel()

	// 24 RSA-4096 recipients take about 12 KiB, beyond the first read.
	var (
		message []byte
		want    []string
	)

	for i := range 24 {
		id := strings.ToUpper(hex.EncodeToString([]byte{0, 0, 0, 0, 0, 0, 0, byte(i)}))
		message = append(message, oldPacket(tagPKESK, pkeskV3(t, id, 512))...)
		want = append(want, id)
	}

	message = append(message, seipd...)

	for _, m := range []struct {
		name    string
		message []byte
	}{
		{name: "binary", message: message},
		{name: "armored", message: armor(message)},
	} {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()

			got, err := KeyIDs(bytes.NewReader(m.message))
			if err != nil {
				t.Fatalf("KeyIDs() error: %v", err)
			}

			if !slices.Equal(got, want) {
				t.Errorf("KeyIDs() = %v, want %v", got, want)
			}
		})
	}
}

// TestKeyIDsMatchesGPG encrypts with the real gpg binary, binary and
// armored, and checks KeyIDs names the encryption subkey gpg chose.
func TestKeyIDsMatchesGPG(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not found in PATH, skipping")
	}

	// gpg-agent's socket path must fit the kernel's sun_path limit (~104
	// bytes on macOS), which t.TempDir()'s long paths can exceed.
	home, err := os.MkdirTemp("", "gbt-pgp-") //nolint:usetesting // t.TempDir() paths are too long for gpg-agent's socket
	if err != nil {
		t.Fatalf("creating gpg homedir: %v", err)
	}

	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "gpgconf", "--homedir", home, "--kill", "gpg-agent").Run() //nolint:gosec // home is this test's own MkdirTemp output
		_ = os.RemoveAll(home)
	})

	gpg := func(stdin []byte, args ...string) []byte {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), "gpg", append([]string{"--homedir", home, "--batch", "--yes"}, args...)...) //nolint:gosec // fixed flags and this test's own homedir
		cmd.Stdin = bytes.NewReader(stdin)

		out, err := cmd.Output()
		if err != nil {
			t.Skipf("gpg %v: %v", args, err)
		}

		return out
	}

	gpg(nil, "--passphrase", "", "--quick-gen-key", "test <test@example.com>", "default", "default", "never")

	want := encryptionSubkeyID(gpg(nil, "--with-colons", "--list-keys", "test@example.com"))
	if want == "" {
		t.Fatal("gpg listed no encryption subkey")
	}

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "binary", want: want},
		{name: "armored", args: []string{"--armor"}, want: want},
		{name: "hidden recipient", args: []string{"--throw-keyids"}, want: HiddenKeyID},
	} {
		args := append([]string{"--trust-model", "always", "--encrypt", "-r", "test@example.com"}, tt.args...)

		got, err := KeyIDs(bytes.NewReader(gpg([]byte("secret"), args...)))
		if err != nil {
			t.Fatalf("%s: KeyIDs() error: %v", tt.name, err)
		}

		if !slices.Equal(got, []string{tt.want}) {
			t.Errorf("%s: KeyIDs() = %v, want [%s]", tt.name, got, tt.want)
		}
	}
}

// encryptionSubkeyID returns the key ID of the first encryption-capable
// subkey in gpg --with-colons output.
func encryptionSubkeyID(colons []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(colons))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) > 11 && f[0] == "sub" && strings.Contains(f[11], "e") {
			return f[4]
		}
	}

	return ""
}
