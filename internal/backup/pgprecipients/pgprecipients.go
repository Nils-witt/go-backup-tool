// Package pgprecipients reads which keys an OpenPGP message is encrypted
// to from its Public-Key Encrypted Session Key packets (RFC 9580 §5.1),
// without decrypting it or needing any key: the dashboard's receiver file
// listing shows them for files the receiver itself can't open. It
// understands binary and ASCII-armored messages, and imports nothing else
// of this module.
package pgprecipients

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// HiddenKeyID is the key ID of a recipient the sender hid (gpg's
// --throw-keyids): the wildcard key ID.
const HiddenKeyID = "0000000000000000"

// ErrNotEncrypted is returned for input that doesn't start with an OpenPGP
// message's public-key session key packets.
var ErrNotEncrypted = errors.New("not an OpenPGP message encrypted to a public key")

var (
	// errTruncated means the input ended inside a packet: reading more of
	// the message may still turn up recipients.
	errTruncated = errors.New("truncated packet")

	// errUnsupported means a packet no session key packet looks like.
	errUnsupported = errors.New("unsupported packet")
)

const (
	// firstRead is how much of a message KeyIDs reads first; it doubles
	// that up to maxRead while the session key packets continue. One
	// recipient takes a few hundred bytes, so a first read usually does.
	firstRead = 4 << 10
	maxRead   = 64 << 10

	tagPKESK  = 1
	tagSKESK  = 3
	tagMarker = 10

	armorBegin = "-----BEGIN PGP MESSAGE-----"
)

// KeyIDs returns the key ID (16 upper-case hex digits) of each recipient
// of the OpenPGP message read from r, in message order; a hidden
// recipient is HiddenKeyID. A key ID names the (sub)key encrypted to,
// usually an encryption subkey rather than the primary key. KeyIDs reads
// only as much of r as the session key packets take, and at most 64 KiB.
// Input that isn't an encrypted message, or has no public-key recipient
// (e.g. one encrypted to a passphrase only), yields ErrNotEncrypted.
func KeyIDs(r io.Reader) ([]string, error) {
	var head []byte

	limit := firstRead

	for {
		chunk, err := io.ReadAll(io.LimitReader(r, int64(limit-len(head))))
		if err != nil {
			return nil, err
		}

		head = append(head, chunk...)
		atEOF := len(head) < limit

		ids, done := parse(head)
		if done || atEOF || limit >= maxRead {
			if len(ids) == 0 {
				return nil, ErrNotEncrypted
			}

			return ids, nil
		}

		limit = min(2*limit, maxRead)
	}
}

// parse collects the recipients of the message prefix head, binary or
// armored. done reports whether the session key packets ended within it.
func parse(head []byte) (ids []string, done bool) {
	if trimmed := bytes.TrimLeft(head, " \t\r\n"); bytes.HasPrefix(trimmed, []byte(armorBegin)) {
		head = dearmor(trimmed)
	}

	return parsePackets(head)
}

// parsePackets walks the packets leading data and collects the key ID of
// every PKESK packet. done is true once it reaches a packet that's neither
// a session key nor a marker packet, and false when data runs out first.
func parsePackets(data []byte) (ids []string, done bool) {
	for len(data) > 0 {
		tag, start, length, err := packetHeader(data)
		if err != nil {
			return ids, !errors.Is(err, errTruncated)
		}

		if tag != tagPKESK && tag != tagSKESK && tag != tagMarker {
			return ids, true
		}

		if length > len(data)-start {
			return ids, false
		}

		// An SKESK (a passphrase recipient) or marker packet is skipped:
		// the next packet may still be a PKESK.
		if tag == tagPKESK {
			if id, ok := pkeskKeyID(data[start : start+length]); ok {
				ids = append(ids, id)
			}
		}

		data = data[start+length:]
	}

	return ids, false
}

// packetHeader decodes the header of the packet leading data (which must
// not be empty): its tag, where its body starts and how long it is. A
// packet whose length isn't known up front (a partial or indeterminate
// length) is errUnsupported: session key packets never use one.
func packetHeader(data []byte) (tag byte, start, length int, err error) {
	b := data[0]
	if b&0x80 == 0 {
		return 0, 0, 0, errUnsupported
	}

	var n int

	if b&0x40 != 0 {
		tag = b & 0x3f
		length, n, err = newFormatLength(data[1:])
	} else {
		tag = (b >> 2) & 0x0f
		length, n, err = oldFormatLength(data[1:], b&0x03)
	}

	if err != nil {
		return 0, 0, 0, err
	}

	return tag, 1 + n, length, nil
}

