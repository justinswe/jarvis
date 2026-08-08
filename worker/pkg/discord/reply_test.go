package discord

import (
	"context"
	"testing"

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
}

func (c *fakeReplyClaimer) ClaimReply(_ context.Context, channelID, messageID string) (bool, error) {
	c.channels = append(c.channels, channelID)
	c.messages = append(c.messages, messageID)
	return c.won, c.err
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

	assert.True(t, processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage()),
		"a single-site deployment has nobody to coordinate with")
}

func TestClaimReplyPassesTheDiscordIdentifiers(t *testing.T) {
	claimer := &fakeReplyClaimer{won: true}
	processor := &Processor{replies: claimer}

	assert.True(t, processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage()))
	assert.Equal(t, []string{"channel"}, claimer.channels)
	assert.Equal(t, []string{"123456789012345678"}, claimer.messages)
}

func TestClaimReplyStopsTheLoser(t *testing.T) {
	processor := &Processor{replies: &fakeReplyClaimer{won: false}}

	assert.False(t, processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage()),
		"the other site is already answering this message")
}

func TestClaimReplyAdmitsWhenTheStoreFails(t *testing.T) {
	processor := &Processor{replies: &fakeReplyClaimer{err: errors.New("throttled")}}

	assert.True(t, processor.claimReply(t.Context(), &discordgo.Channel{}, replyMessage()),
		"a duplicate reply beats no reply at all")
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
