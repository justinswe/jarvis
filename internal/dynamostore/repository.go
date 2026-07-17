// Package dynamostore persists Discord messages and server configuration in DynamoDB.
package dynamostore

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/bwmarrin/discordgo"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/std/app"
	"github.com/justinswe/std/errors"
	"github.com/klauspost/compress/zstd"
	"go.uber.org/zap"
)

const (
	configSortKey               = "CONFIG"
	configWriteAttempts         = 3
	decodedMessageLimit         = 1 << 20
	guildConfigSchemaVersion    = 1
	legacyMessageSchemaVersion  = 1
	messageCompressionThreshold = 100
	messageKeyWidth             = 20
	messageSchemaVersion        = 2
)

type dynamoClient interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// Repository implements message recording, message history, and mutable guild configuration.
type Repository struct {
	client   dynamoClient
	table    string
	defaults config.GuildConfig
	encoder  *zstd.Encoder
	decoder  *zstd.Decoder
	now      func() time.Time
}

// New creates a DynamoDB repository using an externally provisioned table.
func New(client dynamoClient, table string, defaults config.GuildConfig) (*Repository, error) {
	if client == nil {
		return nil, errors.New("DynamoDB client is required")
	}
	if strings.TrimSpace(table) == "" {
		return nil, errors.New("DynamoDB table is required")
	}
	if err := defaults.Validate(); err != nil {
		return nil, errors.Wrap(err, "validate default guild configuration")
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, errors.Wrap(err, "create zstd encoder")
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(decodedMessageLimit))
	if err != nil {
		encoder.Close()
		return nil, errors.Wrap(err, "create zstd decoder")
	}
	return &Repository{
		client: client, table: table, defaults: cloneConfig(defaults), encoder: encoder, decoder: decoder, now: time.Now,
	}, nil
}

// Close releases compression resources.
func (r *Repository) Close() error {
	r.encoder.Close()
	r.decoder.Close()
	return nil
}

// Get loads guild configuration and falls back to hard-coded defaults on DynamoDB failures.
func (r *Repository) Get(ctx context.Context, guildID string) (config.GuildConfig, error) {
	loaded, err := r.Load(ctx, guildID)
	if err == nil {
		return loaded, nil
	}
	if ctx.Err() != nil {
		return config.GuildConfig{}, ctx.Err()
	}
	app.L().Warn("DynamoDB guild configuration lookup failed; using defaults",
		zap.String("guild_id", guildID), zap.Error(err))
	return cloneConfig(r.defaults), nil
}

// Load strictly loads one guild configuration for administrative operations.
func (r *Repository) Load(ctx context.Context, guildID string) (config.GuildConfig, error) {
	if guildID == "" {
		return cloneConfig(r.defaults), nil
	}
	output, err := r.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &r.table,
		ConsistentRead: boolPointer(true),
		Key: map[string]dynamodbtypes.AttributeValue{
			"pk": &dynamodbtypes.AttributeValueMemberS{Value: guildPartitionKey(guildID)},
			"sk": &dynamodbtypes.AttributeValueMemberS{Value: configSortKey},
		},
	})
	if err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "get guild configuration")
	}
	if len(output.Item) == 0 {
		return cloneConfig(r.defaults), nil
	}
	var item guildConfigItem
	if err := attributevalue.UnmarshalMap(output.Item, &item); err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "decode guild configuration")
	}
	loaded := item.config()
	if err := loaded.Validate(); err != nil {
		return config.GuildConfig{}, errors.Wrap(err, "validate stored guild configuration")
	}
	return loaded, nil
}

// Update atomically applies a validated settings patch.
func (r *Repository) Update(ctx context.Context, guildID, actorID string, patch config.Patch) (config.GuildConfig, error) {
	return r.mutateConfig(ctx, guildID, actorID, func(current config.GuildConfig) (config.GuildConfig, error) {
		return patch.Apply(current)
	})
}

// AddAdmin atomically adds a delegated guild administrator.
func (r *Repository) AddAdmin(ctx context.Context, guildID, actorID, userID string) (config.GuildConfig, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return config.GuildConfig{}, errors.New("admin user ID is required")
	}
	return r.mutateConfig(ctx, guildID, actorID, func(current config.GuildConfig) (config.GuildConfig, error) {
		if !slices.Contains(current.AdminUserIDs, userID) {
			current.AdminUserIDs = append(current.AdminUserIDs, userID)
		}
		return current, nil
	})
}

