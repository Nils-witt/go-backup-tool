package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// DefaultReceiverMaxObjectSize is the largest object (or file) a receiver
// accepts when its max-object-size: is unset: 50 GiB.
const DefaultReceiverMaxObjectSize int64 = 50 << 30

// byteSizeUnits maps a size suffix (lowercased) to its multiplier: decimal
// (kB, MB, GB, TB) and binary (KiB, MiB, GiB, TiB) units, plus bare bytes.
var byteSizeUnits = map[string]float64{
	"":    1,
	"b":   1,
	"kb":  1e3,
	"mb":  1e6,
	"gb":  1e9,
	"tb":  1e12,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
}

var byteSizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*)$`)

// ParseByteSize parses a size such as "500MB", "10GiB", "1.5 TB" or a bare
// byte count into bytes. Units are case-insensitive; kB/MB/GB/TB are
// decimal, KiB/MiB/GiB/TiB binary. The result must be at least one byte.
func ParseByteSize(s string) (int64, error) {
	m := byteSizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (want e.g. 500MB or 10GiB)", s)
	}

	unit, ok := byteSizeUnits[strings.ToLower(m[2])]
	if !ok {
		return 0, fmt.Errorf("invalid size %q: unknown unit %q (want B, kB, MB, GB, TB, KiB, MiB, GiB or TiB)", s, m[2])
	}

	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}

	bytes := math.Round(n * unit)
	if bytes < 1 || bytes >= math.MaxInt64 {
		return 0, fmt.Errorf("size %q must be between 1 byte and 8 EiB", s)
	}

	return int64(bytes), nil
}

// parseOptionalByteSize parses field's size string s via ParseByteSize; an
// empty s means unset, returning def.
func parseOptionalByteSize(field, s string, def int64) (int64, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}

	n, err := ParseByteSize(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}

	return n, nil
}
