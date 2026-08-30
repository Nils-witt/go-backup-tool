package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// queryRows runs query against db and collects one T per row via scan,
// wrapping any error (the query itself, a per-row scan failure, or a final
// rows.Err()) with errMsg. Shared by every "list of rows" reader in
// schedule_state.go and retention.go's expiredRetentionPaths, which
// otherwise repeat the same query/scan/append/rows.Err boilerplate with
// only the query, args, and scan function differing.
func queryRows[T any](ctx context.Context, db *sql.DB, errMsg, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", errMsg, err)
	}
	defer func() { _ = rows.Close() }()

	var out []T

	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", errMsg, err)
		}

		out = append(out, v)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", errMsg, err)
	}

	return out, nil
}

// queryRowOptional runs query against db and scans a single row via scan,
// reporting (zero, false, nil) if no row matched rather than an error.
// Shared by ReadLastSuccess, ReadLastRun, and ReadLastReceiverEvent, whose
// "row may legitimately not exist yet" handling is otherwise duplicated.
func queryRowOptional[T any](ctx context.Context, db *sql.DB, errMsg, query string, args []any, scan func(*sql.Row) (T, error)) (T, bool, error) {
	var zero T

	v, err := scan(db.QueryRowContext(ctx, query, args...))

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return zero, false, nil
	case err != nil:
		return zero, false, fmt.Errorf("%s: %w", errMsg, err)
	default:
		return v, true, nil
	}
}
