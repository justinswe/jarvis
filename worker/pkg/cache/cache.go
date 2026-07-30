// Package cache is a small, generic Valkey-backed read-through cache. It exists to avoid
// repeated reads of slow-changing per-request data (starting with guild configuration)
// without tying every caller to Valkey's client API or its own serialization/TTL/timeout
// handling.
package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/valkeyconn"
	"github.com/justinswe/std/app"
	"github.com/valkey-io/valkey-go"
	"go.uber.org/zap"
)

// schemaVersion namespaces every key so the cache layout can evolve.
const schemaVersion = "v1"

// commander runs the raw Valkey commands behind Client, isolating the live valkey.Client
// so unit tests can fake it without a Valkey connection.
type commander interface {
	get(ctx context.Context, key string) ([]byte, bool, error)
	set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	del(ctx context.Context, key string) error
}

// Client is a generic, fail-open Valkey-backed cache.
//
// Every operation is fail-open: a Valkey error is logged and treated as a cache miss on
// reads, or logged and dropped on writes. Valkey here is a cache, not a source of truth,
// so callers must always be able to recompute a value on a miss.
type Client struct {
	commands commander
	prefix   string
	timeout  time.Duration
}

// New creates a cache client over an already-connected, externally owned Valkey client.
// The caller remains responsible for closing client.
func New(client valkey.Client, keyPrefix string, timeout time.Duration) *Client {
	return &Client{commands: &valkeyCommander{client: client}, prefix: keyPrefix, timeout: timeout}
}

// GuildKey returns a namespaced, hash-tagged key suffix for one guild, so guild-scoped
// keys stay compatible with Valkey Cluster slot pinning. The tag itself comes from
// valkeyconn.HashTag, the one definition shared with usage metering's guildBase. Client
// prefixes the result with the deployment key prefix passed to New.
func GuildKey(namespace, guildID string) string {
	return schemaVersion + ":c:" + valkeyconn.HashTag(guildID) + ":" + namespace
}

// None of Get, Set, or Delete returns an error, because none of them has a failure a
// caller could act on: Valkey here is a cache, not a source of truth, so every failure
// is logged and degrades to a miss or a dropped write. Returning an error that is always
// nil would only invite call sites to handle a case that cannot happen.

// Get reads and decodes one cached value. The second return is false on a miss or failure.
func Get[T any](ctx context.Context, c *Client, key string) (T, bool) {
	var zero T
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	raw, found, err := c.commands.get(callCtx, c.prefix+":"+key)
	if err != nil {
		app.L().Warn("Cache read failed; treating as a miss", zap.String("key", key), zap.Error(err))
		return zero, false
	}
	if !found {
		return zero, false
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		app.L().Warn("Cache value decode failed; treating as a miss", zap.String("key", key), zap.Error(err))
		return zero, false
	}
	return value, true
}

// Set encodes and writes one cached value with the given time-to-live. A failed write
// only costs a future cache miss.
//
// A ttl of zero or less writes no expiry at all, so callers that rely on a bounded
// staleness must validate their TTL rather than passing one through.
func Set[T any](ctx context.Context, c *Client, key string, value T, ttl time.Duration) {
	raw, err := json.Marshal(value)
	if err != nil {
		app.L().Warn("Cache value encode failed; skipping the write", zap.String("key", key), zap.Error(err))
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.commands.set(callCtx, c.prefix+":"+key, raw, ttl); err != nil {
		app.L().Warn("Cache write failed", zap.String("key", key), zap.Error(err))
	}
}

// Delete removes one cached value. On failure the value stays cached until its
// time-to-live expires.
func Delete(ctx context.Context, c *Client, key string) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.commands.del(callCtx, c.prefix+":"+key); err != nil {
		app.L().Warn("Cache delete failed", zap.String("key", key), zap.Error(err))
	}
}

// GetOrLoad reads key from the cache, falling back to load and populating the cache with
// its result (for ttl) on a miss. An error from load is returned unchanged.
func GetOrLoad[T any](ctx context.Context, c *Client, key string, ttl time.Duration, load func(context.Context) (T, error)) (T, error) {
	if value, found := Get[T](ctx, c, key); found {
		return value, nil
	}
	value, err := load(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	Set(ctx, c, key, value, ttl)
	return value, nil
}

// valkeyCommander is the production commander backed by a live Valkey client.
type valkeyCommander struct {
	client valkey.Client
}

func (c *valkeyCommander) get(ctx context.Context, key string) ([]byte, bool, error) {
	resp := c.client.Do(ctx, c.client.B().Get().Key(key).Build())
	if err := resp.Error(); err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	raw, err := resp.AsBytes()
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

func (c *valkeyCommander) set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	cmd := c.client.B().Set().Key(key).Value(string(value))
	if ttl > 0 {
		return c.client.Do(ctx, cmd.Px(ttl).Build()).Error()
	}
	return c.client.Do(ctx, cmd.Build()).Error()
}

func (c *valkeyCommander) del(ctx context.Context, key string) error {
	return c.client.Do(ctx, c.client.B().Del().Key(key).Build()).Error()
}
