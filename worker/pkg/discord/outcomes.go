package discord

import (
	"context"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/jarvis/store"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

func (p *Processor) startRequestOutcome(ctx context.Context, m *discordgo.Message, retentionDays int) {
	if p.outcomes == nil || m == nil {
		return
	}
	outcomeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := p.outcomes.StartRequest(outcomeCtx, m.GuildID, m.ChannelID, m.ID, retentionDays); err != nil {
		app.L().Warn("Starting request outcome failed", zap.String("message_id", m.ID), zap.Error(err))
	}
}

func (p *Processor) finishRequestOutcome(ctx context.Context, outcome store.RequestOutcome) {
	if p.outcomes == nil {
		return
	}
	outcomeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := p.outcomes.FinishRequest(outcomeCtx, outcome); err != nil {
		app.L().Warn("Finishing request outcome failed", zap.String("message_id", outcome.MessageID), zap.Error(err))
	}
}

func newRequestOutcome(m *discordgo.Message) store.RequestOutcome {
	return store.RequestOutcome{
		GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID,
		Status: store.RequestStatusFailed,
	}
}

func replyMessageIDs(messages []*discordgo.Message) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		if message != nil && message.ID != "" {
			ids = append(ids, message.ID)
		}
	}
	return ids
}
