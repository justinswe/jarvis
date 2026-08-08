package store

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storeDefaults() config.GuildConfig {
	return config.GuildConfig{Settings: config.ServerSettings{
		Prompt:               "You are Jarvis.",
		ThreadMessages:       5,
		ParentMessages:       5,
		ChannelMessages:      5,
		HistoryRunes:         4000,
		MaxOutputTokens:      1024,
		MessageTimeout:       time.Minute,
		MessageRetentionDays: 14,
		ReasoningEffort:      llm.ReasoningMedium,
	}}
}

// memoryStore opens an in-memory SQLite store. The suite below runs against it on every
// `bazel test` and against a real PostgreSQL through the manual postgres_live target.
func memoryStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Config{
		Driver: DriverSQLite, SQLitePath: ":memory:", Defaults: storeDefaults(),
		MCPEncryptionKey: testMCPKey(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// testMCPKey is the deterministic 32-byte secret key the suite seals MCP tokens with.
func testMCPKey() []byte {
	return bytes.Repeat([]byte{42}, 32)
}

func TestOpenAppliesMigrationsIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jarvis.db")
	for range 2 {
		s, err := Open(context.Background(), Config{
			Driver: DriverSQLite, SQLitePath: path, Defaults: storeDefaults(),
		})
		require.NoError(t, err)
		var version int64
		require.NoError(t, s.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version))
		assert.Equal(t, int64(2), version)
		require.NoError(t, s.Close())
	}
}

// TestSQLiteStateSurvivesAReopen is the single-container persistence story: a restart of
// the combined image must come back with its configuration and history intact.
func TestSQLiteStateSurvivesAReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jarvis.db")
	open := func() *Store {
		s, err := Open(context.Background(), Config{
			Driver: DriverSQLite, SQLitePath: path, Defaults: storeDefaults(),
		})
		require.NoError(t, err)
		return s
	}

	s := open()
	prompt := "persisted"
	written, err := s.Update(context.Background(), "42", "7", config.Patch{GuildPrompt: &prompt})
	require.NoError(t, err)
	require.NoError(t, s.Record(context.Background(), plainMessage("1000", "10", "42"), 14))
	require.NoError(t, s.Close())

	reopened := open()
	defer func() { _ = reopened.Close() }()
	loaded, err := reopened.Load(context.Background(), "42")
	require.NoError(t, err)
	assert.Equal(t, written, loaded)
	messages, err := reopened.Messages(context.Background(), "42", "10", 10, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"1000"}, messageIDs(messages))
}

func TestOpenRejectsInvalidConfiguration(t *testing.T) {
	_, err := Open(context.Background(), Config{Driver: DriverSQLite, SQLitePath: ":memory:"})
	assert.ErrorContains(t, err, "default guild configuration")
	_, err = Open(context.Background(), Config{Driver: DriverSQLite, Defaults: storeDefaults()})
	assert.ErrorContains(t, err, "SQLite path")
	_, err = Open(context.Background(), Config{Driver: DriverPostgres, Defaults: storeDefaults()})
	assert.ErrorContains(t, err, "DSN")
	_, err = Open(context.Background(), Config{Driver: Driver("mystery"), Defaults: storeDefaults()})
	assert.ErrorContains(t, err, "unsupported store driver")
}

func TestSuiteOnSQLite(t *testing.T) {
	runStoreSuite(t, memoryStore)
}

