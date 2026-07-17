package dynamostore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/internal/config"
	"github.com/justinswe/std/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type fakeDynamoClient struct {
	get   func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	put   func(context.Context, *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	query func(context.Context, *dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
}

func (c *fakeDynamoClient) GetItem(ctx context.Context, input *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if c.get == nil {
		return &dynamodb.GetItemOutput{}, nil
	}
	return c.get(ctx, input)
}

func (c *fakeDynamoClient) PutItem(ctx context.Context, input *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if c.put == nil {
		return &dynamodb.PutItemOutput{}, nil
	}
	return c.put(ctx, input)
}

func (c *fakeDynamoClient) Query(ctx context.Context, input *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if c.query == nil {
		return &dynamodb.QueryOutput{}, nil
	}
	return c.query(ctx, input)
}

func repositoryDefaults() config.GuildConfig {
	return config.GuildConfig{Settings: config.ServerSettings{
		Prompt: "Jarvis", ThreadMessages: 15, ParentMessages: 10, ChannelMessages: 4, HistoryRunes: 4000,
		MaxOutputTokens: 256, MessageTimeout: time.Minute,
		MessageRetentionDays: config.DefaultMessageRetentionDays, WebSearchEnabled: true, ChannelSearchEnabled: true,
	}}
}

func messageEvent(content string) *discordv1.DiscordMessageCreateEvent {
	return &discordv1.DiscordMessageCreateEvent{
		MessageId: "123456789012345678", ChannelId: "channel", Content: content,
		Kind: discordv1.MessageKind_MESSAGE_KIND_DEFAULT, Author: &discordv1.MessageAuthor{Id: "123456789012345679", Username: "alice"},
		IngestedAt: timestamppb.New(time.Unix(1000, 0).UTC()),
	}
}

func TestRecordCompressesContentOverOneHundredBytes(t *testing.T) {
	incompressible := pseudoRandomASCII(200)
	for _, test := range []struct {
		name       string
		content    string
		compressed bool
	}{
		{name: "one hundred bytes", content: strings.Repeat("a", 100)},
		{name: "one hundred one bytes", content: strings.Repeat("a", 101), compressed: true},
		{name: "UTF-8 bytes", content: strings.Repeat("é", 51), compressed: true},
		{name: "compression is not beneficial", content: incompressible},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stored map[string]dynamodbtypes.AttributeValue
			client := &fakeDynamoClient{put: func(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				stored = input.Item
				assert.Equal(t, "attribute_not_exists(pk)", *input.ConditionExpression)
				return &dynamodb.PutItemOutput{}, nil
			}}
			repository, err := New(client, "table", repositoryDefaults())
			require.NoError(t, err)
			defer repository.Close()

			require.NoError(t, repository.Record(context.Background(), messageEvent(test.content)))
			var item messageItem
			require.NoError(t, attributevalue.UnmarshalMap(stored, &item))
			assert.Equal(t, test.compressed, item.Compressed)
			assert.Equal(t, messageSchemaVersion, item.SchemaVersion)
			if test.compressed {
				binary, ok := stored["content"].(*dynamodbtypes.AttributeValueMemberB)
				require.True(t, ok)
				assert.Less(t, len(binary.Value), len([]byte(test.content)))
			} else {
				text, ok := stored["content"].(*dynamodbtypes.AttributeValueMemberS)
				require.True(t, ok)
				assert.Equal(t, test.content, text.Value)
			}
			decoded, err := repository.decodeMessage(stored)
			require.NoError(t, err)
			assert.Equal(t, test.content, decoded.Content)
			assert.Equal(t, time.Unix(1000, 0).Add(30*24*time.Hour).Unix(), item.ExpiresAt)
		})
	}
}

