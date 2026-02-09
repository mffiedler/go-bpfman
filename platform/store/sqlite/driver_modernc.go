//go:build !cgo_sqlite

package sqlite

import (
	_ "modernc.org/sqlite"
)

const driverName = "sqlite"

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
