// Package valkeyconn dials a shared Valkey connection for the worker's Valkey-backed
// dependencies (usage metering, guild-configuration caching) so they can be pooled
// behind one client instead of opening a connection each.
package valkeyconn

import (
	"context"
	"crypto/tls"
	"time"

	"github.com/justinswe/std/errors"
	"github.com/valkey-io/valkey-go"
)

// HashTag wraps a guild ID in a Valkey Cluster hash tag.
//
// Every key belonging to one guild must carry the identical tag, because the usage
// scripts declare only one key in KEYS and build the rest by concatenation in Lua, and a
// cluster client rejects a script touching more than one slot. It lives here, in the
// package both Valkey-backed callers already share, so the convention has exactly one
// definition: a divergent copy would break cluster mode only, which single-node local
// testing never exercises.
func HashTag(guildID string) string {
	return "{" + guildID + "}"
}

// Config configures a Valkey connection.
type Config struct {
	Addresses []string
	Username  string
	Password  string
	SelectDB  int
	TLS       bool
	Timeout   time.Duration
}

// Dial connects to Valkey and verifies connectivity with a PING before returning.
func Dial(ctx context.Context, cfg Config) (valkey.Client, error) {
	option := valkey.ClientOption{
		InitAddress: cfg.Addresses,
		Username:    cfg.Username,
		Password:    cfg.Password,
		SelectDB:    cfg.SelectDB,
		// Client-side caching requires RESP3 tracking that several managed Valkey
		// providers reject, and every current caller already bounds its own reads
		// with an explicit TTL, so the client-side cache adds nothing.
		DisableCache: true,
		// valkey-go only monitors context cancellation in pipeline mode, which otherwise
		// starts only under concurrent load. Without this, deadlines are advisory.
		AlwaysPipelining: true,
	}
	if cfg.TLS {
		option.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client, err := valkey.NewClient(option)
	if err != nil {
		return nil, errors.Wrap(err, "connect to Valkey")
	}
	pingCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if err := client.Do(pingCtx, client.B().Ping().Build()).Error(); err != nil {
		client.Close()
		return nil, errors.Wrap(err, "verify Valkey connectivity")
	}
	return client, nil
}