func TestDecodeMessageSupportsLegacyBinaryContent(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	for _, compressed := range []bool{false, true} {
		item, _ := repository.messageItem(messageEvent("legacy content"), 30)
		item.SchemaVersion = legacyMessageSchemaVersion
		item.Compressed = compressed
		content := []byte("legacy content")
		if compressed {
			content = repository.encoder.EncodeAll(content, nil)
		}
		attributes := marshalMessageAttributes(t, item, &dynamodbtypes.AttributeValueMemberB{Value: content})
		decoded, err := repository.decodeMessage(attributes)
		require.NoError(t, err)
		assert.Equal(t, "legacy content", decoded.Content)
	}
}

func TestDecodeMessageRejectsInvalidVersionTwoContentTypes(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()
	item, _ := repository.messageItem(messageEvent("content"), 30)

	item.Compressed = true
	_, err = repository.decodeMessage(marshalMessageAttributes(t, item, &dynamodbtypes.AttributeValueMemberS{Value: "not binary"}))
	assert.ErrorContains(t, err, "must be binary")

	item.Compressed = false
	_, err = repository.decodeMessage(marshalMessageAttributes(t, item, &dynamodbtypes.AttributeValueMemberB{Value: []byte("not a string")}))
	assert.ErrorContains(t, err, "must be a string")
}

func TestRecordUsesGuildRetentionAndTreatsDuplicateAsSuccess(t *testing.T) {
	storedConfig := repositoryDefaults()
	storedConfig.Settings.MessageRetentionDays = 90
	storedConfig.Version = 1
	configAttributes, err := attributevalue.MarshalMap(newGuildConfigItem("guild", "admin", storedConfig, time.Unix(1, 0)))
	require.NoError(t, err)
	client := &fakeDynamoClient{
		get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: configAttributes}, nil
		},
		put: func(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
			var item messageItem
			require.NoError(t, attributevalue.UnmarshalMap(input.Item, &item))
			assert.Equal(t, time.Unix(1000, 0).Add(90*24*time.Hour).Unix(), item.ExpiresAt)
			return nil, &dynamodbtypes.ConditionalCheckFailedException{}
		},
	}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()
	event := messageEvent("hello")
	event.GuildId = "guild"
	assert.NoError(t, repository.Record(context.Background(), event))
}

func TestMessagesReturnsPartialDecodedHistory(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()
	repository.now = func() time.Time { return time.Unix(2000, 0) }

	good, goodContent := repository.messageItem(messageEvent(strings.Repeat("a", 101)), 30)
	good.GuildID = "guild"
	goodAttributes := marshalMessageAttributes(t, good, goodContent)
	bad := good
	bad.MessageID = "123456789012345677"
	bad.SK = messageSortKey(bad.MessageID)
	badAttributes := marshalMessageAttributes(t, bad, &dynamodbtypes.AttributeValueMemberB{Value: []byte("not zstd")})
	repository.client = &fakeDynamoClient{query: func(_ context.Context, input *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		assert.True(t, *input.ConsistentRead)
		assert.False(t, *input.ScanIndexForward)
		assert.Contains(t, *input.FilterExpression, "expires_at")
		return &dynamodb.QueryOutput{Items: []map[string]dynamodbtypes.AttributeValue{goodAttributes, badAttributes}}, nil
	}}

	messages, err := repository.Messages(context.Background(), "guild", "channel", 2, "123456789012345680")
	assert.ErrorContains(t, err, "decompress")
	require.Len(t, messages, 1)
	assert.Equal(t, strings.Repeat("a", 101), messages[0].Content)
}

