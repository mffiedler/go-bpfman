//go:build cgo_sqlite

package sqlite

import (
	_ "github.com/mattn/go-sqlite3"
)

const driverName = "sqlite3"

// dsn builds a mattn/go-sqlite3 DSN from a path and pragma key-value
// pairs. Each pair is formatted as _key=value in the query string.
func dsn(path string, pragmas [][2]string) string {
	s := path
	for i, p := range pragmas {
		if i == 0 {
			s += "?"
		} else {
			s += "&"
		}
		s += "_" + p[0] + "=" + p[1]
	}
	return s
}
