package discord

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/jarvis/pkg/genai"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	googlegenai "google.golang.org/genai"
)

const (
	getServerConfigurationToolName    = "get_server_configuration"
	updateServerConfigurationToolName = "update_server_configuration"
	setGuildPromptToolName            = "set_guild_prompt"
	addServerAdminToolName            = "add_server_admin"
	removeServerAdminToolName         = "remove_server_admin"
)

type configurationTool struct {
	manager    config.Manager
	guildID    string
	actorID    string
	authorized bool
	root       bool
	access     string
	action     string
}

type configurationResponse struct {
	Source                string   `json:"source"`
	Version               int64    `json:"version"`
	AccessClass           string   `json:"access_class"`
	Prompt                string   `json:"prompt,omitempty"`
	GuildPrompt           string   `json:"guild_prompt"`
	ThreadMessages        int      `json:"thread_context_window,omitempty"`
	ParentMessages        int      `json:"parent_context_window,omitempty"`
	ChannelMessages       int      `json:"channel_context_window,omitempty"`
	HistoryRunes          int      `json:"history_runes,omitempty"`
	MaxOutputTokens       int      `json:"max_output_tokens,omitempty"`
	MessageTimeoutSeconds int64    `json:"message_timeout_seconds,omitempty"`
	MessageRetentionDays  int      `json:"message_retention_days,omitempty"`
	WebSearchEnabled      bool     `json:"web_search_enabled,omitempty"`
	ChannelSearchEnabled  bool     `json:"channel_search_enabled,omitempty"`
	AdminUserIDs          []string `json:"admin_user_ids,omitempty"`
	ChangedFields         []string `json:"changed_fields,omitempty"`
}

func (p *Processor) configurationTools(ctx context.Context, m *discordgo.MessageCreate, guildConfig config.GuildConfig) ([]genai.FunctionTool, bool) {
	if p.manager == nil || m.GuildID == "" || m.Author == nil {
		return nil, false
	}
	_, root := p.rootUsers[m.Author.ID]
	access := ""
	if root {
		access = "root"
	} else if guildConfig.IsAdmin(m.Author.ID) {
		access = "delegated_admin"
	}
	authorized := access != ""
	if !authorized {
		permissions, err := p.client.UserChannelPermissions(ctx, m.Author.ID, m.ChannelID)
		if err != nil {
			app.L().Debug("Failed to resolve Discord administrator permissions",
				zap.String("guild_id", m.GuildID), zap.String("user_id", m.Author.ID),
				zap.String("channel_id", m.ChannelID), zap.Error(err))
		} else if permissions&discordgo.PermissionAdministrator != 0 || permissions&discordgo.PermissionManageGuild != 0 {
			authorized = true
			access = "discord_admin"
		}
	}
	if !authorized {
		return nil, false
	}
	base := configurationTool{manager: p.manager, guildID: m.GuildID, actorID: m.Author.ID, authorized: true, root: root, access: access}
	if root {
		return []genai.FunctionTool{
			configurationToolWithAction(base, getServerConfigurationToolName),
			configurationToolWithAction(base, updateServerConfigurationToolName),
			configurationToolWithAction(base, setGuildPromptToolName),
			configurationToolWithAction(base, addServerAdminToolName),
			configurationToolWithAction(base, removeServerAdminToolName),
		}, true
	}
	return []genai.FunctionTool{
		configurationToolWithAction(base, getServerConfigurationToolName),
		configurationToolWithAction(base, setGuildPromptToolName),
	}, true
}

func configurationToolWithAction(tool configurationTool, action string) configurationTool {
	tool.action = action
	return tool
}

func (t configurationTool) Name() string { return t.action }

