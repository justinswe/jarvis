// Package config defines request-scoped Jarvis behavior settings.
package config

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/justinswe/std/errors"
)

const (
	DefaultMessageRetentionDays = 30
	MaxGuildPromptRunes         = 4000
	MaxMessageRetentionDays     = 3650
	maxContextMessages          = 100
	maxOutputTokensLimit        = 8192
)

// ErrConflict indicates that a concurrent guild configuration update won bounded retries.
var ErrConflict = errors.New("guild configuration changed concurrently")

// ServerSettings controls how Jarvis handles a request for one Discord server.
type ServerSettings struct {
	Prompt               string
	GuildPrompt          string
	ThreadMessages       int
	ParentMessages       int
	ChannelMessages      int
	HistoryRunes         int
	MaxOutputTokens      int
	MessageTimeout       time.Duration
	MessageRetentionDays int
	WebSearchEnabled     bool
	ChannelSearchEnabled bool
}

// Validate checks whether the settings are safe to use for message processing.
func (s ServerSettings) Validate() error {
	if len([]rune(strings.TrimSpace(s.GuildPrompt))) > MaxGuildPromptRunes {
		return errors.Errorf("guild prompt must be at most %d runes", MaxGuildPromptRunes)
	}
	if s.ThreadMessages < 1 || s.ThreadMessages > maxContextMessages {
		return errors.Errorf("thread message limit must be between 1 and %d", maxContextMessages)
	}
	if s.ParentMessages < 1 || s.ParentMessages > maxContextMessages {
		return errors.Errorf("parent message limit must be between 1 and %d", maxContextMessages)
	}
	if s.ChannelMessages < 1 || s.ChannelMessages > maxContextMessages {
		return errors.Errorf("channel message limit must be between 1 and %d", maxContextMessages)
	}
	if s.HistoryRunes <= 0 {
		return errors.New("history rune budget must be positive")
	}
	if s.MaxOutputTokens < 1 || s.MaxOutputTokens > maxOutputTokensLimit {
		return errors.Errorf("max output tokens must be between 1 and %d", maxOutputTokensLimit)
	}
	if s.MessageTimeout <= 0 {
		return errors.New("message timeout must be positive")
	}
	if s.MessageRetentionDays < 1 || s.MessageRetentionDays > MaxMessageRetentionDays {
		return errors.Errorf("message retention must be between 1 and %d days", MaxMessageRetentionDays)
	}
	return nil
}

// EffectivePrompt appends the optional guild instructions to the base prompt.
func (s ServerSettings) EffectivePrompt() string {
	guildPrompt := strings.TrimSpace(s.GuildPrompt)
	if guildPrompt == "" {
		return s.Prompt
	}
	return s.Prompt + "\n\nGuild-specific instructions:\n" + guildPrompt
}

// GuildConfig contains behavior settings and delegated administrators for one Discord server.
type GuildConfig struct {
	Settings     ServerSettings
	AdminUserIDs []string
	Version      int64
}

// Validate checks the complete persisted guild configuration.
func (c GuildConfig) Validate() error {
	if err := c.Settings.Validate(); err != nil {
		return err
	}
	for _, userID := range c.AdminUserIDs {
		if strings.TrimSpace(userID) == "" {
			return errors.New("admin user ID must not be empty")
		}
	}
	return nil
}

// IsAdmin reports whether userID is a delegated administrator.
func (c GuildConfig) IsAdmin(userID string) bool {
	return slices.Contains(c.AdminUserIDs, userID)
}

// Patch contains optional server-setting replacements.
type Patch struct {
	Prompt               *string
	GuildPrompt          *string
	ThreadMessages       *int
	ParentMessages       *int
	ChannelMessages      *int
	HistoryRunes         *int
	MaxOutputTokens      *int
	MessageTimeout       *time.Duration
	MessageRetentionDays *int
	WebSearchEnabled     *bool
	ChannelSearchEnabled *bool
}

// Empty reports whether the patch changes no fields.
func (p Patch) Empty() bool {
	return p.Prompt == nil && p.GuildPrompt == nil && p.ThreadMessages == nil && p.ParentMessages == nil && p.ChannelMessages == nil &&
		p.HistoryRunes == nil && p.MaxOutputTokens == nil && p.MessageTimeout == nil &&
		p.MessageRetentionDays == nil && p.WebSearchEnabled == nil && p.ChannelSearchEnabled == nil
}

// Apply applies the patch and validates the resulting configuration.
func (p Patch) Apply(current GuildConfig) (GuildConfig, error) {
	if p.Empty() {
		return GuildConfig{}, errors.New("configuration patch must not be empty")
	}
	settings := current.Settings
	if p.Prompt != nil {
		settings.Prompt = *p.Prompt
	}
	if p.GuildPrompt != nil {
		settings.GuildPrompt = strings.TrimSpace(*p.GuildPrompt)
	}
	if p.ThreadMessages != nil {
		settings.ThreadMessages = *p.ThreadMessages
	}
	if p.ParentMessages != nil {
		settings.ParentMessages = *p.ParentMessages
	}
	if p.ChannelMessages != nil {
		settings.ChannelMessages = *p.ChannelMessages
	}
	if p.HistoryRunes != nil {
		settings.HistoryRunes = *p.HistoryRunes
	}
	if p.MaxOutputTokens != nil {
		settings.MaxOutputTokens = *p.MaxOutputTokens
	}
	if p.MessageTimeout != nil {
		settings.MessageTimeout = *p.MessageTimeout
	}
	if p.MessageRetentionDays != nil {
		settings.MessageRetentionDays = *p.MessageRetentionDays
	}
	if p.WebSearchEnabled != nil {
		settings.WebSearchEnabled = *p.WebSearchEnabled
	}
	if p.ChannelSearchEnabled != nil {
		settings.ChannelSearchEnabled = *p.ChannelSearchEnabled
	}
	current.Settings = settings
	if err := current.Validate(); err != nil {
		return GuildConfig{}, err
	}
	return current, nil
}

// Provider resolves behavior settings and delegated administrators for a Discord server.
type Provider interface {
	Get(context.Context, string) (GuildConfig, error)
}

// Manager reads and changes persisted Discord server configuration.
type Manager interface {
	Load(context.Context, string) (GuildConfig, error)
	Update(context.Context, string, string, Patch) (GuildConfig, error)
	AddAdmin(context.Context, string, string, string) (GuildConfig, error)
	RemoveAdmin(context.Context, string, string, string) (GuildConfig, error)
}

// StaticProvider returns one immutable configuration for every server.
type StaticProvider struct {
	config GuildConfig
}

// NewStaticProvider creates a provider backed by hard-coded settings.
func NewStaticProvider(settings ServerSettings) (*StaticProvider, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	return &StaticProvider{config: GuildConfig{Settings: settings}}, nil
}

// Get returns the configured settings while honoring request cancellation.
func (p *StaticProvider) Get(ctx context.Context, _ string) (GuildConfig, error) {
	select {
	case <-ctx.Done():
		return GuildConfig{}, ctx.Err()
	default:
		return p.config, nil
	}
}
