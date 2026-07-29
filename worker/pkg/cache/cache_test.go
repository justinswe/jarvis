package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCommander struct {
	mu       sync.Mutex
	store    map[string][]byte
	getErr   error
	setErr   error
	delErr   error
	getCalls []string
	setCalls []string
	delCalls []string
}

func newFakeCommander() *fakeCommander {
	return &fakeCommander{store: map[string][]byte{}}
}

func (f *fakeCommander) get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, key)
	if f.getErr != nil {
		return nil, false, f.getErr
	}
	value, found := f.store[key]
	return value, found, nil
}

func (f *fakeCommander) set(_ context.Context, key string, value []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, key)
	if f.setErr != nil {
		return f.setErr
	}
	f.store[key] = value
	return nil
}

func (f *fakeCommander) del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCalls = append(f.delCalls, key)
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.store, key)
	return nil
}

func newTestClient(commands commander) *Client {
	return &Client{commands: commands, prefix: "jarvis", timeout: 50 * time.Millisecond}
}

type testValue struct {
	Name  string
	Count int
}

func TestGetReportsAMissWhenUnset(t *testing.T) {
	client := newTestClient(newFakeCommander())

	_, found := Get[testValue](context.Background(), client, "k1")
	assert.False(t, found)
}

func TestSetThenGetRoundTrips(t *testing.T) {
	client := newTestClient(newFakeCommander())
	want := testValue{Name: "guild-1", Count: 3}

	Set(context.Background(), client, "k1", want, time.Minute)
	got, found := Get[testValue](context.Background(), client, "k1")
	require.True(t, found)
	assert.Equal(t, want, got)
}

func TestGetTreatsCommanderErrorAsAMiss(t *testing.T) {
	commands := newFakeCommander()
	commands.getErr = errors.New("valkey unavailable")
	client := newTestClient(commands)

	value, found := Get[testValue](context.Background(), client, "k1")
	assert.False(t, found, "a Valkey failure must degrade to a miss")
	assert.Zero(t, value)
}

func TestGetTreatsMalformedValueAsAMiss(t *testing.T) {
	commands := newFakeCommander()
	commands.store["jarvis:k1"] = []byte("not json")
	client := newTestClient(commands)

	_, found := Get[testValue](context.Background(), client, "k1")
	assert.False(t, found)
}

func TestSetSwallowsACommanderError(t *testing.T) {
	commands := newFakeCommander()
	commands.setErr = errors.New("valkey unavailable")
	client := newTestClient(commands)

	Set(context.Background(), client, "k1", testValue{Name: "x"}, time.Minute)

	_, found := Get[testValue](context.Background(), client, "k1")
	assert.False(t, found, "a failed write must only cost a future cache miss")
}

func TestDeleteSwallowsACommanderError(t *testing.T) {
	commands := newFakeCommander()
	commands.delErr = errors.New("valkey unavailable")
	client := newTestClient(commands)

	Delete(context.Background(), client, "k1")

	assert.Equal(t, []string{"jarvis:k1"}, commands.delCalls, "the delete must have been attempted")
}

func TestGetOrLoadPopulatesTheCacheOnAMiss(t *testing.T) {
	commands := newFakeCommander()
	client := newTestClient(commands)
	calls := 0

	value, err := GetOrLoad(context.Background(), client, "k1", time.Minute, func(context.Context) (testValue, error) {
		calls++
		return testValue{Name: "loaded"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, testValue{Name: "loaded"}, value)
	assert.Equal(t, 1, calls)
	assert.Contains(t, commands.setCalls, "jarvis:k1", "a miss must populate the cache")
}

func TestGetOrLoadSkipsLoadOnAHit(t *testing.T) {
	commands := newFakeCommander()
	client := newTestClient(commands)
	Set(context.Background(), client, "k1", testValue{Name: "cached"}, time.Minute)

	value, err := GetOrLoad(context.Background(), client, "k1", time.Minute, func(context.Context) (testValue, error) {
		t.Fatal("load must not be called on a cache hit")
		return testValue{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, testValue{Name: "cached"}, value)
}

func TestGetOrLoadPropagatesTheLoadError(t *testing.T) {
	client := newTestClient(newFakeCommander())
	loadErr := errors.New("source unavailable")

	_, err := GetOrLoad(context.Background(), client, "k1", time.Minute, func(context.Context) (testValue, error) {
		return testValue{}, loadErr
	})
	assert.ErrorIs(t, err, loadErr)
}

func TestGuildKeyIsHashTaggedAndSchemaVersioned(t *testing.T) {
	assert.Equal(t, "v1:c:{g1}:guildconfig", GuildKey("guildconfig", "g1"))
}
