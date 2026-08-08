package store

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/justinswe/std/errors"
)

// Message kinds, numerically identical to discordv1.MessageKind in
// api/jarvis/discord/v1/worker.proto. Restated here so the store does not depend on the
// wire contract to describe a stored row.
const (
	messageKindUnspecified = 0
	messageKindDefault     = 1
	messageKindReply       = 2
)

// Record persists one Discord message for history and search. Duplicate delivery of the
// same channel/message pair is idempotent.
func (s *Store) Record(ctx context.Context, m *discordgo.Message, retentionDays int) error {
	if m == nil || m.Author == nil {
		return errors.New("message and author are required")
	}
	cid, err := snowflake(m.ChannelID)
	if err != nil {
		return err
	}
	mid, err := snowflake(m.ID)
	if err != nil {
		return err
	}
	aid, err := snowflake(m.Author.ID)
	if err != nil {
		return err
	}
	gid, err := optionalSnowflake(m.GuildID)
	if err != nil {
		return err
	}
	refChannel, refMessage, err := reference(m.MessageReference)
	if err != nil {
		return err
	}
	mentions, err := mentionedIDs(m.Mentions)
	if err != nil {
		return err
	}
	if retentionDays <= 0 {
		retentionDays = s.defaults.Settings.MessageRetentionDays
	}
	ingested := s.now().UTC()
	created := ingested
	if !m.Timestamp.IsZero() {
		created = m.Timestamp.UTC()
	} else if timestamp, err := discordgo.SnowflakeTimestamp(m.ID); err == nil {
		created = timestamp.UTC()
	}
	_, err = s.db.ExecContext(ctx, s.q(`
		INSERT INTO messages (channel_id, message_id, guild_id, author_id, author_username,
			author_global_name, author_bot, message_kind, content, mentioned_user_ids,
			reference_channel_id, reference_message_id, created_at, ingested_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (channel_id, message_id) DO NOTHING`),
		cid, mid, gid, aid, m.Author.Username, m.Author.GlobalName, boolInt(m.Author.Bot),
		messageKind(m.Type), m.Content, mentions, refChannel, refMessage,
		created.Unix(), ingested.Unix(),
		ingested.Add(time.Duration(retentionDays)*24*time.Hour).Unix(),
	)
	return errors.Wrap(err, "write Discord message")
}

// Messages returns newest-first stored messages before the supplied Discord message ID.
func (s *Store) Messages(ctx context.Context, guildID, channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
	if channelID == "" {
		return nil, errors.New("channel ID is required")
	}
	if limit <= 0 {
		return nil, errors.New("message limit must be positive")
	}
	cid, err := snowflake(channelID)
	if err != nil {
		return nil, err
	}
	before := int64(math.MaxInt64)
	if beforeID != "" {
		if before, err = snowflake(beforeID); err != nil {
			return nil, err
		}
	}
	query := `
		SELECT message_id, guild_id, author_id, author_username, author_global_name,
			author_bot, message_kind, content, mentioned_user_ids,
			reference_channel_id, reference_message_id, created_at
		FROM messages WHERE channel_id = ? AND message_id < ? AND expires_at > @now`
	args := []any{cid, before}
	if guildID != "" {
		gid, err := snowflake(guildID)
		if err != nil {
			return nil, err
		}
		query += ` AND guild_id = ?`
		args = append(args, gid)
	}
	query += ` ORDER BY message_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, errors.Wrap(err, "query Discord message history")
	}
	defer rows.Close()
	var messages []*discordgo.Message
	for rows.Next() {
		message, err := scanMessage(rows, channelID)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, errors.Wrap(rows.Err(), "read Discord message history")
}

func scanMessage(rows interface{ Scan(...any) error }, channelID string) (*discordgo.Message, error) {
	var (
		mid, gid, aid, refChannel, refMessage, createdAt int64
		username, globalName, content, mentionsJSON      string
		bot, kind                                        int
	)
	if err := rows.Scan(&mid, &gid, &aid, &username, &globalName, &bot, &kind,
		&content, &mentionsJSON, &refChannel, &refMessage, &createdAt); err != nil {
		return nil, errors.Wrap(err, "decode Discord message row")
	}
	message := &discordgo.Message{
		ID:        strconv.FormatInt(mid, 10),
		GuildID:   formatOptionalID(gid),
		ChannelID: channelID,
		Content:   content,
		Type:      messageType(kind),
		Timestamp: time.Unix(createdAt, 0).UTC(),
		Author: &discordgo.User{
			ID: strconv.FormatInt(aid, 10), Username: username, GlobalName: globalName, Bot: bot != 0,
		},
	}
	var mentions []string
	if err := json.Unmarshal([]byte(mentionsJSON), &mentions); err != nil {
		return nil, errors.Wrap(err, "decode mentioned user IDs")
	}
	for _, userID := range mentions {
		message.Mentions = append(message.Mentions, &discordgo.User{ID: userID})
	}
	if refChannel != 0 || refMessage != 0 {
		message.MessageReference = &discordgo.MessageReference{
			MessageID: formatOptionalID(refMessage),
			ChannelID: formatOptionalID(refChannel),
			GuildID:   message.GuildID,
		}
	}
	return message, nil
}

// reference flattens an optional reply reference to the schema's zero-means-absent pair.
func reference(ref *discordgo.MessageReference) (channelID, messageID int64, err error) {
	if ref == nil {
		return 0, 0, nil
	}
	if channelID, err = optionalSnowflake(ref.ChannelID); err != nil {
		return 0, 0, err
	}
	if messageID, err = optionalSnowflake(ref.MessageID); err != nil {
		return 0, 0, err
	}
	return channelID, messageID, nil
}

// mentionedIDs renders mentioned user IDs as the JSON array the schema stores. The list
// is only ever read back whole, never queried, which is why it is not a table.
func mentionedIDs(mentions []*discordgo.User) (string, error) {
	ids := make([]string, 0, len(mentions))
	for _, user := range mentions {
		if user != nil && user.ID != "" {
			ids = append(ids, user.ID)
		}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", errors.Wrap(err, "encode mentioned user IDs")
	}
	return string(encoded), nil
}

func messageKind(messageType discordgo.MessageType) int {
	switch messageType {
	case discordgo.MessageTypeDefault:
		return messageKindDefault
	case discordgo.MessageTypeReply:
		return messageKindReply
	default:
		return messageKindUnspecified
	}
}

// messageType maps a stored kind back to a discordgo type; unknown kinds map to a type
// the worker acts on for neither history nor targeting.
func messageType(kind int) discordgo.MessageType {
	switch kind {
	case messageKindDefault:
		return discordgo.MessageTypeDefault
	case messageKindReply:
		return discordgo.MessageTypeReply
	default:
		return discordgo.MessageType(-1)
	}
}

func formatOptionalID(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}
