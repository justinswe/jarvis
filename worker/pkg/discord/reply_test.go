package discord

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
)

type fakeReplyClaimer struct {
	won      bool
	err      error
	holdErr  error
	channels []string
	messages []string
	held     []string
	renewed  []string
	renew    func(context.Context, string, string) error
}

func (c *fakeReplyClaimer) ClaimReply(_ context.Context, channelID, messageID string) (bool, error) {
	c.channels = append(c.channels, channelID)
	c.messages = append(c.messages, messageID)
	return c.won, c.err
}

func (c *fakeReplyClaimer) RenewReply(ctx context.Context, channelID, messageID string) error {
	if c.renew != nil {
		return c.renew(ctx, channelID, messageID)
	}
	c.renewed = append(c.renewed, messageID)
	return nil
}

func (c *fakeReplyClaimer) HoldReply(_ context.Context, _, messageID string) error {
	c.held = append(c.held, messageID)
	return c.holdErr
}

func replyMessage() *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "123456789012345678", ChannelID: "channel", GuildID: "guild",
		Author: &discordgo.User{ID: "user", Username: "user"},
	}}
}

func TestClaimReplyAdmitsWhenNoStoreIsConfigured(t *testing.T) {
	processor := &Processor{}

	claimed, err := processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage())
	assert.NoError(t, err)
	assert.True(t, claimed,
		"a single-site deployment has nobody to coordinate with")
}

func TestClaimReplyPassesTheDiscordIdentifiers(t *testing.T) {
	claimer := &fakeReplyClaimer{won: true}
	processor := &Processor{replies: claimer}

	claimed, err := processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage())
	assert.NoError(t, err)
	assert.True(t, claimed)
	assert.Equal(t, []string{"channel"}, claimer.channels)
	assert.Equal(t, []string{"123456789012345678"}, claimer.messages)
}

func TestClaimReplyStopsTheLoser(t *testing.T) {
	processor := &Processor{replies: &fakeReplyClaimer{won: false}}

	claimed, err := processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage())
	assert.NoError(t, err)
	assert.False(t, claimed,
		"the other site is already answering this message")
}

func TestClaimReplyFailsClosedWhenTheStoreFails(t *testing.T) {
	processor := &Processor{replies: &fakeReplyClaimer{err: errors.New("throttled")}}

	claimed, err := processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage())
	assert.Error(t, err)
	assert.False(t, claimed, "an uncoordinated reply can duplicate a public answer")
}

func TestMaintainReplyClaimCancelsGenerationWhenRenewalFails(t *testing.T) {
	renewed := make(chan struct{}, 1)
	claimer := &fakeReplyClaimer{renew: func(context.Context, string, string) error {
		renewed <- struct{}{}
		return errors.New("lease lost")
	}}
	processor := &Processor{replies: claimer, replyClaimRenewInterval: time.Millisecond}
	ctx, cancel := context.WithCancelCause(t.Context())
	stop := processor.maintainReplyClaim(ctx, cancel, replyMessage())
	defer stop()

	select {
	case <-renewed:
	case <-time.After(time.Second):
		t.Fatal("reply claim was not renewed")
	}
	<-ctx.Done()
	assert.ErrorContains(t, context.Cause(ctx), "maintain Discord reply claim")
}

func TestClaimRenewIntervalStaysInsideLease(t *testing.T) {
	assert.Equal(t, 2*time.Second, claimRenewInterval(6*time.Second))
	assert.Equal(t, 10*time.Second, claimRenewInterval(time.Minute))
}

// TestHoldReplyExtendsTheAnsweredClaim covers the window the short claim leaves open: a
// copy delivered after the claim would have lapsed must still find the message taken.
func TestHoldReplyExtendsTheAnsweredClaim(t *testing.T) {
	claimer := &fakeReplyClaimer{won: true}
	processor := &Processor{replies: claimer}

	processor.holdReply(t.Context(), &discordgo.Channel{}, replyMessage())

	assert.Equal(t, []string{"123456789012345678"}, claimer.held)
}

// TestHoldReplyOutlivesACancelledRequest is why the extension does not take the caller's
// context: a SIGTERM arriving just after the reply was posted would otherwise leave the
// claim short and let a redelivery answer the same message again.
func TestHoldReplyOutlivesACancelledRequest(t *testing.T) {
	claimer := &fakeReplyClaimer{won: true}
	processor := &Processor{replies: claimer}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	processor.holdReply(ctx, &discordgo.Channel{}, replyMessage())

	assert.Equal(t, []string{"123456789012345678"}, claimer.held)
}

func TestHoldReplyToleratesAnUnavailableStore(t *testing.T) {
	processor := &Processor{replies: &fakeReplyClaimer{holdErr: errors.New("throttled")}}

	assert.NotPanics(t, func() { processor.holdReply(t.Context(), &discordgo.Channel{}, replyMessage()) },
		"the reply is already posted; a failed extension costs at most a duplicate")

	nothing := &Processor{}
	assert.NotPanics(t, func() { nothing.holdReply(t.Context(), &discordgo.Channel{}, replyMessage()) })
}