// RemoveAdmin atomically removes a delegated guild administrator.
func (r *Repository) RemoveAdmin(ctx context.Context, guildID, actorID, userID string) (config.GuildConfig, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return config.GuildConfig{}, errors.New("admin user ID is required")
	}
	return r.mutateConfig(ctx, guildID, actorID, func(current config.GuildConfig) (config.GuildConfig, error) {
		current.AdminUserIDs = slices.DeleteFunc(current.AdminUserIDs, func(candidate string) bool { return candidate == userID })
		return current, nil
	})
}

func (r *Repository) mutateConfig(ctx context.Context, guildID, actorID string, mutate func(config.GuildConfig) (config.GuildConfig, error)) (config.GuildConfig, error) {
	if strings.TrimSpace(guildID) == "" {
		return config.GuildConfig{}, errors.New("guild ID is required")
	}
	if strings.TrimSpace(actorID) == "" {
		return config.GuildConfig{}, errors.New("configuration actor ID is required")
	}
	for attempt := 0; attempt < configWriteAttempts; attempt++ {
		current, err := r.Load(ctx, guildID)
		if err != nil {
			return config.GuildConfig{}, err
		}
		updated, err := mutate(cloneConfig(current))
		if err != nil {
			return config.GuildConfig{}, err
		}
		updated.AdminUserIDs = normalizedUserIDs(updated.AdminUserIDs)
		if err := updated.Validate(); err != nil {
			return config.GuildConfig{}, err
		}
		if equalConfig(current, updated) {
			return current, nil
		}
		updated.Version = current.Version + 1
		item := newGuildConfigItem(guildID, actorID, updated, r.now())
		attributes, err := attributevalue.MarshalMap(item)
		if err != nil {
			return config.GuildConfig{}, errors.Wrap(err, "encode guild configuration")
		}
		input := &dynamodb.PutItemInput{TableName: &r.table, Item: attributes}
		if current.Version == 0 {
			condition := "attribute_not_exists(pk)"
			input.ConditionExpression = &condition
		} else {
			condition := "#version = :version"
			input.ConditionExpression = &condition
			input.ExpressionAttributeNames = map[string]string{"#version": "version"}
			input.ExpressionAttributeValues = map[string]dynamodbtypes.AttributeValue{
				":version": &dynamodbtypes.AttributeValueMemberN{Value: int64String(current.Version)},
			}
		}
		if _, err := r.client.PutItem(ctx, input); err != nil {
			var conflict *dynamodbtypes.ConditionalCheckFailedException
			if errors.As(err, &conflict) {
				continue
			}
			return config.GuildConfig{}, errors.Wrap(err, "write guild configuration")
		}
		return updated, nil
	}
	return config.GuildConfig{}, config.ErrConflict
}

// Record persists one normalized Discord MessageCreate event.
func (r *Repository) Record(ctx context.Context, event *discordv1.DiscordMessageCreateEvent) error {
	if event == nil {
		return errors.New("message event is required")
	}
	if event.MessageId == "" || event.ChannelId == "" || event.Author == nil || event.Author.Id == "" {
		return errors.New("message event is incomplete")
	}
	retention := r.defaults.Settings.MessageRetentionDays
	if event.GuildId != "" {
		guildConfig, err := r.Load(ctx, event.GuildId)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			app.L().Warn("DynamoDB retention lookup failed; using default",
				zap.String("guild_id", event.GuildId), zap.Error(err))
		} else {
			retention = guildConfig.Settings.MessageRetentionDays
		}
	}
	item, content := r.messageItem(event, retention)
	attributes, err := attributevalue.MarshalMap(item)
	if err != nil {
		return errors.Wrap(err, "encode Discord message")
	}
	attributes["content"] = content
	condition := "attribute_not_exists(pk)"
	_, err = r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &r.table, Item: attributes, ConditionExpression: &condition,
	})
	if err != nil {
		var duplicate *dynamodbtypes.ConditionalCheckFailedException
		if errors.As(err, &duplicate) {
			return nil
		}
		return errors.Wrap(err, "write Discord message")
	}
	return nil
}

