// Package consumer processes normalized Discord messages delivered by the message queue.
//
// A message stays un-acknowledged for as long as the worker is working on it, so a worker
// that crashes mid-message has that message redelivered rather than losing it. Messages
// that can never succeed are terminated instead of retried.
package consumer

import (
	"context"

	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/mq"
	"github.com/justinswe/jarvis/worker/pkg/llm"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// Processor handles a normalized Discord message request.
//
// Recording messages for history is the processor's concern, not this package's: only the
// processor knows whether a message is targeted at the bot, and only targeted
// conversation is stored.
type Processor interface {
	Process(context.Context, *discordgo.MessageCreate) error
}

// Start subscribes to the configured broker and processes messages until Stop is called.
func Start(ctx context.Context, cfg mq.Config, processor Processor) (mq.Subscription, error) {
	if processor == nil {
		return nil, errors.New("message processor is required")
	}
	return mq.Subscribe(ctx, cfg, func(msgCtx context.Context, msg mq.Message) {
		handle(msgCtx, msg, processor)
	})
}

// handle processes one message, holding it un-acknowledged until it completes.
func handle(ctx context.Context, msg mq.Message, processor Processor) {
	request := &discordv1.IngestMessageRequest{}
	if err := proto.Unmarshal(msg.Data(), request); err != nil {
		terminate(msg, "invalid protobuf message", err)
		return
	}
	discordMsg, err := discordMessage(request)
	if err != nil {
		terminate(msg, "invalid Discord message", err)
		return
	}

	fields := []zap.Field{
		zap.String("guild_id", discordMsg.GuildID),
		zap.String("channel_id", discordMsg.ChannelID),
		zap.String("message_id", discordMsg.ID),
	}
	if processErr := processor.Process(ctx, discordMsg); processErr != nil {
		// Only a request nothing will retry has actually cost the user an answer, so
		// that is the one worth an alert. A redelivery usually succeeds, and logging it
		// at the same level would bury the failures that do not.
		if !redeliverable(ctx, processErr) {
			app.L().Error("Discord message processing failed", append(fields, zap.Error(processErr))...)
			terminate(msg, "model rejected the request", processErr)
			return
		}
		app.L().Warn("Discord message processing failed", append(fields, zap.Error(processErr))...)
		if err := msg.Nak(); err != nil {
			app.L().Warn("Message negative acknowledgement failed", append(fields, zap.Error(err))...)
		}
		return
	}
	if err := msg.Ack(); err != nil {
		app.L().Warn("Message acknowledgement failed", append(fields, zap.Error(err))...)
	}
}

// redeliverable reports whether another delivery could plausibly succeed.
//
// A model failure already carries that judgement, so reuse it rather than retrying
// everything: replaying a whole request is expensive, and the failures worth replaying
// are exactly the transient ones. Anything the model classified as permanent — an
// unsupported input, an invalid request — fails identically every time, and retrying it
// only spends the budget again. Errors from outside the model layer stay retryable,
// since they are usually infrastructure and usually transient.
func redeliverable(ctx context.Context, err error) bool {
	// A worker that is draining learned nothing about the request itself, and every
	// in-flight message fails at once when it stops. Those must reach a surviving
	// worker, or a deploy would silently drop whatever was being answered.
	if ctx.Err() != nil {
		return true
	}
	var modelErr *llm.Error
	if !errors.As(err, &modelErr) {
		return true
	}
	return modelErr.Retryable()
}

// terminate drops a message that redelivery could never make succeed.
func terminate(msg mq.Message, reason string, cause error) {
	app.L().Warn("Dropping unprocessable message", zap.String("reason", reason), zap.Error(cause))
	if err := msg.Term(); err != nil {
		app.L().Warn("Message termination failed", zap.Error(err))
	}
}