// runStoreSuite asserts the behavior both backends must share. openStore returns a fresh,
// empty store per subtest.
func runStoreSuite(t *testing.T, openStore func(*testing.T) *Store) {
	t.Run("GetReturnsDefaultsForUnknownGuild", func(t *testing.T) {
		s := openStore(t)
		loaded, err := s.Get(context.Background(), "42")
		require.NoError(t, err)
		assert.Equal(t, storeDefaults().Settings, loaded.Settings)
		assert.Empty(t, loaded.Tier)
		assert.Zero(t, loaded.Version)
	})

	t.Run("LoadRejectsANonNumericGuildID", func(t *testing.T) {
		s := openStore(t)
		_, err := s.Load(context.Background(), "not-a-snowflake")
		assert.ErrorContains(t, err, "invalid Discord ID")
	})

	t.Run("UpdatePersistsAPatchAndIncrementsTheVersion", func(t *testing.T) {
		s := openStore(t)
		prompt := "Guild-specific rules."
		updated, err := s.Update(context.Background(), "42", "7", config.Patch{GuildPrompt: &prompt})
		require.NoError(t, err)
		assert.Equal(t, int64(1), updated.Version)
		assert.Equal(t, prompt, updated.Settings.GuildPrompt)

		loaded, err := s.Load(context.Background(), "42")
		require.NoError(t, err)
		assert.Equal(t, updated, loaded)

		enabled := true
		second, err := s.Update(context.Background(), "42", "7", config.Patch{WebSearchEnabled: &enabled})
		require.NoError(t, err)
		assert.Equal(t, int64(2), second.Version)
		assert.Equal(t, prompt, second.Settings.GuildPrompt)
		assert.True(t, second.Settings.WebSearchEnabled)
	})

	t.Run("UpdateWithoutChangesWritesNothing", func(t *testing.T) {
		s := openStore(t)
		prompt := "same"
		first, err := s.Update(context.Background(), "42", "7", config.Patch{GuildPrompt: &prompt})
		require.NoError(t, err)
		again, err := s.Update(context.Background(), "42", "7", config.Patch{GuildPrompt: &prompt})
		require.NoError(t, err)
		assert.Equal(t, first.Version, again.Version)
	})

	t.Run("UpdateRejectsAnInvalidPatch", func(t *testing.T) {
		s := openStore(t)
		tooMany := 10_000
		_, err := s.Update(context.Background(), "42", "7", config.Patch{ThreadMessages: &tooMany})
		require.Error(t, err)
		loaded, err := s.Load(context.Background(), "42")
		require.NoError(t, err)
		assert.Zero(t, loaded.Version)
	})

	t.Run("MutationsRequireGuildAndActor", func(t *testing.T) {
		s := openStore(t)
		prompt := "p"
		_, err := s.Update(context.Background(), "", "7", config.Patch{GuildPrompt: &prompt})
		assert.ErrorContains(t, err, "guild ID is required")
		_, err = s.Update(context.Background(), "42", " ", config.Patch{GuildPrompt: &prompt})
		assert.ErrorContains(t, err, "actor ID is required")
	})

	t.Run("AdminsAreAddedRemovedAndNormalized", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		_, err := s.AddAdmin(ctx, "42", "7", "  900  ")
		require.NoError(t, err)
		updated, err := s.AddAdmin(ctx, "42", "7", "100")
		require.NoError(t, err)
		assert.Equal(t, []string{"100", "900"}, updated.AdminUserIDs)
		assert.True(t, updated.IsAdmin("900"))

		again, err := s.AddAdmin(ctx, "42", "7", "900")
		require.NoError(t, err)
		assert.Equal(t, updated.Version, again.Version, "re-adding an admin writes nothing")

		removed, err := s.RemoveAdmin(ctx, "42", "7", "900")
		require.NoError(t, err)
		assert.Equal(t, []string{"100"}, removed.AdminUserIDs)

		_, err = s.AddAdmin(ctx, "42", "7", " ")
		assert.ErrorContains(t, err, "admin user ID is required")
	})

	t.Run("TierResolvesThroughTheOwningAccount", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		// Standing in for the external accounts API, which owns these tables.
		seedAccount(t, s, "pro", 900, 42)

		loaded, err := s.Load(ctx, "42")
		require.NoError(t, err)
		assert.Equal(t, "pro", loaded.Tier)

		unlinked, err := s.Load(ctx, "43")
		require.NoError(t, err)
		assert.Empty(t, unlinked.Tier)
	})

	t.Run("MCPServersAreAttachedUpdatedAndRemoved", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		updated, err := s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "https://mcp.example.com/github"})
		require.NoError(t, err)
		require.Len(t, updated.MCPServers, 1)
		assert.Equal(t, config.MCPServer{Name: "github", URL: "https://mcp.example.com/github", Enabled: true}, updated.MCPServers[0])

		// Re-attaching updates in place; a token upgrades the credential.
		updated, err = s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "https://mcp.example.com/v2", AuthToken: "bearer-secret"})
		require.NoError(t, err)
		require.Len(t, updated.MCPServers, 1)
		assert.True(t, updated.MCPServers[0].HasAuth)
		assert.Equal(t, "https://mcp.example.com/v2", updated.MCPServers[0].URL)

		// An empty token on an update keeps the stored credential.
		updated, err = s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "https://mcp.example.com/v3"})
		require.NoError(t, err)
		assert.True(t, updated.MCPServers[0].HasAuth)

		got, err := s.Get(ctx, "42")
		require.NoError(t, err)
		require.Len(t, got.MCPServers, 1, "the request read path sees attached servers")

		removed, err := s.RemoveMCPServer(ctx, "42", "7", "github")
		require.NoError(t, err)
		assert.Empty(t, removed.MCPServers)
		_, err = s.RemoveMCPServer(ctx, "42", "7", "github")
		assert.ErrorContains(t, err, "not attached")
	})

	t.Run("MCPServerRejectsBadNamesAndURLs", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		_, err := s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "Bad_Name", URL: "https://mcp.example.com"})
		assert.ErrorContains(t, err, "lowercase")
		_, err = s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "http://mcp.example.com"})
		assert.ErrorContains(t, err, "https")
		_, err = s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "https://user:pass@mcp.example.com"})
		assert.ErrorContains(t, err, "credentials")
		_, err = s.AddMCPServer(ctx, "42", "", config.MCPServerInput{Name: "github", URL: "https://mcp.example.com"})
		assert.ErrorContains(t, err, "actor ID is required")
	})

	t.Run("MCPServerAuthRoundTripsEncryptedAtRest", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		_, err := s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "https://mcp.example.com", AuthToken: "the-secret-token"})
		require.NoError(t, err)
		token, err := s.MCPServerAuth(ctx, "42", "github")
		require.NoError(t, err)
		assert.Equal(t, "the-secret-token", token)

		// Encrypted at rest: the raw column never carries the plaintext.
		var stored string
		require.NoError(t, s.db.QueryRowContext(ctx, s.q(
			`SELECT auth_ciphertext FROM guild_mcp_servers WHERE guild_id = ? AND name = ?`), 42, "github").Scan(&stored))
		assert.NotEmpty(t, stored)
		assert.NotContains(t, stored, "the-secret-token")

		_, err = s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "open", URL: "https://open.example.com"})
		require.NoError(t, err)
		token, err = s.MCPServerAuth(ctx, "42", "open")
		require.NoError(t, err)
		assert.Empty(t, token, "a tokenless server needs no authentication")
		_, err = s.MCPServerAuth(ctx, "42", "missing")
		assert.ErrorContains(t, err, "not attached")
	})

	t.Run("MCPServerTokenRequiresTheEncryptionKey", func(t *testing.T) {
		s := openStore(t)
		s.mcpKey = nil
		ctx := context.Background()
		_, err := s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "https://mcp.example.com", AuthToken: "secret"})
		assert.ErrorContains(t, err, "encryption key")
		_, err = s.AddMCPServer(ctx, "42", "7", config.MCPServerInput{Name: "github", URL: "https://mcp.example.com"})
		require.NoError(t, err, "tokenless servers never need the key")
	})

	t.Run("RecordAndMessagesRoundTrip", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		message := &discordgo.Message{
			ID: "2000", ChannelID: "10", GuildID: "42", Content: "hello world",
			Type:             discordgo.MessageTypeReply,
			Author:           &discordgo.User{ID: "900", Username: "user", GlobalName: "User", Bot: false},
			Mentions:         []*discordgo.User{{ID: "901"}, {ID: "902"}},
			MessageReference: &discordgo.MessageReference{ChannelID: "10", MessageID: "1999"},
		}
		require.NoError(t, s.Record(ctx, message, 14))
		require.NoError(t, s.Record(ctx, message, 14), "duplicate delivery is idempotent")

		messages, err := s.Messages(ctx, "42", "10", 10, "")
		require.NoError(t, err)
		require.Len(t, messages, 1)
		got := messages[0]
		assert.Equal(t, "2000", got.ID)
		assert.Equal(t, "42", got.GuildID)
		assert.Equal(t, "10", got.ChannelID)
		assert.Equal(t, "hello world", got.Content)
		assert.Equal(t, discordgo.MessageTypeReply, got.Type)
		assert.Equal(t, "900", got.Author.ID)
		assert.Equal(t, "User", got.Author.GlobalName)
		require.Len(t, got.Mentions, 2)
		assert.Equal(t, "901", got.Mentions[0].ID)
		require.NotNil(t, got.MessageReference)
		assert.Equal(t, "1999", got.MessageReference.MessageID)
	})

	t.Run("MessagesReturnsNewestFirstBeforeTheCursor", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		for _, id := range []string{"3000", "1000", "2000"} {
			require.NoError(t, s.Record(ctx, plainMessage(id, "10", "42"), 14))
		}
		messages, err := s.Messages(ctx, "42", "10", 10, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"3000", "2000", "1000"}, messageIDs(messages))

		before, err := s.Messages(ctx, "42", "10", 10, "3000")
		require.NoError(t, err)
		assert.Equal(t, []string{"2000", "1000"}, messageIDs(before))

		limited, err := s.Messages(ctx, "42", "10", 1, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"3000"}, messageIDs(limited))
	})

	t.Run("MessagesFiltersByGuildAndExcludesExpired", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		require.NoError(t, s.Record(ctx, plainMessage("1000", "10", "42"), 14))
		require.NoError(t, s.Record(ctx, plainMessage("2000", "10", "77"), 14))
		// Backdate one message past its retention; the sweeper has not run.
		_, err := s.db.Exec(s.q(`UPDATE messages SET expires_at = 1 WHERE message_id = ?`), 1000)
		require.NoError(t, err)

		messages, err := s.Messages(ctx, "42", "10", 10, "")
		require.NoError(t, err)
		assert.Empty(t, messageIDs(messages), "expired and foreign-guild messages are invisible")

		other, err := s.Messages(ctx, "77", "10", 10, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"2000"}, messageIDs(other))
	})

	t.Run("SweepDeletesExpiredMessagesAndLapsedClaims", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		require.NoError(t, s.Record(ctx, plainMessage("1000", "10", "42"), 14))
		_, err := s.db.Exec(s.q(`UPDATE messages SET expires_at = 1 WHERE message_id = ?`), 1000)
		require.NoError(t, err)
		seedClaim(t, s, 10, 999, -10)

		require.NoError(t, s.sweepOnce(ctx))
		assert.Zero(t, countRows(t, s, "messages"))
		assert.Zero(t, countRows(t, s, "reply_claims"))
	})

	t.Run("ClaimReplyRefusesASecondCopy", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		won, err := s.ClaimReply(ctx, "10", "2000")
		require.NoError(t, err)
		assert.True(t, won)
		// One subscription spans every site, so the duplicate can land on this same
		// process; owning the claim must not matter.
		again, err := s.ClaimReply(ctx, "10", "2000")
		require.NoError(t, err)
		assert.False(t, again)
	})

	t.Run("ClaimReplyAdmitsExactlyOneOfManyConcurrentCopies", func(t *testing.T) {
		s := openStore(t)
		var wins atomic.Int64
		var group sync.WaitGroup
		for range 8 {
			group.Add(1)
			go func() {
				defer group.Done()
				won, err := s.ClaimReply(context.Background(), "10", "2000")
				assert.NoError(t, err)
				if won {
					wins.Add(1)
				}
			}()
		}
		group.Wait()
		assert.Equal(t, int64(1), wins.Load(), "the conditional upsert is the whole dedup guarantee")
	})

	t.Run("ClaimReplyRetakesALapsedClaim", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		seedClaim(t, s, 10, 2000, -5)
		won, err := s.ClaimReply(ctx, "10", "2000")
		require.NoError(t, err)
		assert.True(t, won)
	})

	t.Run("HoldReplyOutlastsEveryDeliverableCopy", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		won, err := s.ClaimReply(ctx, "10", "2000")
		require.NoError(t, err)
		require.True(t, won)
		require.NoError(t, s.HoldReply(ctx, "10", "2000"))

		var expires int64
		require.NoError(t, s.db.QueryRow(s.q(
			`SELECT expires_at FROM reply_claims WHERE channel_id = ? AND message_id = ?`), 10, 2000).Scan(&expires))
		assert.InDelta(t, time.Now().Add(replyHoldTTL).Unix(), expires, 10,
			"the hold matches the queue's own retention")

		lost, err := s.ClaimReply(ctx, "10", "2000")
		require.NoError(t, err)
		assert.False(t, lost, "a late duplicate finds the claim held")
	})

	t.Run("HoldReplyIsUnconditional", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		seedClaim(t, s, 10, 2000, 3600) // someone else holds a live claim
		require.NoError(t, s.HoldReply(ctx, "10", "2000"))
		var owner string
		require.NoError(t, s.db.QueryRow(s.q(
			`SELECT owner FROM reply_claims WHERE channel_id = ? AND message_id = ?`), 10, 2000).Scan(&owner))
		assert.Equal(t, s.replyOwner, owner)
	})

	t.Run("ClaimReplyValidatesItsKey", func(t *testing.T) {
		s := openStore(t)
		_, err := s.ClaimReply(context.Background(), "", "2000")
		assert.ErrorContains(t, err, "channel ID")
		_, err = s.ClaimReply(context.Background(), "10", "")
		assert.ErrorContains(t, err, "message ID")
	})
}

