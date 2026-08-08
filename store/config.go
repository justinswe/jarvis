package store

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

// querier is the subset of *sql.DB and *sql.Tx the read helpers need, so loads run both
// standalone and inside a mutation's transaction.
type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// guildConfigColumns names every stored settings column, in the one order the scan and
// upsert below both use.
const guildConfigColumns = `prompt, guild_prompt, thread_messages, parent_messages,
	channel_messages, history_runes, max_output_tokens, message_timeout_seconds,
	message_retention_days, web_search_enabled, channel_search_enabled, reasoning_effort,
	primary_model_profile, fallback_model_profile, version, updated_at, updated_by_user_id`

// Get loads guild configuration and falls back to hard-coded defaults on store failures.
func (s *Store) Get(ctx context.Context, guildID string) (config.GuildConfig, error) {
	loaded, err := s.Load(ctx, guildID)
	if err == nil {
		return loaded, nil
	}
	if ctx.Err() != nil {
		return config.GuildConfig{}, ctx.Err()
	}
	app.L().Warn("Guild configuration lookup failed; using defaults",
		zap.String("guild_id", guildID), zap.Error(err))
	return cloneConfig(s.defaults), nil
}

// Load strictly loads one guild configuration for administrative operations.
func (s *Store) Load(ctx context.Context, guildID string) (config.GuildConfig, error) {
	if guildID == "" {
		return cloneConfig(s.defaults), nil
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return config.GuildConfig{}, err
	}
	loaded, err := s.loadGuildConfig(ctx, s.db, gid, false)
	if err != nil {
		return config.GuildConfig{}, err
	}
	if err := loaded.Validate(); err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "validate stored guild configuration")
	}
	return loaded, nil
}

// Update atomically applies a validated settings patch.
func (s *Store) Update(ctx context.Context, guildID, actorID string, patch config.Patch) (config.GuildConfig, error) {
	return s.mutateConfig(ctx, guildID, actorID, patch.Apply)
}

// AddAdmin atomically adds a delegated guild administrator.
func (s *Store) AddAdmin(ctx context.Context, guildID, actorID, userID string) (config.GuildConfig, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return config.GuildConfig{}, errors.New("admin user ID is required")
	}
	return s.mutateConfig(ctx, guildID, actorID, func(current config.GuildConfig) (config.GuildConfig, error) {
		if !slices.Contains(current.AdminUserIDs, userID) {
			current.AdminUserIDs = append(current.AdminUserIDs, userID)
		}
		return current, nil
	})
}

// RemoveAdmin atomically removes a delegated guild administrator.
func (s *Store) RemoveAdmin(ctx context.Context, guildID, actorID, userID string) (config.GuildConfig, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return config.GuildConfig{}, errors.New("admin user ID is required")
	}
	return s.mutateConfig(ctx, guildID, actorID, func(current config.GuildConfig) (config.GuildConfig, error) {
		current.AdminUserIDs = slices.DeleteFunc(current.AdminUserIDs, func(candidate string) bool { return candidate == userID })
		return current, nil
	})
}

// mutateConfig applies one mutation inside a transaction. The row lock (see
// dialect.forUpdate) serializes concurrent mutators, so unlike a conditional-write store
// there is no retry loop and no conflict to surface.
func (s *Store) mutateConfig(ctx context.Context, guildID, actorID string, mutate func(config.GuildConfig) (config.GuildConfig, error)) (config.GuildConfig, error) {
	if strings.TrimSpace(guildID) == "" {
		return config.GuildConfig{}, errors.New("guild ID is required")
	}
	if strings.TrimSpace(actorID) == "" {
		return config.GuildConfig{}, errors.New("configuration actor ID is required")
	}
	gid, err := snowflake(guildID)
	if err != nil {
		return config.GuildConfig{}, err
	}
	actor, err := snowflake(actorID)
	if err != nil {
		return config.GuildConfig{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "begin configuration transaction")
	}
	defer func() { _ = tx.Rollback() }()
	current, err := s.loadGuildConfig(ctx, tx, gid, true)
	if err != nil {
		return config.GuildConfig{}, err
	}
	updated, err := mutate(cloneConfig(current))
	if err != nil {
		return config.GuildConfig{}, err
	}
	updated.AdminUserIDs = normalizedUserIDs(updated.AdminUserIDs)
	if err := updated.Validate(); err != nil {
		return config.GuildConfig{}, err
	}
	if equalConfig(current, updated) {
		return current, nil
	}
	updated.Version = current.Version + 1
	if err := s.writeGuildConfig(ctx, tx, gid, actor, updated); err != nil {
		return config.GuildConfig{}, err
	}
	if err := tx.Commit(); err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "commit guild configuration")
	}
	return updated, nil
}

