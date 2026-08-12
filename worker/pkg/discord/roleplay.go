package discord

import (
	"context"
	"regexp"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/jarvis/worker/pkg/character"
	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

// oocCommand marks an out-of-character message: it runs in assistant mode with full
// tools even inside a roleplay channel, which is how characters and memory are managed
// there. Matches the SillyTavern user convention.
var oocCommand = regexp.MustCompile(`(?i)^\s*(?:\(\s*ooc\s*\)|ooc\b[:,]?|//)`)

// activeCharacter resolves the channel's bound character, or nil for assistant mode.
func (p *Processor) activeCharacter(ctx context.Context, m *discordgo.MessageCreate) *store.Character {
	if p.characters == nil || m.GuildID == "" {
		return nil
	}
	active, err := p.characters.ChannelCharacter(ctx, m.ChannelID)
	if err != nil {
		app.L().Warn("Channel character lookup failed; using assistant mode",
			zap.String("channel_id", m.ChannelID), zap.Error(err))
		return nil
	}
	return active
}

// activeRoleplay resolves the channel's bound character into a roleplay configuration,
// or nil for assistant mode.
func (p *Processor) activeRoleplay(ctx context.Context, m *discordgo.MessageCreate) *genai.RoleplayConfig {
	if oocCommand.MatchString(sanitizeContent(m.Content, p.botID)) {
		return nil
	}
	return roleplayConfigFrom(p.activeCharacter(ctx, m), m)
}

// roleplayConfigFrom macro-substitutes the card so the generation layer receives
// finished prose. A nil or unparseable character means assistant mode.
func roleplayConfigFrom(active *store.Character, m *discordgo.MessageCreate) *genai.RoleplayConfig {
	if active == nil {
		return nil
	}
	card, err := character.Parse([]byte(active.CardJSON))
	if err != nil {
		app.L().Warn("Stored character card failed to parse; using assistant mode",
			zap.String("channel_id", m.ChannelID), zap.String("character", active.Name), zap.Error(err))
		return nil
	}
	user := displayName(m.Author)
	sub := func(text string) string { return character.ReplaceMacros(text, card.Data.Name, user) }
	return &genai.RoleplayConfig{
		CharacterName:           card.Data.Name,
		Description:             sub(card.Data.Description),
		Personality:             sub(card.Data.Personality),
		Scenario:                sub(card.Data.Scenario),
		ExampleDialogue:         sub(card.Data.MesExample),
		CardSystemPrompt:        sub(card.Data.SystemPrompt),
		PostHistoryInstructions: sub(card.Data.PostHistoryInstructions),
		UserName:                user,
	}
}

// hasActiveCharacter reports whether the channel has a bound character, which makes
// every channel message targeted: a companion answers the room, not only mentions.
func (p *Processor) hasActiveCharacter(ctx context.Context, channelID string) bool {
	if p.characters == nil {
		return false
	}
	active, err := p.characters.ChannelCharacter(ctx, channelID)
	return err == nil && active != nil
}

// memoryScope is the POV owner of the channel's memory: the bound character's ID, or
// zero for the assistant persona.
func memoryScope(active *store.Character) int64 {
	if active == nil {
		return 0
	}
	return active.ID
}
