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
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

// Processor handles a normalized Discord message request.
type Processor interface {
	Process(context.Context, *discordgo.MessageCreate) error
}

// Recorder persists one validated normalized Discord message before processing.
type Recorder interface {
	Record(context.Context, *discordv1.DiscordMessageCreateEvent) error
}

// Start subscribes to the configured broker and processes messages until Stop is called.
func Start(ctx context.Context, cfg mq.Config, processor Processor, recorder Recorder) (mq.Subscription, error) {
	if processor == nil {
		return nil, errors.New("message processor is required")
	}
	return mq.Subscribe(ctx, cfg, func(msgCtx context.Context, msg mq.Message) {
		handle(msgCtx, msg, processor, recorder)
	})
}

// handle processes one message, holding it un-acknowledged until it completes.
func handle(ctx context.Context, msg mq.Message, processor Processor, recorder Recorder) {
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
	if recorder != nil {
		if err := recorder.Record(ctx, request.Event); err != nil {
			app.L().Warn("Discord message recording failed", append(fields, zap.Error(err))...)
		}
	}
	if processErr := processor.Process(ctx, discordMsg); processErr != nil {
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

// terminate drops a message that redelivery could never make succeed.
func terminate(msg mq.Message, reason string, cause error) {
	app.L().Warn("Dropping unprocessable message", zap.String("reason", reason), zap.Error(cause))
	if err := msg.Term(); err != nil {
		app.L().Warn("Message termination failed", zap.Error(err))
	}
}
