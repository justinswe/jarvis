package discord

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

// replyHoldTimeout bounds the extension, which runs after the user already has their
// answer. It is short for the same reason the reaction cleanup is: nothing waits on it.
const replyHoldTimeout = 5 * time.Second

// claimReply reports whether this worker should answer the message.
//
// It is checked once targeting has decided the message deserves an answer and before any
// model call, so a worker that loses costs one conditional write rather than a whole
// generation. Both the admin command and the AI reply sit behind it: each one posts to
// Discord, and each must happen exactly once.
//
// A claim it cannot reach admits the request, matching how every other shared dependency
// on this path degrades. The cost of that choice is a possible duplicate reply while the
// store is unavailable, which is better than answering nobody.
func (p *Processor) claimReply(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate) bool {
	if p.replies == nil {
		return true
	}
	won, err := p.replies.ClaimReply(ctx, m.ChannelID, m.ID)
	if err != nil {
		app.L().Warn("Reply claim unavailable; answering anyway",
			append(discordRequestFields(channel, m), zap.Error(err))...)
		return true
	}
	if !won {
		app.L().Debug("Another worker claimed this reply", discordRequestFields(channel, m)...)
	}
	return won
}

// holdReply extends this worker's claim now that the message has been answered.
//
// The claim taken before generation is short, because a worker that dies mid-generation
// must not lock out its own redelivery. That leaves a duplicate arriving late enough —
// held behind a full in-flight budget at the other site, or redelivered after a transient
// failure — free to claim a message that has already been answered. Extending closes that
// window, and only ever runs on a path that produced a reply.
//
// A failure to extend is logged and swallowed: the reply is already posted, so the worst
// case is the duplicate that would have happened anyway.
func (p *Processor) holdReply(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate) {
	if p.replies == nil {
		return
	}
	// The caller's context may already be cancelled by the shutdown that followed the
	// reply; the claim still has to outlive it.
	holdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replyHoldTimeout)
	defer cancel()
	if err := p.replies.HoldReply(holdCtx, m.ChannelID, m.ID); err != nil {
		app.L().Warn("Extending the reply claim failed; a late duplicate may answer again",
			append(discordRequestFields(channel, m), zap.Error(err))...)
	}
}