func TestMessagesUsesExclusiveBeforeCursorAcrossSearchPages(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()
	repository.now = func() time.Time { return time.Unix(2000, 0) }

	attributes := func(messageID string) map[string]dynamodbtypes.AttributeValue {
		event := messageEvent("deploy")
		event.MessageId = messageID
		event.GuildId = "guild"
		item, content := repository.messageItem(event, 30)
		return marshalMessageAttributes(t, item, content)
	}
	var beforeValues []string
	repository.client = &fakeDynamoClient{query: func(_ context.Context, input *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
		before := input.ExpressionAttributeValues[":before"].(*dynamodbtypes.AttributeValueMemberS).Value
		beforeValues = append(beforeValues, before)
		switch before {
		case messageSortKey("123456789012345680"):
			return &dynamodb.QueryOutput{Items: []map[string]dynamodbtypes.AttributeValue{attributes("123456789012345679")}}, nil
		case messageSortKey("123456789012345679"):
			return &dynamodb.QueryOutput{Items: []map[string]dynamodbtypes.AttributeValue{attributes("123456789012345678")}}, nil
		default:
			return &dynamodb.QueryOutput{}, nil
		}
	}}

	newer, err := repository.Messages(context.Background(), "guild", "channel", 1, "123456789012345680")
	require.NoError(t, err)
	require.Len(t, newer, 1)
	older, err := repository.Messages(context.Background(), "guild", "channel", 1, newer[0].ID)
	require.NoError(t, err)
	require.Len(t, older, 1)
	assert.Equal(t, "123456789012345678", older[0].ID)
	assert.Equal(t, []string{messageSortKey("123456789012345680"), messageSortKey("123456789012345679")}, beforeValues)
}

func TestConfigurationMutationMaterializesDefaultsAndDelegates(t *testing.T) {
	var stored map[string]dynamodbtypes.AttributeValue
	client := &fakeDynamoClient{}
	client.get = func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: stored}, nil
	}
	client.put = func(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		stored = input.Item
		return &dynamodb.PutItemOutput{}, nil
	}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	prompt := "New Jarvis"
	guildPrompt := "Use guild terminology."
	updated, err := repository.Update(context.Background(), "guild", "actor", config.Patch{Prompt: &prompt, GuildPrompt: &guildPrompt})
	require.NoError(t, err)
	assert.Equal(t, int64(1), updated.Version)
	assert.Equal(t, prompt, updated.Settings.Prompt)
	assert.Equal(t, guildPrompt, updated.Settings.GuildPrompt)
	assert.Equal(t, repositoryDefaults().Settings.MessageRetentionDays, updated.Settings.MessageRetentionDays)

	updated, err = repository.AddAdmin(context.Background(), "guild", "actor", "123456789012345678")
	require.NoError(t, err)
	assert.Equal(t, int64(2), updated.Version)
	assert.Equal(t, []string{"123456789012345678"}, updated.AdminUserIDs)
	assert.Equal(t, guildPrompt, updated.Settings.GuildPrompt)
	unchanged, err := repository.AddAdmin(context.Background(), "guild", "actor", "123456789012345678")
	require.NoError(t, err)
	assert.Equal(t, updated, unchanged)

	removed, err := repository.RemoveAdmin(context.Background(), "guild", "actor", "123456789012345678")
	require.NoError(t, err)
	assert.Empty(t, removed.AdminUserIDs)
	assert.Equal(t, guildPrompt, removed.Settings.GuildPrompt)
	assert.Equal(t, int64(3), removed.Version)
}

func TestConfigurationPersistsGuildPromptAndLoadsMissingField(t *testing.T) {
	value := repositoryDefaults()
	value.Settings.GuildPrompt = "Use the guild vocabulary."
	attributes, err := attributevalue.MarshalMap(newGuildConfigItem("guild", "admin", value, time.Unix(1, 0)))
	require.NoError(t, err)
	client := &fakeDynamoClient{get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: attributes}, nil
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	loaded, err := repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Equal(t, value.Settings.GuildPrompt, loaded.Settings.GuildPrompt)

	delete(attributes, "guild_prompt")
	loaded, err = repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Empty(t, loaded.Settings.GuildPrompt)
}

