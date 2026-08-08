package config

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubProvider struct {
	mu    sync.Mutex
	calls int
	cfg   GuildConfig
	err   error
}

func (p *stubProvider) Get(context.Context, string) (GuildConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return GuildConfig{}, p.err
	}
	return p.cfg, nil
}

func (p *stubProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type stubManager struct {
	loadCfg     GuildConfig
	loadCalls   int
	updateCfg   GuildConfig
	updateErr   error
	updateCalls int
}

func (m *stubManager) Load(context.Context, string) (GuildConfig, error) {
	m.loadCalls++
	return m.loadCfg, nil
}

func (m *stubManager) Update(context.Context, string, string, Patch) (GuildConfig, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return GuildConfig{}, m.updateErr
	}
	return m.updateCfg, nil
}

func (m *stubManager) AddAdmin(context.Context, string, string, string) (GuildConfig, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return GuildConfig{}, m.updateErr
	}
	return m.updateCfg, nil
}

func (m *stubManager) RemoveAdmin(context.Context, string, string, string) (GuildConfig, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return GuildConfig{}, m.updateErr
	}
	return m.updateCfg, nil
}

func (m *stubManager) AddMCPServer(context.Context, string, string, MCPServerInput) (GuildConfig, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return GuildConfig{}, m.updateErr
	}
	return m.updateCfg, nil
}

func (m *stubManager) RemoveMCPServer(context.Context, string, string, string) (GuildConfig, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return GuildConfig{}, m.updateErr
	}
	return m.updateCfg, nil
}

func TestCachedProviderCachesAcrossCalls(t *testing.T) {
	provider := &stubProvider{cfg: GuildConfig{Tier: "gold"}}
	cachedProvider := NewCachedProvider(provider, cache.NewMemory(time.Second), time.Minute)

	first, err := cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)
	assert.Equal(t, "gold", first.Tier)
	assert.Equal(t, 1, provider.callCount())

	second, err := cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)
	assert.Equal(t, "gold", second.Tier)
	assert.Equal(t, 1, provider.callCount(), "a cache hit must not call the underlying provider again")
}

func TestCachedProviderKeysAreScopedPerGuild(t *testing.T) {
	provider := &stubProvider{cfg: GuildConfig{Tier: "gold"}}
	cachedProvider := NewCachedProvider(provider, cache.NewMemory(time.Second), time.Minute)

	_, err := cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)
	_, err = cachedProvider.Get(context.Background(), "g2")
	require.NoError(t, err)
	assert.Equal(t, 2, provider.callCount(), "different guilds must not share a cache entry")
}

func TestCachedProviderPropagatesTheUnderlyingError(t *testing.T) {
	wantErr := errors.New("store unavailable")
	provider := &stubProvider{err: wantErr}
	cachedProvider := NewCachedProvider(provider, cache.NewMemory(time.Second), time.Minute)

	_, err := cachedProvider.Get(context.Background(), "g1")
	assert.ErrorIs(t, err, wantErr)
}

func TestCachedManagerLoadBypassesTheCache(t *testing.T) {
	sharedCache := cache.NewMemory(time.Second)
	provider := &stubProvider{cfg: GuildConfig{Tier: "cached"}}
	cachedProvider := NewCachedProvider(provider, sharedCache, time.Minute)
	_, err := cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)

	manager := &stubManager{loadCfg: GuildConfig{Tier: "fresh"}}
	cachedManager := NewCachedManager(manager, sharedCache)

	loaded, err := cachedManager.Load(context.Background(), "g1")
	require.NoError(t, err)
	assert.Equal(t, "fresh", loaded.Tier, "Load must always reflect the source of truth, not a stale cache entry")
	assert.Equal(t, 1, manager.loadCalls)
}

func TestCachedManagerInvalidatesOnSuccessfulUpdate(t *testing.T) {
	sharedCache := cache.NewMemory(time.Second)
	provider := &stubProvider{cfg: GuildConfig{Tier: "before"}}
	cachedProvider := NewCachedProvider(provider, sharedCache, time.Minute)
	_, err := cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)
	require.Equal(t, 1, provider.callCount())

	manager := &stubManager{updateCfg: GuildConfig{Tier: "after"}}
	cachedManager := NewCachedManager(manager, sharedCache)
	_, err = cachedManager.Update(context.Background(), "g1", "actor", Patch{Prompt: stringPointer("x")})
	require.NoError(t, err)

	_, err = cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)
	assert.Equal(t, 2, provider.callCount(), "a mutation must invalidate the cache so the next read is fresh")
}

func TestCachedManagerDoesNotInvalidateOnAFailedUpdate(t *testing.T) {
	sharedCache := cache.NewMemory(time.Second)
	provider := &stubProvider{cfg: GuildConfig{Tier: "before"}}
	cachedProvider := NewCachedProvider(provider, sharedCache, time.Minute)
	_, err := cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)
	require.Equal(t, 1, provider.callCount())

	manager := &stubManager{updateErr: errors.New("conflict")}
	cachedManager := NewCachedManager(manager, sharedCache)
	_, err = cachedManager.Update(context.Background(), "g1", "actor", Patch{Prompt: stringPointer("x")})
	require.Error(t, err)

	_, err = cachedProvider.Get(context.Background(), "g1")
	require.NoError(t, err)
	assert.Equal(t, 1, provider.callCount(), "a failed mutation must not invalidate the cache")
}

func TestCachedManagerInvalidatesOnAdminChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CachedManager) error
	}{
		{name: "AddAdmin", mutate: func(m *CachedManager) error {
			_, err := m.AddAdmin(context.Background(), "g1", "actor", "u1")
			return err
		}},
		{name: "RemoveAdmin", mutate: func(m *CachedManager) error {
			_, err := m.RemoveAdmin(context.Background(), "g1", "actor", "u1")
			return err
		}},
		{name: "AddMCPServer", mutate: func(m *CachedManager) error {
			_, err := m.AddMCPServer(context.Background(), "g1", "actor", MCPServerInput{Name: "github", URL: "https://mcp.example.com"})
			return err
		}},
		{name: "RemoveMCPServer", mutate: func(m *CachedManager) error {
			_, err := m.RemoveMCPServer(context.Background(), "g1", "actor", "github")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sharedCache := cache.NewMemory(time.Second)
			provider := &stubProvider{cfg: GuildConfig{Tier: "before"}}
			cachedProvider := NewCachedProvider(provider, sharedCache, time.Minute)
			_, err := cachedProvider.Get(context.Background(), "g1")
			require.NoError(t, err)

			manager := &stubManager{}
			cachedManager := NewCachedManager(manager, sharedCache)
			require.NoError(t, test.mutate(cachedManager))

			_, err = cachedProvider.Get(context.Background(), "g1")
			require.NoError(t, err)
			assert.Equal(t, 2, provider.callCount())
		})
	}
}

func stringPointer(s string) *string { return &s }
