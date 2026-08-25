package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/justinswe/std/errors"
)

// Request outcome states.
const (
	RequestStatusProcessing  = "processing"
	RequestStatusAnswered    = "answered"
	RequestStatusRefused     = "refused"
	RequestStatusRateLimited = "rate_limited"
	RequestStatusFailed      = "failed"
)

// RequestOutcome is the terminal operational state of one targeted Discord message.
type RequestOutcome struct {
	GuildID         string
	ChannelID       string
	MessageID       string
	Status          string
	ReplyMessageIDs []string
	ErrorKind       string
	Duration        time.Duration
}

// StartRequest creates the processing record before admission or generation.
func (s *Store) StartRequest(ctx context.Context, guildID, channelID, messageID string, retentionDays int) error {
	gid, err := optionalSnowflake(guildID)
	if err != nil {
		return err
	}
	cid, mid, err := claimKey(channelID, messageID)
	if err != nil {
		return err
	}
	if retentionDays <= 0 {
		retentionDays = s.defaults.Settings.MessageRetentionDays
	}
	retentionSeconds := int64(time.Duration(retentionDays) * 24 * time.Hour / time.Second)
	_, err = s.db.ExecContext(ctx, s.q(`
		INSERT INTO request_outcomes (channel_id, message_id, guild_id, status,
			created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, @now, @now, @now + ?)
		ON CONFLICT (channel_id, message_id) DO NOTHING`),
		cid, mid, gid, RequestStatusProcessing, retentionSeconds)
	return errors.Wrap(err, "start request outcome")
}

// FinishRequest writes one terminal state without persisting prompt or response text.
func (s *Store) FinishRequest(ctx context.Context, outcome RequestOutcome) error {
	cid, mid, err := claimKey(outcome.ChannelID, outcome.MessageID)
	if err != nil {
		return err
	}
	replyIDs, err := outcomeReplyIDs(outcome.ReplyMessageIDs)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, s.q(`
		UPDATE request_outcomes SET status = ?, reply_message_ids = ?, error_kind = ?,
			duration_ms = ?, updated_at = @now
		WHERE channel_id = ? AND message_id = ?`),
		outcome.Status, replyIDs, outcome.ErrorKind, outcome.Duration.Milliseconds(), cid, mid)
	if err != nil {
		return errors.Wrap(err, "finish request outcome")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "confirm request outcome")
	}
	if affected == 0 {
		return errors.New("request outcome not found")
	}
	return nil
}

func outcomeReplyIDs(ids []string) (string, error) {
	validated := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := snowflake(id); err != nil {
			return "", errors.Wrap(err, "reply message ID")
		}
		validated = append(validated, id)
	}
	encoded, err := json.Marshal(validated)
	return string(encoded), errors.Wrap(err, "encode reply message IDs")
}