func TestConfigurationIgnoresLegacyTemperatureAttribute(t *testing.T) {
	value := repositoryDefaults()
	value.Version = 3
	attributes, err := attributevalue.MarshalMap(newGuildConfigItem("guild", "admin", value, time.Unix(1, 0)))
	require.NoError(t, err)
	assert.NotContains(t, attributes, "temperature")
	attributes["temperature"] = &dynamodbtypes.AttributeValueMemberN{Value: "1.4"}
	client := &fakeDynamoClient{get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: attributes}, nil
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	loaded, err := repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Equal(t, value.Settings, loaded.Settings)
	assert.Equal(t, value.Version, loaded.Version)
}

func TestConfigurationUpdateDropsLegacyTemperatureAttribute(t *testing.T) {
	value := repositoryDefaults()
	value.Version = 1
	attributes, err := attributevalue.MarshalMap(newGuildConfigItem("guild", "admin", value, time.Unix(1, 0)))
	require.NoError(t, err)
	attributes["temperature"] = &dynamodbtypes.AttributeValueMemberN{Value: "1.4"}
	client := &fakeDynamoClient{
		get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: attributes}, nil
		},
		put: func(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
			assert.NotContains(t, input.Item, "temperature")
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	prompt := "Updated"
	updated, err := repository.Update(context.Background(), "guild", "actor", config.Patch{Prompt: &prompt})
	require.NoError(t, err)
	assert.Equal(t, prompt, updated.Settings.Prompt)
}

func TestConfigurationProviderFailsOpenButManagerDoesNot(t *testing.T) {
	wantErr := errors.New("unavailable")
	repository, err := New(&fakeDynamoClient{get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return nil, wantErr
	}}, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	got, err := repository.Get(context.Background(), "guild")
	require.NoError(t, err)
	assert.Equal(t, repositoryDefaults(), got)
	_, err = repository.Load(context.Background(), "guild")
	assert.ErrorIs(t, err, wantErr)
}

func TestConfigurationConflictIsBounded(t *testing.T) {
	client := &fakeDynamoClient{put: func(context.Context, *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()
	prompt := "new"
	_, err = repository.Update(context.Background(), "guild", "actor", config.Patch{Prompt: &prompt})
	assert.ErrorContains(t, err, "concurrently")
}

func marshalMessageAttributes(t *testing.T, item messageItem, content dynamodbtypes.AttributeValue) map[string]dynamodbtypes.AttributeValue {
	t.Helper()
	attributes, err := attributevalue.MarshalMap(item)
	require.NoError(t, err)
	attributes["content"] = content
	return attributes
}

func pseudoRandomASCII(length int) string {
	content := make([]byte, length)
	state := uint32(0x9e3779b9)
	for i := range content {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		content[i] = byte(32 + state%95)
	}
	return string(content)
}

func BenchmarkContentEncoding(b *testing.B) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, repository.Close()) })

	for _, benchmark := range []struct {
		name    string
		content string
	}{
		{name: "short", content: "hello from Discord"},
		{name: "natural language", content: strings.Repeat("Jarvis summarizes the current Discord conversation clearly. ", 20)},
		{name: "repetitive", content: strings.Repeat("compress me ", 200)},
		{name: "unicode", content: strings.Repeat("こんにちは世界 ", 100)},
		{name: "high entropy", content: pseudoRandomASCII(2000)},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			rawBytes := len([]byte(benchmark.content))
			storedBytes := 0
			for i := 0; i < b.N; i++ {
				attribute, _ := repository.encodeContent(benchmark.content)
				switch value := attribute.(type) {
				case *dynamodbtypes.AttributeValueMemberS:
					storedBytes = len([]byte(value.Value))
				case *dynamodbtypes.AttributeValueMemberB:
					storedBytes = len(value.Value)
				}
			}
			b.ReportMetric(float64(rawBytes), "raw_bytes")
			b.ReportMetric(float64(storedBytes), "stored_bytes")
			b.ReportMetric(float64(storedBytes)/float64(rawBytes), "storage_ratio")
		})
	}
}