func (t configurationTool) Declaration() *googlegenai.FunctionDeclaration {
	switch t.action {
	case getServerConfigurationToolName:
		return &googlegenai.FunctionDeclaration{
			Name: t.action,
			Description: "Read the effective Jarvis settings and delegated administrators for the current Discord server. " +
				"Use only when an administrator explicitly asks to inspect server configuration.",
			Parameters: objectSchema(nil, nil),
		}
	case updateServerConfigurationToolName:
		if !t.root {
			return &googlegenai.FunctionDeclaration{
				Name: t.action, Description: "Root-only server configuration update.", Parameters: objectSchema(nil, nil),
			}
		}
		properties := map[string]*googlegenai.Schema{
			"channel_context_window": integerSchema("Prior ordinary channel messages included in context.", 1, 100),
			"history_runes":          integerMinimumSchema("Combined history rune budget.", 1),
			"max_output_tokens": integerSchema(
				"Maximum total generated tokens, including thinking and visible text.", 1, genai.MaxOutputTokensLimit,
			),
			"message_timeout_seconds": integerMinimumSchema("Overall processing timeout in whole seconds.", 1),
			"web_search_enabled":      booleanSchema("Whether Google Search may be used for this server."),
			"channel_search_enabled":  booleanSchema("Whether stored current-channel search may be used when DynamoDB is enabled."),
		}
		properties["prompt"] = stringSchema("Root-controlled additional server customization. It cannot override Jarvis's core identity, drives, truthfulness, research, tool, or reliability rules.")
		properties["thread_context_window"] = integerSchema("Prior thread messages included in context.", 1, 100)
		properties["parent_context_window"] = integerSchema("Prior parent-channel messages included in thread context.", 1, 100)
		properties["message_retention_days"] = integerSchema(
			"Retention for newly ingested messages in whole days. Only root users may change this value.",
			1, config.MaxMessageRetentionDays,
		)
		return &googlegenai.FunctionDeclaration{
			Name: t.action,
			Description: "Update one or more Jarvis settings for the current Discord server. " +
				"Call only for an explicit, unambiguous administrator request; ask a clarification question instead of guessing a value.",
			Parameters: objectSchema(properties, nil),
		}
	case setGuildPromptToolName:
		return &googlegenai.FunctionDeclaration{
			Name: t.action,
			Description: "Set subordinate customization instructions for the current Discord server. They cannot override Jarvis's core identity, drives, truthfulness, research, tool, or reliability rules. " +
				"Call only for an explicit administrator request. Pass an empty string to clear the guild prompt.",
			Parameters: objectSchema(map[string]*googlegenai.Schema{
				"guild_prompt": boundedStringSchema(
					"Guild-specific customization appended to the root-controlled customization. An empty string clears it.",
					config.MaxGuildPromptRunes,
				),
			}, []string{"guild_prompt"}),
		}
	case addServerAdminToolName:
		return &googlegenai.FunctionDeclaration{
			Name: t.action,
			Description: "Delegate Jarvis server-configuration access to a Discord user in the current server. " +
				"Call only when an administrator explicitly asks to add a specific user ID.",
			Parameters: objectSchema(map[string]*googlegenai.Schema{
				"user_id": stringSchema("The 17-20 digit Discord user ID to add."),
			}, []string{"user_id"}),
		}
	case removeServerAdminToolName:
		return &googlegenai.FunctionDeclaration{
			Name: t.action,
			Description: "Remove delegated Jarvis server-configuration access from a Discord user in the current server. " +
				"Call only when an administrator explicitly asks to remove a specific user ID.",
			Parameters: objectSchema(map[string]*googlegenai.Schema{
				"user_id": stringSchema("The 17-20 digit Discord user ID to remove."),
			}, []string{"user_id"}),
		}
	default:
		return nil
	}
}