// loadGuildConfig assembles one guild's configuration: the settings row (defaults when
// absent), its delegated administrators, and the owning account's tier.
func (s *Store) loadGuildConfig(ctx context.Context, q querier, gid int64, lock bool) (config.GuildConfig, error) {
	query := `SELECT ` + guildConfigColumns + ` FROM guild_configs WHERE guild_id = ?`
	if lock {
		query += s.d.forUpdate
	}
	var (
		loaded                        = cloneConfig(s.defaults)
		settings                      config.ServerSettings
		timeoutSeconds                int64
		webSearch, channelSearch      int
		effort                        string
		version, updatedAt, updatedBy int64
	)
	err := q.QueryRowContext(ctx, s.q(query), gid).Scan(
		&settings.Prompt, &settings.GuildPrompt, &settings.ThreadMessages, &settings.ParentMessages,
		&settings.ChannelMessages, &settings.HistoryRunes, &settings.MaxOutputTokens, &timeoutSeconds,
		&settings.MessageRetentionDays, &webSearch, &channelSearch, &effort,
		&settings.PrimaryModelProfile, &settings.FallbackModelProfile, &version, &updatedAt, &updatedBy,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A guild with no stored row runs on the defaults, version 0.
	case err != nil:
		return config.GuildConfig{}, errors.Wrap(err, "read guild configuration")
	default:
		settings.MessageTimeout = time.Duration(timeoutSeconds) * time.Second
		settings.WebSearchEnabled = webSearch != 0
		settings.ChannelSearchEnabled = channelSearch != 0
		settings.ReasoningEffort = reasoningEffort(effort)
		loaded.Settings = settings
		loaded.Version = version
	}
	admins, err := s.loadAdmins(ctx, q, gid)
	if err != nil {
		return config.GuildConfig{}, err
	}
	loaded.AdminUserIDs = admins
	tier, err := s.loadTier(ctx, q, gid)
	if err != nil {
		return config.GuildConfig{}, err
	}
	loaded.Tier = tier
	servers, err := s.loadMCPServers(ctx, q, gid)
	if err != nil {
		return config.GuildConfig{}, err
	}
	loaded.MCPServers = servers
	return loaded, nil
}

func (s *Store) loadAdmins(ctx context.Context, q querier, gid int64) ([]string, error) {
	rows, err := q.QueryContext(ctx, s.q(`SELECT user_id FROM guild_admins WHERE guild_id = ?`), gid)
	if err != nil {
		return nil, errors.Wrap(err, "read guild administrators")
	}
	defer rows.Close()
	var admins []string
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, errors.Wrap(err, "decode guild administrator")
		}
		admins = append(admins, strconv.FormatInt(userID, 10))
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "read guild administrators")
	}
	return normalizedUserIDs(admins), nil
}

// loadTier resolves the guild's effective subscription tier from its owning account. The
// accounts and account_guilds tables are written by the external accounts API, never by
// Jarvis; an unlinked guild runs at the deployment default tier, reported as "".
func (s *Store) loadTier(ctx context.Context, q querier, gid int64) (string, error) {
	var tier string
	err := q.QueryRowContext(ctx, s.q(`
		SELECT a.tier FROM account_guilds g
		JOIN accounts a ON a.user_id = g.account_user_id
		WHERE g.guild_id = ?`), gid).Scan(&tier)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", errors.Wrap(err, "resolve account tier")
	}
	return tier, nil
}

