package gpgkeys

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRecipient wraps every reason ResolveRecipients can't pin a recipient
// to exactly one key.
var ErrRecipient = errors.New("gpg recipient")

// IsFingerprint reports whether s is a full key fingerprint: 40 (v4) or 64
// (v5/v6) hex digits, optionally 0x-prefixed and grouped with spaces.
func IsFingerprint(s string) bool {
	h := normalizeHex(s)
	return (len(h) == 40 || len(h) == 64) && isHex(h)
}

// ResolveRecipients returns the fingerprint of the one usable key each of
// recipients names, in order, for encrypting to them with --trust-model
// always: gpg itself would take the first key whose user ID merely
// contains the recipient, so a key someone added to the keyring carrying a
// recipient's address could silently receive the data instead.
//
// A recipient is matched against every key that can currently encrypt: a
// fingerprint against the key's fingerprint, a 16- or 8-digit key ID
// against the end of its key ID, an email address (bare or in <>) against
// its user IDs' addresses exactly, and anything else as a case-insensitive
// substring of a user ID, like gpg does. Matching no key, or more than
// one, fails with ErrRecipient.
func (k *Keyring) ResolveRecipients(ctx context.Context, recipients []string) ([]string, error) {
	if len(recipients) == 0 {
		return nil, nil
	}

	keys, err := k.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing gpg keys: %w", err)
	}

	out := make([]string, len(recipients))

	for i, r := range recipients {
		fpr, err := resolveRecipient(keys, r)
		if err != nil {
			return nil, err
		}

		out[i] = fpr
	}

	return out, nil
}

// resolveRecipient is ResolveRecipients for one recipient against keys.
func resolveRecipient(keys []Key, recipient string) (string, error) {
	if strings.TrimSpace(recipient) == "" {
		return "", fmt.Errorf("%w: empty recipient", ErrRecipient)
	}

	var matched []string

	for _, key := range keys {
		if usable(key) && recipientMatches(key, recipient) {
			matched = append(matched, key.Fingerprint)
		}
	}

	switch len(matched) {
	case 0:
		return "", fmt.Errorf("%w %q: no usable public key in the keyring matches it", ErrRecipient, recipient)
	case 1:
		return matched[0], nil
	default:
		return "", fmt.Errorf("%w %q matches %d keys (%s): name the recipient by fingerprint instead",
			ErrRecipient, recipient, len(matched), strings.Join(matched, ", "))
	}
}

// usable reports whether key can encrypt right now.
func usable(key Key) bool {
	return key.CanEncrypt && !key.Revoked && !key.Expired && !key.Disabled
}

// recipientMatches reports whether recipient names key (see
// ResolveRecipients for how).
func recipientMatches(key Key, recipient string) bool {
	recipient = strings.TrimSpace(recipient)

	if h := normalizeHex(recipient); isHex(h) {
		switch len(h) {
		case 40, 64:
			return strings.EqualFold(key.Fingerprint, h)
		case 8, 16:
			return strings.HasSuffix(strings.ToUpper(key.KeyID), h)
		}
	}

	if strings.Contains(recipient, "@") {
		want := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(recipient, "<"), ">"))

		for _, uid := range key.UserIDs {
			if emailOf(uid) == want {
				return true
			}
		}

		return false
	}

	lower := strings.ToLower(recipient)

	for _, uid := range key.UserIDs {
		if strings.Contains(strings.ToLower(uid), lower) {
			return true
		}
	}

	return false
}

// normalizeHex upper-cases s and drops a 0x prefix and any spaces, the way
// fingerprints are often written.
func normalizeHex(s string) string {
	s = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	return strings.TrimPrefix(s, "0X")
}

func isHex(s string) bool {
	if s == "" {
		return false
	}

	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}

	return true
}