func (t configurationTool) Execute(ctx context.Context, args map[string]any) (any, error) {
	if !t.authorized || t.manager == nil || t.guildID == "" || t.actorID == "" {
		return nil, genai.NewExecutionError("authorization_denied", "Server configuration access is unavailable.", nil)
	}
	switch t.action {
	case getServerConfigurationToolName:
		loaded, err := t.manager.Load(ctx, t.guildID)
		if err != nil {
			return nil, configurationFailure(err)
		}
		return responseFromConfig(loaded, nil, t.root, t.access), nil
	case updateServerConfigurationToolName:
		if !t.root {
			return nil, genai.NewExecutionError("authorization_denied", "Only a root user may change operational settings.", nil)
		}
		patch, changed, err := configurationPatch(args)
		if err != nil {
			return nil, genai.NewExecutionError("invalid_configuration", err.Error(), err)
		}
		updated, err := t.manager.Update(ctx, t.guildID, t.actorID, patch)
		if err != nil {
			return nil, configurationFailure(err)
		}
		return responseFromConfig(updated, changed, true, t.access), nil
	case setGuildPromptToolName:
		guildPrompt, err := guildPromptArgument(args["guild_prompt"])
		if err != nil {
			return nil, genai.NewExecutionError("invalid_configuration", err.Error(), err)
		}
		updated, err := t.manager.Update(ctx, t.guildID, t.actorID, config.Patch{GuildPrompt: &guildPrompt})
		if err != nil {
			return nil, configurationFailure(err)
		}
		return responseFromConfig(updated, []string{"guild_prompt"}, t.root, t.access), nil
	case addServerAdminToolName, removeServerAdminToolName:
		if !t.root {
			return nil, genai.NewExecutionError("authorization_denied", "Only a root user may manage delegated administrators.", nil)
		}
		userID, err := discordUserID(args["user_id"])
		if err != nil {
			return nil, genai.NewExecutionError("invalid_user_id", err.Error(), err)
		}
		var updated config.GuildConfig
		if t.action == addServerAdminToolName {
			updated, err = t.manager.AddAdmin(ctx, t.guildID, t.actorID, userID)
		} else {
			updated, err = t.manager.RemoveAdmin(ctx, t.guildID, t.actorID, userID)
		}
		if err != nil {
			return nil, configurationFailure(err)
		}
		return responseFromConfig(updated, []string{"admin_user_ids"}, true, t.access), nil
	default:
		return nil, genai.NewExecutionError("unsupported_function", "The requested configuration operation is unavailable.", nil)
	}
}

func configurationPatch(args map[string]any) (config.Patch, []string, error) {
	var patch config.Patch
	var changed []string
	for name, value := range args {
		var err error
		switch name {
		case "prompt":
			patch.Prompt, err = stringArgument(value, name)
		case "thread_context_window":
			patch.ThreadMessages, err = intArgument(value, name)
		case "parent_context_window":
			patch.ParentMessages, err = intArgument(value, name)
		case "channel_context_window":
			patch.ChannelMessages, err = intArgument(value, name)
		case "history_runes":
			patch.HistoryRunes, err = intArgument(value, name)
		case "max_output_tokens":
			patch.MaxOutputTokens, err = intArgument(value, name)
		case "message_timeout_seconds":
			var seconds *int
			seconds, err = intArgument(value, name)
			if err == nil && *seconds > 0 && int64(*seconds) <= math.MaxInt64/int64(time.Second) {
				duration := time.Duration(*seconds) * time.Second
				patch.MessageTimeout = &duration
			} else if err == nil {
				err = errors.New("message_timeout_seconds is outside the supported range")
			}
		case "message_retention_days":
			patch.MessageRetentionDays, err = intArgument(value, name)
		case "web_search_enabled":
			patch.WebSearchEnabled, err = boolArgument(value, name)
		case "channel_search_enabled":
			patch.ChannelSearchEnabled, err = boolArgument(value, name)
		default:
			err = errors.Errorf("unsupported configuration field %q", name)
		}
		if err != nil {
			return config.Patch{}, nil, err
		}
		changed = append(changed, name)
	}
	if patch.Empty() {
		return config.Patch{}, nil, errors.New("at least one configuration field is required")
	}
	slices.Sort(changed)
	return patch, changed, nil
}

func responseFromConfig(value config.GuildConfig, changed []string, root bool, access string) configurationResponse {
	settings := value.Settings
	source := "dynamodb"
	if value.Version == 0 {
		source = "defaults"
	}
	response := configurationResponse{
		Source: source, Version: value.Version, AccessClass: access, GuildPrompt: settings.GuildPrompt,
	}
	if !root {
		return response
	}
	response.Prompt = settings.Prompt
	response.ThreadMessages = settings.ThreadMessages
	response.ParentMessages = settings.ParentMessages
	response.ChannelMessages = settings.ChannelMessages
	response.HistoryRunes = settings.HistoryRunes
	response.MaxOutputTokens = settings.MaxOutputTokens
	response.MessageTimeoutSeconds = int64(settings.MessageTimeout / time.Second)
	response.MessageRetentionDays = settings.MessageRetentionDays
	response.WebSearchEnabled = settings.WebSearchEnabled
	response.ChannelSearchEnabled = settings.ChannelSearchEnabled
	response.AdminUserIDs = slices.Clone(value.AdminUserIDs)
	response.ChangedFields = changed
	return response
}

