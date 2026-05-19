//go:build !cgo_sqlite

package sqlite

import (
	"errors"

	sqlite "modernc.org/sqlite"
)

const driverName = "sqlite"

// isBusyError reports whether err carries a SQLite SQLITE_BUSY
// condition. Both the plain code (5) and extended codes
// (SQLITE_BUSY_RECOVERY = 261, SQLITE_BUSY_SNAPSHOT = 517,
// SQLITE_BUSY_TIMEOUT = 773) end with 5 in the low byte; mask
// to the primary code for a single check. The retry layer in
// RunInTransaction treats all of these as transient.
func isBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	const sqliteBusy = 5
	return sqliteErr.Code()&0xff == sqliteBusy
}

// dsn builds a modernc.org/sqlite DSN from a path and pragma
// key-value pairs. Each pair is formatted as _pragma=key(value) in
// the query string.
func dsn(path string, pragmas [][2]string) string {
	s := path
	for i, p := range pragmas {
		if i == 0 {
			s += "?"
		} else {
			s += "&"
		}
		s += "_pragma=" + p[0] + "(" + p[1] + ")"
	}
	return s
}
