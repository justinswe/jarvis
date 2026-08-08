package store

import (
	"strconv"
	"strings"
)

// dialect is the complete per-backend divergence: everything else in this package is one
// implementation written against the SQL both engines share.
type dialect struct {
	// now is a SQL expression yielding the backend's own clock as unix seconds. Reply
	// claims compare expiries against it so that workers at different sites race one
	// server's clock instead of each other's.
	now string
	// forUpdate locks the row a read-modify-write transaction is about to rewrite.
	// SQLite has no row locks and needs none: its write transactions are serialized.
	forUpdate string
	// rebind converts the package's `?` placeholders to the backend's positional form.
	rebind func(string) string
}

var (
	sqliteDialect = dialect{
		now:    "unixepoch()",
		rebind: func(query string) string { return query },
	}
	postgresDialect = dialect{
		now:       "extract(epoch from now())::bigint",
		forUpdate: " FOR UPDATE",
		rebind:    rebindPositional,
	}
)

// rebindPositional rewrites `?` placeholders as `$1..$n`. The package's queries never
// contain a literal question mark, so no quote tracking is needed.
func rebindPositional(query string) string {
	var builder strings.Builder
	builder.Grow(len(query) + 8)
	n := 0
	for _, r := range query {
		if r != '?' {
			builder.WriteRune(r)
			continue
		}
		n++
		builder.WriteByte('$')
		builder.WriteString(strconv.Itoa(n))
	}
	return builder.String()
}
