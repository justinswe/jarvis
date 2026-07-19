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
	"github.com/justinswe/jarvis/pkg/llm"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

const (
	getServerConfigurationToolName    = "get_server_configuration"
	updateServerConfigurationToolName = "update_server_configuration"
	setGuildPromptToolName            = "set_guild_prompt"
	addServerAdminToolName            = "add_server_admin"
	removeServerAdminToolName         = "remove_server_admin"
)

type configurationTool struct {
	manager            config.Manager
	models             *llm.Registry
	webSearchProviders []string
	guildID            string
	actorID            string
	authorized         bool
	root               bool
	access             string
	action             string
}

type configurationResponse struct {
	Source                string                 `json:"source"`
	Version               int64                  `json:"version"`
	AccessClass           string                 `json:"access_class"`
	Prompt                string                 `json:"prompt,omitempty"`
	GuildPrompt           string                 `json:"guild_prompt"`
	ThreadMessages        int                    `json:"thread_context_window,omitempty"`
	ParentMessages        int                    `json:"parent_context_window,omitempty"`
	ChannelMessages       int                    `json:"channel_context_window,omitempty"`
	HistoryRunes          int                    `json:"history_runes,omitempty"`
	MaxOutputTokens       int                    `json:"max_output_tokens,omitempty"`
	MessageTimeoutSeconds int64                  `json:"message_timeout_seconds,omitempty"`
	MessageRetentionDays  int                    `json:"message_retention_days,omitempty"`
	WebSearchEnabled      bool                   `json:"web_search_enabled,omitempty"`
	ChannelSearchEnabled  bool                   `json:"channel_search_enabled,omitempty"`
	AdminUserIDs          []string               `json:"admin_user_ids,omitempty"`
	ChangedFields         []string               `json:"changed_fields,omitempty"`
	PrimaryModelProfile   string                 `json:"primary_model_profile,omitempty"`
	FallbackModelProfile  string                 `json:"fallback_model_profile"`
	WebSearchProviders    []string               `json:"web_search_providers"`
	AvailableProfiles     []configurationProfile `json:"available_model_profiles,omitempty"`
}