// seedAccount writes a tier, account, and guild link the way the external accounts API
// would, straight through SQL.
func seedAccount(t *testing.T, s *Store, tier string, userID, guildID int64) {
	t.Helper()
	_, err := s.db.Exec(s.q(
		`INSERT INTO tiers (name, requests_per_second, burst, tokens_per_hour) VALUES (?, ?, ?, ?)`),
		tier, 1, 2, 200000)
	require.NoError(t, err)
	_, err = s.db.Exec(s.q(
		`INSERT INTO accounts (user_id, tier, created_at, updated_at) VALUES (?, ?, 0, 0)`), userID, tier)
	require.NoError(t, err)
	_, err = s.db.Exec(s.q(
		`INSERT INTO account_guilds (guild_id, account_user_id, created_at) VALUES (?, ?, 0)`), guildID, userID)
	require.NoError(t, err)
}

// seedClaim inserts a claim owned by another process expiring offset seconds from now.
func seedClaim(t *testing.T, s *Store, channelID, messageID, offsetSeconds int64) {
	t.Helper()
	_, err := s.db.Exec(s.q(
		`INSERT INTO reply_claims (channel_id, message_id, owner, expires_at) VALUES (?, ?, 'other/1', @now + ?)`),
		channelID, messageID, offsetSeconds)
	require.NoError(t, err)
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&count))
	return count
}

func plainMessage(id, channelID, guildID string) *discordgo.Message {
	return &discordgo.Message{
		ID: id, ChannelID: channelID, GuildID: guildID, Content: "content " + id,
		Type: discordgo.MessageTypeDefault, Author: &discordgo.User{ID: "900", Username: "user"},
	}
}

func messageIDs(messages []*discordgo.Message) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}
