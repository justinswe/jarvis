package discord

import (
	"context"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
)

// replyHoldTimeout bounds the extension, which runs after the user already has their
// answer. It is short for the same reason the reaction cleanup is: nothing waits on it.
const (
	replyHoldTimeout          = 5 * time.Second
	defaultClaimRenewInterval = 10 * time.Second
	maximumClaimRenewInterval = 10 * time.Second
)

func claimRenewInterval(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return defaultClaimRenewInterval
	}
	interval := ttl / 3
	if interval > maximumClaimRenewInterval {
		return maximumClaimRenewInterval
	}
	if interval <= 0 {
		return defaultClaimRenewInterval
	}
	return interval
}

// claimReply reports whether this worker should answer the message.
//
// It is checked once targeting has decided the message deserves an answer and before any
// model call, so a worker that loses costs one conditional write rather than a whole
// generation. Both the admin command and the AI reply sit behind it: each one posts to
// Discord, and each must happen exactly once.
//
// A claim it cannot reach rejects the attempt. Posting without ownership can produce
// multiple public replies, so the queue must retry after the shared store recovers.
func (p *Processor) claimReply(ctx context.Context, channel *discordgo.Channel, m *discordgo.MessageCreate) (bool, error) {
	if p.replies == nil {
		return true, nil
	}
	won, err := p.replies.ClaimReply(ctx, m.ChannelID, m.ID)
	if err != nil {
		app.L().Warn("Reply claim unavailable; refusing uncoordinated reply",
			append(discordRequestFields(channel, m), zap.Error(err))...)
		return false, errors.Wrap(err, "claim Discord reply")
	}
	if !won {
		app.L().Debug("Another worker claimed this reply", discordRequestFields(channel, m)...)
	}
	return won, nil
}

func (p *Processor) maintainReplyClaim(ctx context.Context, cancel context.CancelCauseFunc, m *discordgo.MessageCreate) func() {
	if p.replies == nil {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	interval := p.replyClaimRenewInterval
	if interval <= 0 {
		interval = defaultClaimRenewInterval
	}
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if err := p.replies.RenewReply(ctx, m.ChannelID, m.ID); err != nil {
					cancel(errors.Wrap(err, "maintain Discord reply claim"))
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-stopped
	}
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
