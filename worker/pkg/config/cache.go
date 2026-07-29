package config

import (
	"context"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/cache"
)

// CachedProvider decorates a Provider with a Valkey-backed read-through cache, so repeated
// reads for the same guild within ttl are served without a call to next.
type CachedProvider struct {
	next  Provider
	cache *cache.Client
	ttl   time.Duration
}

// NewCachedProvider wraps next with a read-through cache backed by cacheClient.
func NewCachedProvider(next Provider, cacheClient *cache.Client, ttl time.Duration) *CachedProvider {
	return &CachedProvider{next: next, cache: cacheClient, ttl: ttl}
}

// Get returns the cached configuration for guildID, loading and caching it from next on a
// cache miss.
func (p *CachedProvider) Get(ctx context.Context, guildID string) (GuildConfig, error) {
	return cache.GetOrLoad(ctx, p.cache, guildConfigCacheKey(guildID), p.ttl,
		func(loadCtx context.Context) (GuildConfig, error) { return p.next.Get(loadCtx, guildID) })
}

// CachedManager decorates a Manager, invalidating the shared guild-configuration cache
// after every successful mutation. Load passes straight through uncached: it is the
// strict, admin-facing read used immediately after a mutation (see the
// get_server_configuration tool), where a stale cache read would be a visible bug.
type CachedManager struct {
	next  Manager
	cache *cache.Client
}

// NewCachedManager wraps next so its mutations invalidate cacheClient's cached entry for
// the affected guild.
func NewCachedManager(next Manager, cacheClient *cache.Client) *CachedManager {
	return &CachedManager{next: next, cache: cacheClient}
}

// Load reads guildID's configuration directly from next, bypassing the cache.
func (m *CachedManager) Load(ctx context.Context, guildID string) (GuildConfig, error) {
	return m.next.Load(ctx, guildID)
}

// Update applies patch and invalidates the cached configuration for guildID on success.
func (m *CachedManager) Update(ctx context.Context, guildID, actorID string, patch Patch) (GuildConfig, error) {
	updated, err := m.next.Update(ctx, guildID, actorID, patch)
	if err != nil {
		return GuildConfig{}, err
	}
	m.invalidate(ctx, guildID)
	return updated, nil
}

// AddAdmin delegates admin, then invalidates the cached configuration for guildID.
func (m *CachedManager) AddAdmin(ctx context.Context, guildID, actorID, userID string) (GuildConfig, error) {
	updated, err := m.next.AddAdmin(ctx, guildID, actorID, userID)
	if err != nil {
		return GuildConfig{}, err
	}
	m.invalidate(ctx, guildID)
	return updated, nil
}

// RemoveAdmin delegates admin, then invalidates the cached configuration for guildID.
func (m *CachedManager) RemoveAdmin(ctx context.Context, guildID, actorID, userID string) (GuildConfig, error) {
	updated, err := m.next.RemoveAdmin(ctx, guildID, actorID, userID)
	if err != nil {
		return GuildConfig{}, err
	}
	m.invalidate(ctx, guildID)
	return updated, nil
}

// SetTier delegates tier, then invalidates the cached configuration for guildID.
func (m *CachedManager) SetTier(ctx context.Context, guildID, actorID, tier string) (GuildConfig, error) {
	updated, err := m.next.SetTier(ctx, guildID, actorID, tier)
	if err != nil {
		return GuildConfig{}, err
	}
	m.invalidate(ctx, guildID)
	return updated, nil
}

// invalidate deletes the cached configuration for guildID, outliving ctx so a caller's
// deadline can't skip invalidation after a mutation has already committed.
func (m *CachedManager) invalidate(ctx context.Context, guildID string) {
	cache.Delete(context.WithoutCancel(ctx), m.cache, guildConfigCacheKey(guildID))
}

// guildConfigCacheKey returns the cache key namespace shared by CachedProvider and
// CachedManager for one guild's configuration.
func guildConfigCacheKey(guildID string) string {
	return cache.GuildKey("guildconfig", guildID)
}
