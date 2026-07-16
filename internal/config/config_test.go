package config

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validSettings() ServerSettings {
	return ServerSettings{
		ThreadMessages:       1,
		ParentMessages:       1,
		ChannelMessages:      1,
		HistoryRunes:         100,
		MaxOutputTokens:      100,
		MessageTimeout:       time.Second,
		MessageRetentionDays: DefaultMessageRetentionDays,
	}
}

func TestStaticProviderReturnsSettingsForEveryGuild(t *testing.T) {
	want := validSettings()
	provider, err := NewStaticProvider(want)
	require.NoError(t, err)

	for _, guildID := range []string{"guild", ""} {
		got, err := provider.Get(context.Background(), guildID)
		require.NoError(t, err)
		assert.Equal(t, want, got.Settings)
	}
}

func TestPatchAppliesAndValidatesCompleteConfiguration(t *testing.T) {
	value := GuildConfig{Settings: validSettings(), AdminUserIDs: []string{"admin"}}
	retention := 90
	updated, err := (Patch{MessageRetentionDays: &retention}).Apply(value)
	require.NoError(t, err)
	assert.Equal(t, 90, updated.Settings.MessageRetentionDays)
	assert.Equal(t, []string{"admin"}, updated.AdminUserIDs)

	invalid := MaxMessageRetentionDays + 1
	_, err = (Patch{MessageRetentionDays: &invalid}).Apply(value)
	assert.ErrorContains(t, err, "message retention")
}

func TestGuildPromptCompositionAndClearing(t *testing.T) {
	settings := validSettings()
	settings.Prompt = "Base prompt"
	assert.Equal(t, "Base prompt", settings.EffectivePrompt())

	guildPrompt := "  Be concise for this guild.  "
	updated, err := (Patch{GuildPrompt: &guildPrompt}).Apply(GuildConfig{Settings: settings})
	require.NoError(t, err)
	assert.Equal(t, "Be concise for this guild.", updated.Settings.GuildPrompt)
	assert.Equal(t, "Base prompt\n\nGuild-specific instructions:\nBe concise for this guild.", updated.Settings.EffectivePrompt())

	clear := "  "
	cleared, err := (Patch{GuildPrompt: &clear}).Apply(updated)
	require.NoError(t, err)
	assert.Empty(t, cleared.Settings.GuildPrompt)
	assert.Equal(t, "Base prompt", cleared.Settings.EffectivePrompt())
}

func TestGuildPromptRejectsMoreThanMaximumRunes(t *testing.T) {
	settings := validSettings()
	settings.GuildPrompt = strings.Repeat("界", MaxGuildPromptRunes+1)
	assert.ErrorContains(t, settings.Validate(), "guild prompt")
}

func TestStaticProviderRejectsInvalidSettings(t *testing.T) {
	settings := validSettings()
	settings.HistoryRunes = 0
	_, err := NewStaticProvider(settings)
	assert.ErrorContains(t, err, "history rune budget")
}

func TestServerSettingsAllowExpandedOutputTokenBudget(t *testing.T) {
	settings := validSettings()
	settings.MaxOutputTokens = 8192
	assert.NoError(t, settings.Validate())

	settings.MaxOutputTokens = 8193
	assert.ErrorContains(t, settings.Validate(), "between 1 and 8192")
}

func TestStaticProviderHonorsCancellation(t *testing.T) {
	provider, err := NewStaticProvider(validSettings())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = provider.Get(ctx, "guild")
	assert.ErrorIs(t, err, context.Canceled)
}