// Messages returns newest-first stored messages before the supplied Discord message ID.
func (r *Repository) Messages(ctx context.Context, guildID, channelID string, limit int, beforeID string) ([]*discordgo.Message, error) {
	if channelID == "" {
		return nil, errors.New("channel ID is required")
	}
	if limit <= 0 {
		return nil, errors.New("message limit must be positive")
	}
	beforeKey := "MESSAGE#~~~~~~~~~~~~~~~~~~~~"
	if beforeID != "" {
		beforeKey = messageSortKey(beforeID)
	}
	values := map[string]dynamodbtypes.AttributeValue{
		":pk":      &dynamodbtypes.AttributeValueMemberS{Value: channelPartitionKey(channelID)},
		":before":  &dynamodbtypes.AttributeValueMemberS{Value: beforeKey},
		":expires": &dynamodbtypes.AttributeValueMemberN{Value: int64String(r.now().Unix())},
	}
	var messages []*discordgo.Message
	var startKey map[string]dynamodbtypes.AttributeValue
	var partialErr error
	for len(messages) < limit {
		pageLimit := int32(min(100, limit-len(messages)))
		keyCondition := "pk = :pk AND sk < :before"
		filter := "attribute_not_exists(expires_at) OR expires_at > :expires"
		output, err := r.client.Query(ctx, &dynamodb.QueryInput{
			TableName:                 &r.table,
			ConsistentRead:            boolPointer(true),
			ExclusiveStartKey:         startKey,
			ExpressionAttributeValues: values,
			FilterExpression:          &filter,
			KeyConditionExpression:    &keyCondition,
			Limit:                     &pageLimit,
			ScanIndexForward:          boolPointer(false),
		})
		if err != nil {
			return messages, errors.Wrap(err, "query Discord message history")
		}
		for _, attributes := range output.Items {
			message, err := r.decodeMessage(attributes)
			if err != nil {
				if partialErr == nil {
					partialErr = err
				}
				continue
			}
			if guildID == "" || message.GuildID == guildID {
				messages = append(messages, message)
			}
			if len(messages) == limit {
				break
			}
		}
		if len(output.LastEvaluatedKey) == 0 {
			break
		}
		startKey = output.LastEvaluatedKey
	}
	return messages, partialErr
}

func (r *Repository) messageItem(event *discordv1.DiscordMessageCreateEvent, retentionDays int) (messageItem, dynamodbtypes.AttributeValue) {
	ingestedAt := r.now().UTC()
	if event.IngestedAt != nil && event.IngestedAt.IsValid() {
		ingestedAt = event.IngestedAt.AsTime().UTC()
	}
	createdAt := ingestedAt
	if timestamp, err := discordgo.SnowflakeTimestamp(event.MessageId); err == nil {
		createdAt = timestamp.UTC()
	}
	content, compressed := r.encodeContent(event.Content)
	item := messageItem{
		PK: channelPartitionKey(event.ChannelId), SK: messageSortKey(event.MessageId), EntityType: "MESSAGE", SchemaVersion: messageSchemaVersion,
		MessageID: event.MessageId, GuildID: event.GuildId, ChannelID: event.ChannelId, Compressed: compressed,
		MessageKind: int32(event.Kind), AuthorID: event.Author.Id, AuthorUsername: event.Author.Username,
		AuthorGlobalName: event.Author.GlobalName, AuthorBot: event.Author.Bot, MentionedUserIDs: event.MentionedUserIds,
		CreatedAtMillis: createdAt.UnixMilli(), IngestedAtMillis: ingestedAt.UnixMilli(),
		ExpiresAt: ingestedAt.Add(time.Duration(retentionDays) * 24 * time.Hour).Unix(),
	}
	if event.Reference != nil {
		item.ReferenceMessageID = event.Reference.MessageId
		item.ReferenceChannelID = event.Reference.ChannelId
	}
	return item, content
}

func (r *Repository) encodeContent(content string) (dynamodbtypes.AttributeValue, bool) {
	raw := []byte(content)
	if len(raw) > messageCompressionThreshold {
		compressed := r.encoder.EncodeAll(raw, make([]byte, 0, len(raw)))
		if len(compressed) < len(raw) {
			return &dynamodbtypes.AttributeValueMemberB{Value: compressed}, true
		}
	}
	return &dynamodbtypes.AttributeValueMemberS{Value: content}, false
}