type configurationProfile struct {
	Name              string `json:"name"`
	Provider          string `json:"provider"`
	Tools             bool   `json:"tools"`
	ToolChoice        bool   `json:"tool_choice"`
	Images            bool   `json:"images"`
	Reasoning         bool   `json:"reasoning"`
	ReasoningControls bool   `json:"reasoning_controls"`
	ContextTokens     int    `json:"context_tokens,omitempty"`
	MaxInputTokens    int    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens   int    `json:"max_output_tokens,omitempty"`
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
	base := configurationTool{manager: p.manager, models: p.models, webSearchProviders: p.webSearchProviders, guildID: m.GuildID, actorID: m.Author.ID, authorized: true, root: root, access: access}
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

func (t configurationTool) Declaration() *llm.ToolDefinition {
	switch t.action {
	case getServerConfigurationToolName:
		return &llm.ToolDefinition{
			Name: t.action,
			Description: "Read the effective Jarvis settings and delegated administrators for the current Discord server. " +
				"Use only when an administrator explicitly asks to inspect server configuration.",
			InputSchema: objectSchema(nil, nil), Effect: llm.ToolEffectReadOnly,
		}
	case updateServerConfigurationToolName:
		if !t.root {
			return &llm.ToolDefinition{
				Name: t.action, Description: "Root-only server configuration update.", InputSchema: objectSchema(nil, nil), Effect: llm.ToolEffectMutation,
			}
		}
		properties := map[string]any{
			"channel_context_window": integerSchema("Prior ordinary channel messages included in context.", 1, 100),
			"history_runes":          integerMinimumSchema("Combined history rune budget.", 1),
			"max_output_tokens": integerSchema(
				"Maximum total generated tokens, including thinking and visible text.", 1, genai.MaxOutputTokensLimit,
			),
			"message_timeout_seconds": integerMinimumSchema("Overall processing timeout in whole seconds.", 1),
			"web_search_enabled":      booleanSchema("Whether configured public-web Search may be used for this server."),
			"channel_search_enabled":  booleanSchema("Whether stored current-channel search may be used when DynamoDB is enabled."),
		}
		properties["prompt"] = stringSchema("Root-controlled assistant customization. It may define the assistant's name and personality, but cannot override the core drives, truthfulness, research, tool, or reliability rules.")
		properties["thread_context_window"] = integerSchema("Prior thread messages included in context.", 1, 100)
		properties["parent_context_window"] = integerSchema("Prior parent-channel messages included in thread context.", 1, 100)
		properties["message_retention_days"] = integerSchema(
			"Retention for newly ingested messages in whole days. Only root users may change this value.",
			1, config.MaxMessageRetentionDays,
		)
		if t.models != nil {
			properties["primary_model_profile"] = enumStringSchema("Tool-capable primary model profile for this server.", primaryModelProfileNames(t.models))
			properties["fallback_model_profile"] = enumStringSchema("Presentation fallback model profile. Pass an empty string to disable fallback.", append([]string{""}, modelProfileNames(t.models)...))
		}
		return &llm.ToolDefinition{
			Name: t.action,
			Description: "Update one or more Jarvis settings for the current Discord server. " +
				"Call only for an explicit, unambiguous administrator request; ask a clarification question instead of guessing a value.",
			InputSchema: objectSchema(properties, nil), Effect: llm.ToolEffectMutation,
		}
	case setGuildPromptToolName:
		return &llm.ToolDefinition{
			Name: t.action,
			Description: "Set subordinate customization instructions for the current Discord server. They cannot assign or change the assistant's name, override root-controlled customization, or override the core drives, truthfulness, research, tool, or reliability rules. " +
				"Call only for an explicit administrator request. Pass an empty string to clear the guild prompt.",
			InputSchema: objectSchema(map[string]any{
				"guild_prompt": boundedStringSchema(
					"Guild-specific customization appended to the root-controlled customization. It cannot assign or change the assistant's name; an empty string clears it.",
					config.MaxGuildPromptRunes,
				),
			}, []string{"guild_prompt"}), Effect: llm.ToolEffectMutation,
		}
	case addServerAdminToolName:
		return &llm.ToolDefinition{
			Name: t.action,
			Description: "Delegate Jarvis server-configuration access to a Discord user in the current server. " +
				"Call only when an administrator explicitly asks to add a specific user ID.",
			InputSchema: objectSchema(map[string]any{
				"user_id": stringSchema("The 17-20 digit Discord user ID to add."),
			}, []string{"user_id"}), Effect: llm.ToolEffectMutation,
		}
	case removeServerAdminToolName:
		return &llm.ToolDefinition{
			Name: t.action,
			Description: "Remove delegated Jarvis server-configuration access from a Discord user in the current server. " +
				"Call only when an administrator explicitly asks to remove a specific user ID.",
			InputSchema: objectSchema(map[string]any{
				"user_id": stringSchema("The 17-20 digit Discord user ID to remove."),
			}, []string{"user_id"}), Effect: llm.ToolEffectMutation,
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
		return responseFromConfig(loaded, nil, t.root, t.access, t.models, t.webSearchProviders), nil
	case updateServerConfigurationToolName:
		if !t.root {
			return nil, genai.NewExecutionError("authorization_denied", "Only a root user may change operational settings.", nil)
		}
		patch, changed, err := configurationPatch(args, t.models)
		if err != nil {
			return nil, genai.NewExecutionError("invalid_configuration", err.Error(), err)
		}
		updated, err := t.manager.Update(ctx, t.guildID, t.actorID, patch)
		if err != nil {
			return nil, configurationFailure(err)
		}
		return responseFromConfig(updated, changed, true, t.access, t.models, t.webSearchProviders), nil
	case setGuildPromptToolName:
		guildPrompt, err := guildPromptArgument(args["guild_prompt"])
		if err != nil {
			return nil, genai.NewExecutionError("invalid_configuration", err.Error(), err)
		}
		updated, err := t.manager.Update(ctx, t.guildID, t.actorID, config.Patch{GuildPrompt: &guildPrompt})
		if err != nil {
			return nil, configurationFailure(err)
		}
		return responseFromConfig(updated, []string{"guild_prompt"}, t.root, t.access, t.models, t.webSearchProviders), nil
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
		return responseFromConfig(updated, []string{"admin_user_ids"}, true, t.access, t.models, t.webSearchProviders), nil
	default:
		return nil, genai.NewExecutionError("unsupported_function", "The requested configuration operation is unavailable.", nil)
	}
}

func configurationPatch(args map[string]any, models *llm.Registry) (config.Patch, []string, error) {
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
		case "primary_model_profile":
			patch.PrimaryModelProfile, err = modelProfileArgument(value, name, false)
		case "fallback_model_profile":
			patch.FallbackModelProfile, err = modelProfileArgument(value, name, true)
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
	if patch.PrimaryModelProfile != nil || patch.FallbackModelProfile != nil {
		if models == nil {
			return config.Patch{}, nil, errors.New("model profile configuration is unavailable")
		}
		patch.ValidateModelProfiles = func(settings config.ServerSettings) error {
			primary, ok := models.Profile(settings.PrimaryModelProfile)
			if !ok {
				return errors.Errorf("unknown primary model profile %q", settings.PrimaryModelProfile)
			}
			if !primary.ToolsEnabled() {
				return errors.Errorf("primary model profile %q must confirm tools and tool choice", settings.PrimaryModelProfile)
			}
			if settings.FallbackModelProfile != "" {
				if _, ok := models.Profile(settings.FallbackModelProfile); !ok {
					return errors.Errorf("unknown fallback model profile %q", settings.FallbackModelProfile)
				}
			}
			return nil
		}
	}
	slices.Sort(changed)
	return patch, changed, nil
}

func responseFromConfig(value config.GuildConfig, changed []string, root bool, access string, models *llm.Registry, webSearchProviders []string) configurationResponse {
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
	response.PrimaryModelProfile = settings.PrimaryModelProfile
	response.FallbackModelProfile = settings.FallbackModelProfile
	response.WebSearchProviders = append([]string{}, webSearchProviders...)
	if models != nil {
		for _, profile := range models.Profiles() {
			response.AvailableProfiles = append(response.AvailableProfiles, configurationProfile{
				Name: profile.Name, Provider: string(profile.Provider),
				Tools: profile.Capabilities.Tools, ToolChoice: profile.Capabilities.ToolChoice,
				Images: profile.Capabilities.Images, Reasoning: profile.Capabilities.Reasoning,
				ReasoningControls: profile.Capabilities.ReasoningControls,
				ContextTokens:     profile.Capabilities.ContextTokens, MaxInputTokens: profile.Capabilities.MaxInputTokens,
				MaxOutputTokens: profile.Capabilities.MaxOutputTokens,
			})
		}
	}
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
	if errors.Is(err, config.ErrInvalidConfiguration) {
		return genai.NewExecutionError("invalid_configuration", "The requested server configuration is invalid.", err)
	}
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

func modelProfileArgument(value any, name string, allowEmpty bool) (*string, error) {
	result, ok := value.(string)
	if !ok {
		return nil, errors.Errorf("%s must be a string", name)
	}
	result = strings.TrimSpace(result)
	if result == "" && !allowEmpty {
		return nil, errors.Errorf("%s must not be empty", name)
	}
	return &result, nil
}

func modelProfileNames(models *llm.Registry) []string {
	profiles := models.Profiles()
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, profile.Name)
	}
	return names
}

func primaryModelProfileNames(models *llm.Registry) []string {
	profiles := models.Profiles()
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile.ToolsEnabled() {
			names = append(names, profile.Name)
		}
	}
	return names
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

func objectSchema(properties map[string]any, required []string) llm.JSONSchema {
	if properties == nil {
		properties = map[string]any{}
	}
	result := llm.JSONSchema{"type": "object", "properties": properties}
	if len(required) > 0 {
		result["required"] = required
	}
	return result
}

func stringSchema(description string) llm.JSONSchema {
	return llm.JSONSchema{"type": "string", "description": description}
}

func enumStringSchema(description string, values []string) llm.JSONSchema {
	return llm.JSONSchema{"type": "string", "description": description, "enum": values}
}

func boundedStringSchema(description string, maximum int) llm.JSONSchema {
	return llm.JSONSchema{"type": "string", "description": description, "maxLength": maximum}
}

func booleanSchema(description string) llm.JSONSchema {
	return llm.JSONSchema{"type": "boolean", "description": description}
}

func integerSchema(description string, minimum, maximum int) llm.JSONSchema {
	return llm.JSONSchema{"type": "integer", "description": description, "minimum": minimum, "maximum": maximum}
}

func integerMinimumSchema(description string, minimum int) llm.JSONSchema {
	return llm.JSONSchema{"type": "integer", "description": description, "minimum": minimum}
}