// loadMCPServers reads the guild's attached MCP servers. The ciphertext never leaves
// this function: callers see only whether a credential exists.
func (s *Store) loadMCPServers(ctx context.Context, q querier, gid int64) ([]config.MCPServer, error) {
	rows, err := q.QueryContext(ctx, s.q(
		`SELECT name, url, auth_ciphertext, enabled FROM guild_mcp_servers WHERE guild_id = ? ORDER BY name`), gid)
	if err != nil {
		return nil, errors.Wrap(err, "read guild MCP servers")
	}
	defer rows.Close()
	var servers []config.MCPServer
	for rows.Next() {
		var (
			server     config.MCPServer
			ciphertext string
			enabled    int
		)
		if err := rows.Scan(&server.Name, &server.URL, &ciphertext, &enabled); err != nil {
			return nil, errors.Wrap(err, "decode guild MCP server")
		}
		server.HasAuth = ciphertext != ""
		server.Enabled = enabled != 0
		servers = append(servers, server)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Wrap(err, "read guild MCP servers")
	}
	return servers, nil
}

// AddMCPServer attaches or updates one remote MCP server. It writes the row directly —
// not through the guild_configs mutation funnel — so the external accounts API can
// manage the same rows without racing Jarvis's version counter. An empty auth token on
// an existing row keeps the stored credential.
func (s *Store) AddMCPServer(ctx context.Context, guildID, actorID string, server config.MCPServerInput) (config.GuildConfig, error) {
	gid, err := mcpMutationIDs(guildID, actorID)
	if err != nil {
		return config.GuildConfig{}, err
	}
	if err := config.ValidateMCPServerInput(server); err != nil {
		return config.GuildConfig{}, err
	}
	name := strings.TrimSpace(server.Name)
	serverURL := strings.TrimSpace(server.URL)
	ciphertext := ""
	if strings.TrimSpace(server.AuthToken) != "" {
		ciphertext, err = sealSecret(s.mcpKey, strings.TrimSpace(server.AuthToken))
		if err != nil {
			return config.GuildConfig{}, err
		}
	}
	if _, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO guild_mcp_servers (guild_id, name, url, auth_ciphertext, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, @now, @now)
		ON CONFLICT (guild_id, name) DO UPDATE SET
			url = excluded.url,
			auth_ciphertext = CASE WHEN excluded.auth_ciphertext = ''
				THEN guild_mcp_servers.auth_ciphertext ELSE excluded.auth_ciphertext END,
			enabled = 1, updated_at = excluded.updated_at`),
		gid, name, serverURL, ciphertext); err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "write guild MCP server")
	}
	return s.Load(ctx, guildID)
}

// RemoveMCPServer detaches one remote MCP server by name.
func (s *Store) RemoveMCPServer(ctx context.Context, guildID, actorID, name string) (config.GuildConfig, error) {
	gid, err := mcpMutationIDs(guildID, actorID)
	if err != nil {
		return config.GuildConfig{}, err
	}
	name = strings.TrimSpace(name)
	result, err := s.db.ExecContext(ctx,
		s.q(`DELETE FROM guild_mcp_servers WHERE guild_id = ? AND name = ?`), gid, name)
	if err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "remove guild MCP server")
	}
	if affected, err := result.RowsAffected(); err == nil && affected == 0 {
		return config.GuildConfig{}, errors.Wrapf(config.ErrMCPServerNotAttached, "MCP server %q", name)
	}
	return s.Load(ctx, guildID)
}

// MCPServerAuth decrypts the stored bearer token for one enabled server; "" means the
// server needs no authentication. This is the only place the credential is decrypted.
func (s *Store) MCPServerAuth(ctx context.Context, guildID, name string) (string, error) {
	gid, err := snowflake(guildID)
	if err != nil {
		return "", err
	}
	var ciphertext string
	err = s.db.QueryRowContext(ctx, s.q(
		`SELECT auth_ciphertext FROM guild_mcp_servers WHERE guild_id = ? AND name = ? AND enabled = 1`),
		gid, strings.TrimSpace(name)).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.Wrapf(config.ErrMCPServerNotAttached, "MCP server %q", name)
	}
	if err != nil {
		return "", errors.Wrap(err, "read guild MCP server credential")
	}
	if ciphertext == "" {
		return "", nil
	}
	return openSecret(s.mcpKey, ciphertext)
}

// mcpMutationIDs validates the actor and resolves the guild for an MCP row write.
func mcpMutationIDs(guildID, actorID string) (int64, error) {
	if strings.TrimSpace(actorID) == "" {
		return 0, errors.New("configuration actor ID is required")
	}
	if _, err := snowflake(actorID); err != nil {
		return 0, err
	}
	return snowflake(guildID)
}

// writeGuildConfig upserts the settings row and replaces the administrator set, all
// inside the mutation's transaction.
func (s *Store) writeGuildConfig(ctx context.Context, tx *sql.Tx, gid, actor int64, value config.GuildConfig) error {
	settings := value.Settings
	if _, err := tx.ExecContext(ctx, s.q(`
		INSERT INTO guild_configs (guild_id, `+guildConfigColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (guild_id) DO UPDATE SET
			prompt = excluded.prompt, guild_prompt = excluded.guild_prompt,
			thread_messages = excluded.thread_messages, parent_messages = excluded.parent_messages,
			channel_messages = excluded.channel_messages, history_runes = excluded.history_runes,
			max_output_tokens = excluded.max_output_tokens,
			message_timeout_seconds = excluded.message_timeout_seconds,
			message_retention_days = excluded.message_retention_days,
			web_search_enabled = excluded.web_search_enabled,
			channel_search_enabled = excluded.channel_search_enabled,
			reasoning_effort = excluded.reasoning_effort,
			primary_model_profile = excluded.primary_model_profile,
			fallback_model_profile = excluded.fallback_model_profile,
			version = excluded.version, updated_at = excluded.updated_at,
			updated_by_user_id = excluded.updated_by_user_id`),
		gid, settings.Prompt, settings.GuildPrompt, settings.ThreadMessages, settings.ParentMessages,
		settings.ChannelMessages, settings.HistoryRunes, settings.MaxOutputTokens,
		int64(settings.MessageTimeout/time.Second), settings.MessageRetentionDays,
		boolInt(settings.WebSearchEnabled), boolInt(settings.ChannelSearchEnabled),
		string(settings.ReasoningEffort), settings.PrimaryModelProfile, settings.FallbackModelProfile,
		value.Version, s.now().UTC().Unix(), actor,
	); err != nil {
		return errors.Wrap(err, "write guild configuration")
	}
	if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM guild_admins WHERE guild_id = ?`), gid); err != nil {
		return errors.Wrap(err, "clear guild administrators")
	}
	for _, userID := range value.AdminUserIDs {
		admin, err := snowflake(userID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			s.q(`INSERT INTO guild_admins (guild_id, user_id) VALUES (?, ?)`), gid, admin); err != nil {
			return errors.Wrap(err, "write guild administrator")
		}
	}
	return nil
}

// reasoningEffort defaults rows stored before the setting existed to the medium level.
func reasoningEffort(value string) llm.ReasoningEffort {
	effort := llm.ReasoningEffort(value)
	if !effort.Valid() {
		return llm.ReasoningMedium
	}
	return effort
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func normalizedUserIDs(userIDs []string) []string {
	result := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		userID = strings.TrimSpace(userID)
		if userID != "" && !slices.Contains(result, userID) {
			result = append(result, userID)
		}
	}
	slices.Sort(result)
	return result
}

func cloneConfig(value config.GuildConfig) config.GuildConfig {
	value.AdminUserIDs = slices.Clone(value.AdminUserIDs)
	value.MCPServers = slices.Clone(value.MCPServers)
	return value
}

func equalConfig(left, right config.GuildConfig) bool {
	left.Version, right.Version = 0, 0
	return left.Settings == right.Settings && left.Tier == right.Tier &&
		slices.Equal(normalizedUserIDs(left.AdminUserIDs), normalizedUserIDs(right.AdminUserIDs)) &&
		slices.Equal(left.MCPServers, right.MCPServers)
}