func (r *Repository) decodeMessage(attributes map[string]dynamodbtypes.AttributeValue) (*discordgo.Message, error) {
	var item messageItem
	if err := attributevalue.UnmarshalMap(attributes, &item); err != nil {
		return nil, errors.Wrap(err, "decode Discord message item")
	}
	content, err := storedContent(item, attributes["content"])
	if err != nil {
		return nil, err
	}
	if item.Compressed {
		decoded, err := r.decoder.DecodeAll(content, nil)
		if err != nil {
			return nil, errors.Wrap(err, "decompress Discord message content")
		}
		content = decoded
	}
	if len(content) > decodedMessageLimit {
		return nil, errors.New("decoded Discord message exceeds size limit")
	}
	if !utf8.Valid(content) {
		return nil, errors.New("Discord message content is not valid UTF-8")
	}
	messageType := discordgo.MessageType(-1)
	switch discordv1.MessageKind(item.MessageKind) {
	case discordv1.MessageKind_MESSAGE_KIND_DEFAULT:
		messageType = discordgo.MessageTypeDefault
	case discordv1.MessageKind_MESSAGE_KIND_REPLY:
		messageType = discordgo.MessageTypeReply
	}
	message := &discordgo.Message{
		ID: item.MessageID, GuildID: item.GuildID, ChannelID: item.ChannelID, Content: string(content), Type: messageType,
		Timestamp: time.UnixMilli(item.CreatedAtMillis).UTC(),
		Author:    &discordgo.User{ID: item.AuthorID, Username: item.AuthorUsername, GlobalName: item.AuthorGlobalName, Bot: item.AuthorBot},
	}
	for _, userID := range item.MentionedUserIDs {
		message.Mentions = append(message.Mentions, &discordgo.User{ID: userID})
	}
	if item.ReferenceMessageID != "" || item.ReferenceChannelID != "" {
		message.MessageReference = &discordgo.MessageReference{
			MessageID: item.ReferenceMessageID, ChannelID: item.ReferenceChannelID, GuildID: item.GuildID,
		}
	}
	return message, nil
}

func storedContent(item messageItem, attribute dynamodbtypes.AttributeValue) ([]byte, error) {
	switch item.SchemaVersion {
	case legacyMessageSchemaVersion:
		binary, ok := attribute.(*dynamodbtypes.AttributeValueMemberB)
		if !ok {
			return nil, errors.New("legacy Discord message content must be binary")
		}
		return binary.Value, nil
	case messageSchemaVersion:
		if item.Compressed {
			binary, ok := attribute.(*dynamodbtypes.AttributeValueMemberB)
			if !ok {
				return nil, errors.New("compressed Discord message content must be binary")
			}
			return binary.Value, nil
		}
		text, ok := attribute.(*dynamodbtypes.AttributeValueMemberS)
		if !ok {
			return nil, errors.New("uncompressed Discord message content must be a string")
		}
		return []byte(text.Value), nil
	default:
		return nil, errors.Errorf("unsupported Discord message schema version %d", item.SchemaVersion)
	}
}

type guildConfigItem struct {
	PK                    string   `dynamodbav:"pk"`
	SK                    string   `dynamodbav:"sk"`
	EntityType            string   `dynamodbav:"entity_type"`
	SchemaVersion         int      `dynamodbav:"schema_version"`
	Prompt                string   `dynamodbav:"prompt"`
	GuildPrompt           string   `dynamodbav:"guild_prompt,omitempty"`
	ThreadMessages        int      `dynamodbav:"thread_messages"`
	ParentMessages        int      `dynamodbav:"parent_messages"`
	ChannelMessages       int      `dynamodbav:"channel_messages"`
	HistoryRunes          int      `dynamodbav:"history_runes"`
	MaxOutputTokens       int      `dynamodbav:"max_output_tokens"`
	MessageTimeoutSeconds int64    `dynamodbav:"message_timeout_seconds"`
	MessageRetentionDays  int      `dynamodbav:"message_retention_days"`
	WebSearchEnabled      bool     `dynamodbav:"web_search_enabled"`
	ChannelSearchEnabled  bool     `dynamodbav:"channel_search_enabled"`
	AdminUserIDs          []string `dynamodbav:"admin_user_ids,stringset,omitempty"`
	Version               int64    `dynamodbav:"version"`
	UpdatedAtMillis       int64    `dynamodbav:"updated_at"`
	UpdatedByUserID       string   `dynamodbav:"updated_by_user_id"`
}