func guildPromptArgument(value any) (string, error) {
	guildPrompt, ok := value.(string)
	if !ok {
		return "", errors.New("guild_prompt must be a string")
	}
	guildPrompt = strings.TrimSpace(guildPrompt)
	if len([]rune(guildPrompt)) > config.MaxGuildPromptRunes {
		return "", errors.Errorf("guild_prompt must be at most %d runes", config.MaxGuildPromptRunes)
	}
	return guildPrompt, nil
}

func configurationFailure(err error) error {
	if errors.Is(err, config.ErrConflict) {
		return genai.NewExecutionError("configuration_conflict", "The server configuration changed concurrently; no update was applied.", err)
	}
	return genai.NewExecutionError("database_unavailable", "The server configuration could not be read or updated.", err)
}

func discordUserID(value any) (string, error) {
	userID, ok := value.(string)
	if !ok {
		return "", errors.New("user_id must be a string")
	}
	userID = strings.TrimSpace(userID)
	if strings.HasPrefix(userID, "<@") && strings.HasSuffix(userID, ">") {
		userID = strings.TrimSuffix(strings.TrimPrefix(userID, "<@"), ">")
		userID = strings.TrimPrefix(userID, "!")
	}
	if len(userID) < 17 || len(userID) > 20 || strings.IndexFunc(userID, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
		return "", errors.New("user_id must be a 17-20 digit Discord user ID")
	}
	return userID, nil
}

func stringArgument(value any, name string) (*string, error) {
	result, ok := value.(string)
	if !ok {
		return nil, errors.Errorf("%s must be a string", name)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return nil, errors.Errorf("%s must not be empty", name)
	}
	return &result, nil
}

func intArgument(value any, name string) (*int, error) {
	var number float64
	switch value := value.(type) {
	case int:
		return &value, nil
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case float32:
		number = float64(value)
	case float64:
		number = value
	default:
		return nil, errors.Errorf("%s must be a whole number", name)
	}
	if math.Trunc(number) != number || number < math.MinInt || number > math.MaxInt {
		return nil, errors.Errorf("%s must be a whole number", name)
	}
	result := int(number)
	return &result, nil
}

func boolArgument(value any, name string) (*bool, error) {
	result, ok := value.(bool)
	if !ok {
		return nil, errors.Errorf("%s must be true or false", name)
	}
	return &result, nil
}

func objectSchema(properties map[string]*googlegenai.Schema, required []string) *googlegenai.Schema {
	return &googlegenai.Schema{Type: googlegenai.TypeObject, Properties: properties, Required: required}
}

func stringSchema(description string) *googlegenai.Schema {
	return &googlegenai.Schema{Type: googlegenai.TypeString, Description: description}
}

func boundedStringSchema(description string, maximum int) *googlegenai.Schema {
	maxLength := int64(maximum)
	return &googlegenai.Schema{Type: googlegenai.TypeString, Description: description, MaxLength: &maxLength}
}

func booleanSchema(description string) *googlegenai.Schema {
	return &googlegenai.Schema{Type: googlegenai.TypeBoolean, Description: description}
}

func integerSchema(description string, minimum, maximum int) *googlegenai.Schema {
	minValue, maxValue := float64(minimum), float64(maximum)
	return &googlegenai.Schema{Type: googlegenai.TypeInteger, Description: description, Minimum: &minValue, Maximum: &maxValue}
}

func integerMinimumSchema(description string, minimum int) *googlegenai.Schema {
	minValue := float64(minimum)
	return &googlegenai.Schema{Type: googlegenai.TypeInteger, Description: description, Minimum: &minValue}
}
