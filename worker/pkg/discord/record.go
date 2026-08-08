package discord

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

// recordTimeout bounds recording, which runs off the reply's critical path.
const recordTimeout = 5 * time.Second

// record persists messages as bot-involved conversation, expiring after the guild's
// retention. The context is detached: the reply record runs right where a shutdown lands
// after the answer is posted, and losing it would hollow out the thread's stored history.
// A failure costs stored context, never the reply, so it is logged and swallowed.
func (p *Processor) record(ctx context.Context, retentionDays int, messages ...*discordgo.Message) {
	if p.recorder == nil {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	for _, message := range messages {
		if message == nil {
			continue
		}
		if err := p.recorder.Record(recordCtx, message, retentionDays); err != nil {
			app.L().Warn("Recording Discord message failed",
				zap.String("channel_id", message.ChannelID),
				zap.String("message_id", message.ID),
				zap.Error(err))
		}
	}
}