func newGuildConfigItem(guildID, actorID string, value config.GuildConfig, updatedAt time.Time) guildConfigItem {
	settings := value.Settings
	return guildConfigItem{
		PK: guildPartitionKey(guildID), SK: configSortKey, EntityType: "GUILD_CONFIG", SchemaVersion: guildConfigSchemaVersion,
		Prompt: settings.Prompt, GuildPrompt: settings.GuildPrompt,
		ThreadMessages: settings.ThreadMessages, ParentMessages: settings.ParentMessages,
		ChannelMessages: settings.ChannelMessages, HistoryRunes: settings.HistoryRunes, MaxOutputTokens: settings.MaxOutputTokens,
		MessageTimeoutSeconds: int64(settings.MessageTimeout / time.Second),
		MessageRetentionDays:  settings.MessageRetentionDays, WebSearchEnabled: settings.WebSearchEnabled,
		ChannelSearchEnabled: settings.ChannelSearchEnabled, AdminUserIDs: value.AdminUserIDs, Version: value.Version,
		UpdatedAtMillis: updatedAt.UTC().UnixMilli(), UpdatedByUserID: actorID,
	}
}

func (i guildConfigItem) config() config.GuildConfig {
	return config.GuildConfig{
		Settings: config.ServerSettings{
			Prompt: i.Prompt, GuildPrompt: i.GuildPrompt,
			ThreadMessages: i.ThreadMessages, ParentMessages: i.ParentMessages,
			ChannelMessages: i.ChannelMessages, HistoryRunes: i.HistoryRunes, MaxOutputTokens: i.MaxOutputTokens,
			MessageTimeout:       time.Duration(i.MessageTimeoutSeconds) * time.Second,
			MessageRetentionDays: i.MessageRetentionDays, WebSearchEnabled: i.WebSearchEnabled,
			ChannelSearchEnabled: i.ChannelSearchEnabled,
		},
		AdminUserIDs: normalizedUserIDs(i.AdminUserIDs), Version: i.Version,
	}
}

type messageItem struct {
	PK                 string   `dynamodbav:"pk"`
	SK                 string   `dynamodbav:"sk"`
	EntityType         string   `dynamodbav:"entity_type"`
	SchemaVersion      int      `dynamodbav:"schema_version"`
	MessageID          string   `dynamodbav:"message_id"`
	GuildID            string   `dynamodbav:"guild_id,omitempty"`
	ChannelID          string   `dynamodbav:"channel_id"`
	Compressed         bool     `dynamodbav:"compressed"`
	MessageKind        int32    `dynamodbav:"message_kind"`
	AuthorID           string   `dynamodbav:"author_id"`
	AuthorUsername     string   `dynamodbav:"author_username"`
	AuthorGlobalName   string   `dynamodbav:"author_global_name,omitempty"`
	AuthorBot          bool     `dynamodbav:"author_bot"`
	MentionedUserIDs   []string `dynamodbav:"mentioned_user_ids,omitempty"`
	ReferenceMessageID string   `dynamodbav:"reference_message_id,omitempty"`
	ReferenceChannelID string   `dynamodbav:"reference_channel_id,omitempty"`
	CreatedAtMillis    int64    `dynamodbav:"created_at"`
	IngestedAtMillis   int64    `dynamodbav:"ingested_at"`
	ExpiresAt          int64    `dynamodbav:"expires_at"`
}

func channelPartitionKey(channelID string) string { return "CHANNEL#" + channelID }
func guildPartitionKey(guildID string) string     { return "GUILD#" + guildID }

func messageSortKey(messageID string) string {
	if len(messageID) < messageKeyWidth {
		messageID = strings.Repeat("0", messageKeyWidth-len(messageID)) + messageID
	}
	return "MESSAGE#" + messageID
}

func boolPointer(value bool) *bool { return &value }

func int64String(value int64) string {
	return strconv.FormatInt(value, 10)
}

func normalizedUserIDs(userIDs []string) []string {
	result := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		userID = strings.TrimSpace(userID)
		if userID != "" && !slices.Contains(result, userID) {
			result = append(result, userID)
		}
	}
	slices.Sort(result)
	return result
}

func cloneConfig(value config.GuildConfig) config.GuildConfig {
	value.AdminUserIDs = slices.Clone(value.AdminUserIDs)
	return value
}

func equalConfig(left, right config.GuildConfig) bool {
	left.Version, right.Version = 0, 0
	return left.Settings == right.Settings && slices.Equal(normalizedUserIDs(left.AdminUserIDs), normalizedUserIDs(right.AdminUserIDs))
}