// newFormatLength decodes a packet length in the OpenPGP format (RFC 9580
// §4.2.1), returning it and how many octets encoded it.
func newFormatLength(data []byte) (length, n int, err error) {
	if len(data) == 0 {
		return 0, 0, errTruncated
	}

	switch o := int(data[0]); {
	case o < 192:
		return o, 1, nil
	case o < 224:
		if len(data) < 2 {
			return 0, 0, errTruncated
		}

		return (o-192)<<8 + int(data[1]) + 192, 2, nil
	case o == 255:
		return fourOctetLength(data[1:], 5)
	default:
		return 0, 0, errUnsupported
	}
}

// oldFormatLength decodes a packet length in the legacy format (RFC 9580
// §4.2.2) of the given length type.
func oldFormatLength(data []byte, lengthType byte) (length, n int, err error) {
	switch lengthType {
	case 0:
		if len(data) < 1 {
			return 0, 0, errTruncated
		}

		return int(data[0]), 1, nil
	case 1:
		if len(data) < 2 {
			return 0, 0, errTruncated
		}

		return int(data[0])<<8 | int(data[1]), 2, nil
	case 2:
		return fourOctetLength(data, 4)
	default:
		return 0, 0, errUnsupported
	}
}

// fourOctetLength decodes the big-endian four-octet length data starts
// with, reporting n as the octets the whole length field took. A length
// beyond any session key packet (or int, on a 32-bit platform) is
// errUnsupported.
func fourOctetLength(data []byte, n int) (int, int, error) {
	if len(data) < 4 {
		return 0, 0, errTruncated
	}

	length := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	if length > maxRead {
		return 0, 0, errUnsupported
	}

	return int(length), n, nil
}

// pkeskKeyID returns the recipient key ID of a PKESK packet body: stated
// directly in version 3, derived from the fingerprint in version 6 (the
// last eight octets of a v4 key's, the first eight of a v6 key's).
func pkeskKeyID(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}

	switch body[0] {
	case 3:
		if len(body) < 9 {
			return "", false
		}

		return strings.ToUpper(hex.EncodeToString(body[1:9])), true
	case 6:
		if len(body) < 2 {
			return "", false
		}

		// body[1] counts the key version octet and the fingerprint after
		// it; zero means an anonymous recipient.
		n := int(body[1])
		if n == 0 {
			return HiddenKeyID, true
		}

		if n < 1+8 || len(body) < 2+n {
			return "", false
		}

		keyVersion, fpr := body[2], body[3:2+n]
		if keyVersion == 6 {
			return strings.ToUpper(hex.EncodeToString(fpr[:8])), true
		}

		return strings.ToUpper(hex.EncodeToString(fpr[len(fpr)-8:])), true
	default:
		return "", false
	}
}

// dearmor decodes as much of the ASCII-armored message in armored as it
// holds: the base64 body after the armor headers, up to the checksum or
// end line. A body cut off early decodes to its complete prefix.
func dearmor(armored []byte) []byte {
	var (
		body      strings.Builder
		pastBegin bool
		inBody    bool
	)

	sc := bufio.NewScanner(bytes.NewReader(armored))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		switch {
		case !pastBegin:
			pastBegin = line == armorBegin
		case !inBody:
			// Armor headers ("Version: ...") end at the first blank line.
			inBody = line == ""
		case line == "" || strings.HasPrefix(line, "=") || strings.HasPrefix(line, "-----"):
			return decodeBase64Prefix(body.String())
		default:
			body.WriteString(line)
		}
	}

	return decodeBase64Prefix(body.String())
}

// decodeBase64Prefix decodes the longest prefix of s made of whole
// four-character groups, or returns nil if that isn't valid base64.
func decodeBase64Prefix(s string) []byte {
	out, err := base64.StdEncoding.DecodeString(s[:len(s)-len(s)%4])
	if err != nil {
		return nil
	}

	return out
}
