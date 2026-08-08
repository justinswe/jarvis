package store

import (
	"context"
	"database/sql"
	"embed"
	"sort"
	"strconv"
	"strings"

	"github.com/justinswe/std/errors"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies every embedded migration newer than the last applied version, each in
// its own transaction, so a partially failed upgrade leaves a resumable schema.
func migrate(ctx context.Context, db *sql.DB, d dialect) error {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return errors.Wrap(err, "create schema_migrations table")
	}
	var applied int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&applied); err != nil {
		return errors.Wrap(err, "read applied schema version")
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return errors.Wrap(err, "list embedded migrations")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return err
		}
		if version <= applied {
			continue
		}
		if err := applyMigration(ctx, db, d, entry.Name(), version); err != nil {
			return errors.Wrapf(err, "apply migration %s", entry.Name())
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, d dialect, name string, version int64) error {
	body, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		return errors.Wrap(err, "read migration")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return errors.Wrap(err, "begin migration transaction")
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range splitStatements(string(body)) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return errors.Wrapf(err, "execute %q", firstLine(statement))
		}
	}
	if _, err := tx.ExecContext(ctx, d.rebind(`INSERT INTO schema_migrations (version) VALUES (?)`), version); err != nil {
		return errors.Wrap(err, "record applied migration")
	}
	return errors.Wrap(tx.Commit(), "commit migration")
}

// migrationVersion parses the numeric prefix of NNNN_name.sql.
func migrationVersion(name string) (int64, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, errors.Errorf("migration %s is not named NNNN_description.sql", name)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || version <= 0 {
		return 0, errors.Errorf("migration %s is not named NNNN_description.sql", name)
	}
	return version, nil
}

// splitStatements strips comment lines and splits a migration on semicolons. That is
// sufficient because the schema contains no procedural bodies; revisit only if a
// migration ever needs one.
func splitStatements(body string) []string {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	var statements []string
	for _, statement := range strings.Split(strings.Join(kept, "\n"), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

func firstLine(statement string) string {
	line, _, _ := strings.Cut(statement, "\n")
	return line
}
