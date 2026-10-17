package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parseOnErrorCommandTimeout parses a commands: entry's timeout: string,
// defaulting to defaultOnErrorCommandTimeout when unset. A non-positive
// timeout is rejected: it would leave no time to actually run the command.
func parseOnErrorCommandTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultOnErrorCommandTimeout, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing timeout %q: %w", s, err)
	}

	if d <= 0 {
		return 0, fmt.Errorf("timeout must be positive, got %q", s)
	}

	return d, nil
}

// parseConfigTimeout parses the config file's top-level timeout: string, if
// any, into a time.Duration; an empty string (unset) means no overall run
// timeout.
func parseConfigTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing config file timeout %q: %w", s, err)
	}

	return d, nil
}

// parseRetention parses a local server's retention: string into a
// time.Duration. An empty string means no automatic expiry (the zero
// value); a negative duration is rejected since "delete files from the
// future" isn't meaningful.
func parseRetention(s string) (time.Duration, error) {
	return parseOptionalDayDuration("retention", s, true)
}

// parseOptionalDayDuration parses field's duration string s via
// parseDayDuration; an empty s means unset, returning the zero duration.
// allowZero permits a zero duration (rejecting only negative values, for
// retention:, where "delete from the future" isn't meaningful); when false,
// zero is rejected too (for stale-after:, where "stale after zero time" is
// always true and so isn't a meaningful setting). Shared by parseRetention
// and parseStaleAfter (receivers.go), which otherwise duplicate this trim/
// empty/parse/sign-check shape with only the field name and boundary
// differing.
func parseOptionalDayDuration(field, s string, allowZero bool) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	d, err := parseDayDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parsing %s %q: %w", field, s, err)
	}

	if allowZero && d < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %q", field, s)
	}

	if !allowZero && d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", field, s)
	}

	return d, nil
}

// dayUnitRE matches a leading "<number>d" component (e.g. "7d" or "1.5d") of
// a duration string handled by parseDayDuration.
var dayUnitRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)d`)

// parseDayDuration is time.ParseDuration extended with a "d" (day) unit,
// which the standard library doesn't support: e.g. "30d" or "1d12h" for
// go-backup-tool's retention: (time.ParseDuration's "m" already means
// minutes, so that unit needs no extra support). A day component, if
// present, must come first, mirroring time.ParseDuration's largest-to-
// smallest unit ordering; whatever follows it (if anything) is parsed by
// time.ParseDuration as usual and added on.
func parseDayDuration(s string) (time.Duration, error) {
	rest := s

	neg := false

	switch {
	case strings.HasPrefix(rest, "-"):
		neg, rest = true, rest[1:]
	case strings.HasPrefix(rest, "+"):
		rest = rest[1:]
	}

	if rest == "" {
		return 0, fmt.Errorf("invalid duration %q", s)
	}

	var days float64

	if m := dayUnitRE.FindStringSubmatch(rest); m != nil {
		var err error

		days, err = strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}

		rest = rest[len(m[0]):]
	}

	var rem time.Duration

	if rest != "" {
		var err error

		rem, err = time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
	}

	total := time.Duration(days*24*float64(time.Hour)) + rem
	if neg {
		total = -total
	}

	return total, nil
}
