package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSuiteOnPostgres runs the exact suite the SQLite tests run, against a real
// PostgreSQL. This is the only place the postgres dialect executes: the unit tests cover
// the shared SQL, so placeholder rebinding, FOR UPDATE, and the server-clock expression
// are verified only here.
func TestSuiteOnPostgres(t *testing.T) {
	if manualTestOptions.postgresDSN == "" {
		t.Skip("set --postgres-dsn (or POSTGRES_DSN) to run live PostgreSQL tests")
	}
	runStoreSuite(t, postgresStore)
}

// postgresStore opens the configured PostgreSQL database and empties every table, so each
// subtest starts from the clean state the SQLite suite gets from :memory:.
func postgresStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Config{
		Driver: DriverPostgres, PostgresDSN: manualTestOptions.postgresDSN, Defaults: storeDefaults(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.db.Exec(`TRUNCATE tiers, accounts, account_guilds, subscriptions,
		guild_configs, guild_admins, messages, reply_claims CASCADE`)
	require.NoError(t, err)
	return s
}
