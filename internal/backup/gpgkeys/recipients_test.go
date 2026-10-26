package gpgkeys

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestIsFingerprint(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]bool{
		"0123456789ABCDEF0123456789ABCDEFAAAABBBB":                         true,
		"0x0123456789abcdef0123456789abcdefaaaabbbb":                       true,
		"0123 4567 89AB CDEF 0123  4567 89AB CDEF AAAA BBBB":               true,
		"0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF": true,
		"AAAABBBBCCCCDDDD": false,
		"ops@example.com":  false,
		"":                 false,
	} {
		if got := IsFingerprint(in); got != want {
			t.Errorf("IsFingerprint(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveRecipient(t *testing.T) {
	t.Parallel()

	usableKey := func(fpr, keyID string, uids ...string) Key {
		return Key{Fingerprint: fpr, KeyID: keyID, UserIDs: uids, CanEncrypt: true}
	}

	ops := usableKey("0123456789ABCDEF0123456789ABCDEFAAAABBBB", "89ABCDEFAAAABBBB", "Backup Ops <ops@example.com>")
	devops := usableKey("1111111111111111111111111111111111111111", "1111111111111111", "Dev <devops@example.com>")
	revoked := Key{Fingerprint: "2222222222222222222222222222222222222222", KeyID: "2222222222222222", UserIDs: []string{"Old <ops@example.com>"}, Revoked: true}
	keys := []Key{ops, devops, revoked}

	tests := []struct {
		recipient string
		want      string // "" means an ErrRecipient
	}{
		{"0123456789abcdef0123456789abcdefaaaabbbb", ops.Fingerprint},
		{"0xAAAABBBB", ops.Fingerprint},
		{"ops@example.com", ops.Fingerprint},
		{"<OPS@example.com>", ops.Fingerprint},
		{"devops@example.com", devops.Fingerprint},
		{"backup ops", ops.Fingerprint},
		{"2222222222222222222222222222222222222222", ""}, // revoked
		{"nobody@example.com", ""},
		{"example.com", ""}, // a substring of both keys' user IDs
		{" ", ""},
	}

	for _, tt := range tests {
		got, err := resolveRecipient(keys, tt.recipient)

		switch {
		case tt.want == "" && !errors.Is(err, ErrRecipient):
			t.Errorf("resolveRecipient(%q) = %q, %v; want an ErrRecipient", tt.recipient, got, err)
		case tt.want != "" && (err != nil || got != tt.want):
			t.Errorf("resolveRecipient(%q) = %q, %v; want %q", tt.recipient, got, err, tt.want)
		}
	}
}

// TestResolveRecipientsImpostor checks that a second key carrying a
// recipient's address makes the address ambiguous instead of letting gpg
// pick either one, while the fingerprint still resolves.
func TestResolveRecipientsImpostor(t *testing.T) {
	t.Parallel()

	k, _ := genKey(t, "Ops <ops@example.com>")
	_, impostor := genKey(t, "Impostor <ops@example.com>")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	fprs, err := k.ResolveRecipients(ctx, []string{"ops@example.com"})
	if err != nil || len(fprs) != 1 {
		t.Fatalf("ResolveRecipients() = %v, %v; want one fingerprint", fprs, err)
	}

	if _, err := k.Import(ctx, impostor); err != nil {
		t.Fatalf("Import() error: %v", err)
	}

	if _, err := k.ResolveRecipients(ctx, []string{"ops@example.com"}); !errors.Is(err, ErrRecipient) {
		t.Errorf("ResolveRecipients(ambiguous) error = %v, want ErrRecipient", err)
	}

	got, err := k.ResolveRecipients(ctx, fprs)
	if err != nil || !slices.Equal(got, fprs) {
		t.Errorf("ResolveRecipients(fingerprint) = %v, %v; want %v", got, err, fprs)
	}
}
