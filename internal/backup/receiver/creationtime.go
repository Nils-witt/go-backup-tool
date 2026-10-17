package receiver

import (
	"fmt"
	"time"
)

// parseCreationTime parses raw (the receiver API's creationTime query
// parameter, RFC3339) against now (the time the upload request is being
// handled), returning the zero Time and no error when raw is empty —
// meaning "not provided, use upload time" (see RecordObjectWrite). A
// non-empty raw that fails to parse, or that names a time not strictly
// before now, is an error.
func parseCreationTime(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid creationTime %q: %w", raw, err)
	}

	if !t.Before(now) {
		return time.Time{}, fmt.Errorf("creationTime %q must be before the upload time", raw)
	}

	return t, nil
}
