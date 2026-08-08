// Package store persists Jarvis state in PostgreSQL or SQLite behind one implementation.
//
// The two backends run the same SQL; the dialect struct carries the whole divergence.
// PostgreSQL is the shared store for HA and multi-site deployments, SQLite the
// zero-infrastructure option for one site — it lives in-process, so nothing supervises it
// and nothing can fail over to it.
package store

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the pgx database/sql driver
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	_ "modernc.org/sqlite" // registers the sqlite database/sql driver
)

// Driver selects the storage backend.
type Driver string

const (
	DriverNone     Driver = "none"
	DriverPostgres Driver = "postgres"
	DriverSQLite   Driver = "sqlite"
)

// sweepBatch bounds one retention delete, so expiry never holds a long write transaction.
const sweepBatch = 5000

// Config configures one Store.
type Config struct {
	Driver      Driver
	PostgresDSN string
	SQLitePath  string
	// Defaults is the validated guild configuration served when a guild has none stored.
	Defaults config.GuildConfig
	// SweepInterval is how often expired messages and lapsed claims are deleted. Zero
	// disables the sweeper, which only tests want.
	SweepInterval time.Duration
	// MCPEncryptionKey is the 32-byte AES-256 key sealing guild MCP auth tokens at
	// rest. Empty disables attaching servers that need authentication.
	MCPEncryptionKey []byte
}

// Store implements guild configuration, message history, reply claims, and tier
// resolution over one SQL database.
type Store struct {
	db       *sql.DB
	d        dialect
	defaults config.GuildConfig
	// mcpKey seals guild MCP auth tokens at rest; see crypto.go.
	mcpKey []byte
	now    func() time.Time
	// replyOwner names this process in the claims it takes. It is recorded for the
	// operator reading a claim row, never matched on: see ClaimReply.
	replyOwner string
	// replyClaimTTL bounds how long a claim survives. See SetReplyClaimTTL.
	replyClaimTTL time.Duration
	stop          chan struct{}
	stopped       sync.WaitGroup
	closeOnce     sync.Once
}

// Open connects, applies pending schema migrations, and starts the retention sweeper.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if err := cfg.Defaults.Validate(); err != nil {
		return nil, errors.Wrap(err, "validate default guild configuration")
	}
	if len(cfg.MCPEncryptionKey) != 0 && len(cfg.MCPEncryptionKey) != 32 {
		return nil, errors.New("the MCP encryption key must be exactly 32 bytes")
	}
	db, d, err := open(cfg)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, errors.Wrapf(err, "connect to %s store", cfg.Driver)
	}
	if err := migrate(ctx, db, d); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{
		db: db, d: d, defaults: cloneConfig(cfg.Defaults), now: time.Now,
		mcpKey:     append([]byte(nil), cfg.MCPEncryptionKey...),
		replyOwner: replyClaimOwner(), replyClaimTTL: defaultReplyClaimTTL,
		stop: make(chan struct{}),
	}
	if cfg.SweepInterval > 0 {
		s.stopped.Add(1)
		go s.sweepLoop(cfg.SweepInterval)
	}
	return s, nil
}

func open(cfg Config) (*sql.DB, dialect, error) {
	switch cfg.Driver {
	case DriverPostgres:
		if strings.TrimSpace(cfg.PostgresDSN) == "" {
			return nil, dialect{}, errors.New("PostgreSQL DSN is required")
		}
		db, err := sql.Open("pgx", cfg.PostgresDSN)
		if err != nil {
			return nil, dialect{}, errors.Wrap(err, "open PostgreSQL store")
		}
		db.SetMaxOpenConns(8)
		return db, postgresDialect, nil
	case DriverSQLite:
		path := strings.TrimSpace(cfg.SQLitePath)
		if path == "" {
			return nil, dialect{}, errors.New("SQLite path is required")
		}
		db, err := sql.Open("sqlite", sqliteDSN(path))
		if err != nil {
			return nil, dialect{}, errors.Wrap(err, "open SQLite store")
		}
		if strings.Contains(path, ":memory:") {
			// Every pooled connection to :memory: would otherwise open its own empty
			// database.
			db.SetMaxOpenConns(1)
		} else {
			db.SetMaxOpenConns(4)
		}
		return db, sqliteDialect, nil
	default:
		return nil, dialect{}, errors.Errorf("unsupported store driver %q", cfg.Driver)
	}
}

// sqliteDSN opens path with the pragmas a concurrent long-lived service needs: WAL so
// reads never block the writer, a busy timeout instead of immediate SQLITE_BUSY, enforced
// foreign keys, and immediate write transactions so read-modify-write never deadlocks on
// a lock upgrade.
func sqliteDSN(path string) string {
	return "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
}

// Close stops the sweeper and releases the database.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.stop)
		s.stopped.Wait()
		err = s.db.Close()
	})
	return err
}

// q finalizes one query for the active dialect: @now becomes the backend's own clock and
// the `?` placeholders take the backend's positional form.
func (s *Store) q(query string) string {
	return s.d.rebind(strings.ReplaceAll(query, "@now", s.d.now))
}

func (s *Store) sweepLoop(interval time.Duration) {
	defer s.stopped.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if err := s.sweepOnce(context.Background()); err != nil {
				app.L().Warn("Retention sweep failed", zap.Error(err))
			}
		}
	}
}

// sweepOnce deletes expired messages in bounded batches, then lapsed reply claims. The
// read side already filters expired rows, so a failed sweep costs disk, never correctness.
func (s *Store) sweepOnce(ctx context.Context) error {
	for {
		result, err := s.db.ExecContext(ctx, s.q(`
			DELETE FROM messages WHERE (channel_id, message_id) IN
			(SELECT channel_id, message_id FROM messages WHERE expires_at <= @now LIMIT ?)`),
			sweepBatch)
		if err != nil {
			return errors.Wrap(err, "delete expired messages")
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return errors.Wrap(err, "count expired messages")
		}
		if deleted < sweepBatch {
			break
		}
	}
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM reply_claims WHERE expires_at <= @now`))
	return errors.Wrap(err, "delete lapsed reply claims")
}

// snowflake parses one Discord identifier. Every Discord ID is a numeric snowflake, which
// is what lets the schema store them as integers and order messages without key padding.
func snowflake(id string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil || value <= 0 {
		return 0, errors.Errorf("invalid Discord ID %q", id)
	}
	return value, nil
}

// optionalSnowflake maps the empty ID (a direct message's guild, a missing reference) to
// zero, the schema's "absent" value.
func optionalSnowflake(id string) (int64, error) {
	if strings.TrimSpace(id) == "" {
		return 0, nil
	}
	return snowflake(id)
}

// replyClaimOwner identifies this process in the claims it takes.
func replyClaimOwner() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	return host + "/" + strconv.Itoa(os.Getpid())
}
