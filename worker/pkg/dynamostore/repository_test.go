package dynamostore

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	discordv1 "github.com/justinswe/jarvis/api/jarvis/discord/v1"
	"github.com/justinswe/jarvis/worker/pkg/config"
	"github.com/justinswe/jarvis/worker/pkg/llm"
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
		ReasoningEffort:     llm.ReasoningMedium,
		PrimaryModelProfile: "default", FallbackModelProfile: "fallback",
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
			assert.Equal(t, time.Unix(1000, 0).Add(time.Duration(config.DefaultMessageRetentionDays)*24*time.Hour).Unix(), item.ExpiresAt)
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

type stubRetentionProvider struct {
	calls  int
	config config.GuildConfig
}

func (p *stubRetentionProvider) Get(context.Context, string) (config.GuildConfig, error) {
	p.calls++
	return p.config, nil
}

func TestRecordUsesTheInjectedRetentionLookupWhenSet(t *testing.T) {
	getCalls := 0
	client := &fakeDynamoClient{
		get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			getCalls++
			return &dynamodb.GetItemOutput{}, nil
		},
		put: func(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
			var item messageItem
			require.NoError(t, attributevalue.UnmarshalMap(input.Item, &item))
			assert.Equal(t, time.Unix(1000, 0).Add(90*24*time.Hour).Unix(), item.ExpiresAt)
			return &dynamodb.PutItemOutput{}, nil
		},
	}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	injectedConfig := repositoryDefaults()
	injectedConfig.Settings.MessageRetentionDays = 90
	provider := &stubRetentionProvider{config: injectedConfig}
	repository.SetRetentionLookup(provider)

	event := messageEvent("hello")
	event.GuildId = "guild"
	require.NoError(t, repository.Record(context.Background(), event))

	assert.Equal(t, 1, provider.calls, "Record must use the injected retention lookup")
	assert.Zero(t, getCalls, "an injected retention lookup must bypass DynamoDB entirely")
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

func TestConfigurationV1InheritsModelProfileDefaults(t *testing.T) {
	value := repositoryDefaults()
	attributes, err := attributevalue.MarshalMap(newGuildConfigItem("guild", "admin", value, time.Unix(1, 0)))
	require.NoError(t, err)
	attributes["schema_version"] = &dynamodbtypes.AttributeValueMemberN{Value: "1"}
	delete(attributes, "primary_model_profile")
	delete(attributes, "fallback_model_profile")
	client := &fakeDynamoClient{get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: attributes}, nil
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	loaded, err := repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Equal(t, "default", loaded.Settings.PrimaryModelProfile)
	assert.Equal(t, "fallback", loaded.Settings.FallbackModelProfile)
}

func TestConfigurationV2RoundTripsModelProfiles(t *testing.T) {
	value := repositoryDefaults()
	value.Settings.PrimaryModelProfile = "quality"
	value.Settings.FallbackModelProfile = ""
	item := newGuildConfigItem("guild", "admin", value, time.Unix(1, 0))
	assert.Equal(t, guildConfigSchemaVersion, item.SchemaVersion)
	attributes, err := attributevalue.MarshalMap(item)
	require.NoError(t, err)
	client := &fakeDynamoClient{get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: attributes}, nil
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	loaded, err := repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Equal(t, "quality", loaded.Settings.PrimaryModelProfile)
	assert.Empty(t, loaded.Settings.FallbackModelProfile)
}

func TestConfigurationRoundTripsReasoningEffortAndDefaultsMissingAttribute(t *testing.T) {
	value := repositoryDefaults()
	value.Settings.ReasoningEffort = llm.ReasoningHigh
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
	assert.Equal(t, llm.ReasoningHigh, loaded.Settings.ReasoningEffort)

	delete(attributes, "reasoning_effort")
	loaded, err = repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Equal(t, llm.ReasoningMedium, loaded.Settings.ReasoningEffort)
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

func TestSetTierPersistsAndIsIdempotent(t *testing.T) {
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

	updated, err := repository.SetTier(context.Background(), "guild", "actor", "pro")
	require.NoError(t, err)
	assert.Equal(t, "pro", updated.Tier)
	assert.Equal(t, int64(1), updated.Version)

	loaded, err := repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Equal(t, "pro", loaded.Tier)

	// A repeated assignment must not burn a version, and a real change must.
	unchanged, err := repository.SetTier(context.Background(), "guild", "actor", "pro")
	require.NoError(t, err)
	assert.Equal(t, int64(1), unchanged.Version)

	changed, err := repository.SetTier(context.Background(), "guild", "actor", "free")
	require.NoError(t, err)
	assert.Equal(t, "free", changed.Tier)
	assert.Equal(t, int64(2), changed.Version)
}

func TestSetTierRejectsMalformedTierNames(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	_, err = repository.SetTier(context.Background(), "guild", "actor", "Not A Tier")
	assert.Error(t, err)
}

// mutateConfig short-circuits on equalConfig, so a tier-only change must be visible to it
// or SetTier silently reports success without writing anything.
func TestEqualConfigDetectsTierOnlyChanges(t *testing.T) {
	left := repositoryDefaults()
	right := repositoryDefaults()
	assert.True(t, equalConfig(left, right))

	right.Tier = "pro"
	assert.False(t, equalConfig(left, right), "a tier-only change must not be treated as a no-op")
}

func TestConfigurationWithoutStoredTierLoadsEmptyAndKeepsModelProfiles(t *testing.T) {
	value := repositoryDefaults()
	attributes, err := attributevalue.MarshalMap(newGuildConfigItem("guild", "admin", value, time.Unix(1, 0)))
	require.NoError(t, err)
	_, present := attributes["tier"]
	assert.False(t, present, "an empty tier must not be written")

	client := &fakeDynamoClient{get: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
		return &dynamodb.GetItemOutput{Item: attributes}, nil
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	defer repository.Close()

	loaded, err := repository.Load(context.Background(), "guild")
	require.NoError(t, err)
	assert.Empty(t, loaded.Tier)
	assert.Equal(t, value.Settings.PrimaryModelProfile, loaded.Settings.PrimaryModelProfile,
		"adding the tier attribute must not have bumped the schema version")
}

// claimTable is a DynamoDB stand-in that evaluates the reply claim's condition the way
// the service does, so the tests exercise the expression rather than a paraphrase of it.
type claimTable struct {
	items map[string]map[string]dynamodbtypes.AttributeValue
}

func newClaimTable() *claimTable {
	return &claimTable{items: map[string]map[string]dynamodbtypes.AttributeValue{}}
}

func (c *claimTable) put(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
	key := attributeString(input.Item["pk"]) + "|" + attributeString(input.Item["sk"])
	existing, held := c.items[key]
	// An unconditional put overwrites whatever is there, which is what HoldReply relies on.
	if input.ConditionExpression != nil && held && !c.wins(existing, input) {
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}
	c.items[key] = input.Item
	return &dynamodb.PutItemOutput{}, nil
}

// wins evaluates the one arm that can beat an existing claim: it has lapsed.
// attribute_not_exists is the caller's `held` check.
//
// The comparison parses rather than comparing the two attribute strings, because DynamoDB
// compares N attributes numerically and a lexicographic stand-in disagrees the moment the
// two timestamps differ in width — which would make this fake pass a condition the service
// rejects.
func (c *claimTable) wins(existing map[string]dynamodbtypes.AttributeValue, input *dynamodb.PutItemInput) bool {
	expires, err := strconv.ParseInt(attributeNumber(existing["expires_at"]), 10, 64)
	if err != nil {
		return false
	}
	now, err := strconv.ParseInt(attributeNumber(input.ExpressionAttributeValues[":now"]), 10, 64)
	if err != nil {
		return false
	}
	return expires <= now
}

func TestClaimReplyWinsOnceAndLosesToOtherWorkers(t *testing.T) {
	table := newClaimTable()
	client := &fakeDynamoClient{put: table.put}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	other, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	other.replyOwner = "other-site/1"

	won, err := repository.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	assert.True(t, won)

	// The other site's copy of the same Discord message must not produce a second reply.
	won, err = other.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	assert.False(t, won)

	// A different message in the same channel is unaffected.
	won, err = other.ClaimReply(t.Context(), "channel", "123456789012345679")
	require.NoError(t, err)
	assert.True(t, won)
}

// TestClaimReplyRefusesASecondCopyDeliveredToTheSameWorker is the duplicate the claim
// exists to stop, and the one an owner arm in the condition would let through.
//
// Every site's workers share one subscription, so the two copies of a Discord message are
// load-balanced independently and land on the same process roughly one time in however
// many workers are running. A claim keyed on the process rather than the message would
// match itself and answer twice.
func TestClaimReplyRefusesASecondCopyDeliveredToTheSameWorker(t *testing.T) {
	table := newClaimTable()
	repository, err := New(&fakeDynamoClient{put: table.put}, "table", repositoryDefaults())
	require.NoError(t, err)

	won, err := repository.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	require.True(t, won)

	won, err = repository.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	assert.False(t, won, "one worker holding both copies must still answer once")
}

func TestClaimReplyRetakesALapsedClaim(t *testing.T) {
	table := newClaimTable()
	client := &fakeDynamoClient{put: table.put}
	dead, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	claimed := time.Unix(1000, 0).UTC()
	dead.now = func() time.Time { return claimed }
	dead.SetReplyClaimTTL(30 * time.Second)

	won, err := dead.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	require.True(t, won)

	// That worker died mid-generation. Once its claim lapses the redelivery must be able
	// to answer, or the reply is lost rather than merely delayed.
	survivor, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	survivor.replyOwner = "survivor/1"
	survivor.now = func() time.Time { return claimed.Add(31 * time.Second) }

	won, err = survivor.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	assert.True(t, won)
}

func TestClaimReplyWritesAnExpiringClaimUnderTheMessageChannel(t *testing.T) {
	var item map[string]dynamodbtypes.AttributeValue
	var names map[string]string
	var values map[string]dynamodbtypes.AttributeValue
	client := &fakeDynamoClient{put: func(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		item, names, values = input.Item, input.ExpressionAttributeNames, input.ExpressionAttributeValues
		return &dynamodb.PutItemOutput{}, nil
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	now := time.Unix(1000, 0).UTC()
	repository.now = func() time.Time { return now }
	repository.SetReplyClaimTTL(45 * time.Second)

	won, err := repository.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	require.True(t, won)

	assert.Equal(t, "CHANNEL#channel", attributeString(item["pk"]))
	assert.Equal(t, "REPLY#123456789012345678", attributeString(item["sk"]))
	assert.Equal(t, replyClaimEntityType, attributeString(item["entity_type"]))
	assert.NotEmpty(t, attributeString(item["owner"]))
	// The claim expires rather than accumulating forever, and does so in time for the
	// redelivery a dead claimant would otherwise block.
	assert.Equal(t, int64String(now.Add(45*time.Second).Unix()), attributeNumber(item["expires_at"]))
	// The condition must turn on the message alone. Matching the owner would let the
	// second copy of one message, delivered to this same process, beat its own claim.
	assert.Empty(t, names, "the claim must not reference the owner attribute")
	assert.NotContains(t, values, ":owner")
}

// TestHoldReplyOutlastsEveryDeliverableCopy is the other half of the claim. ClaimReply is
// short so a crash releases the message; that same shortness lets a copy delayed past the
// acknowledgement wait answer a message someone has already replied to. Extending once the
// reply exists is what closes it, so the held claim must outlast the queue's retention.
func TestHoldReplyOutlastsEveryDeliverableCopy(t *testing.T) {
	var item map[string]dynamodbtypes.AttributeValue
	var conditional bool
	client := &fakeDynamoClient{put: func(_ context.Context, input *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		item, conditional = input.Item, input.ConditionExpression != nil
		return &dynamodb.PutItemOutput{}, nil
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	now := time.Unix(1000, 0).UTC()
	repository.now = func() time.Time { return now }
	repository.SetReplyClaimTTL(30 * time.Second)

	require.NoError(t, repository.HoldReply(t.Context(), "channel", "123456789012345678"))

	assert.Equal(t, "CHANNEL#channel", attributeString(item["pk"]))
	assert.Equal(t, "REPLY#123456789012345678", attributeString(item["sk"]))
	assert.Equal(t, int64String(now.Add(discordv1.MaxMessageAge).Unix()), attributeNumber(item["expires_at"]),
		"a held claim must outlive the longest a copy can sit in the queue")
	assert.False(t, conditional, "the reply is already posted; nothing may refuse to record it")
}

// TestHoldReplyRefusesToTakeAClaimItCannotAddress guards the same identifiers ClaimReply
// does: a claim written under an empty key would suppress nothing and hide that it had.
func TestHoldReplyRefusesToTakeAClaimItCannotAddress(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)

	assert.Error(t, repository.HoldReply(t.Context(), "", "123456789012345678"))
	assert.Error(t, repository.HoldReply(t.Context(), "channel", " "))
}

// TestClaimReplyRefusesAHeldMessage is the pair of properties in one place: the extension
// really does lock out the late copy, and it does so for every worker.
func TestClaimReplyRefusesAHeldMessage(t *testing.T) {
	table := newClaimTable()
	client := &fakeDynamoClient{put: table.put}
	winner, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	claimed := time.Unix(1000, 0).UTC()
	winner.now = func() time.Time { return claimed }

	won, err := winner.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, winner.HoldReply(t.Context(), "channel", "123456789012345678"))

	// The other site's copy, delivered long after the original claim would have lapsed.
	late, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)
	late.replyOwner = "late-site/1"
	late.now = func() time.Time { return claimed.Add(5 * time.Minute) }

	won, err = late.ClaimReply(t.Context(), "channel", "123456789012345678")
	require.NoError(t, err)
	assert.False(t, won, "a message that has been answered must stay answered")
}

func TestSetReplyClaimTTLIgnoresNonPositiveValues(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)

	repository.SetReplyClaimTTL(0)
	assert.Equal(t, defaultReplyClaimTTL, repository.replyClaimTTL,
		"a claim that never expires would strand every redelivery")
	repository.SetReplyClaimTTL(-time.Second)
	assert.Equal(t, defaultReplyClaimTTL, repository.replyClaimTTL)
}

func TestClaimReplyRequiresBothIdentifiers(t *testing.T) {
	repository, err := New(&fakeDynamoClient{}, "table", repositoryDefaults())
	require.NoError(t, err)

	_, err = repository.ClaimReply(t.Context(), "", "123456789012345678")
	assert.Error(t, err)
	_, err = repository.ClaimReply(t.Context(), "channel", " ")
	assert.Error(t, err)
}

func TestClaimReplySurfacesUnexpectedErrors(t *testing.T) {
	client := &fakeDynamoClient{put: func(context.Context, *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
		return nil, errors.New("throttled")
	}}
	repository, err := New(client, "table", repositoryDefaults())
	require.NoError(t, err)

	won, err := repository.ClaimReply(t.Context(), "channel", "123456789012345678")
	assert.Error(t, err, "the caller decides how to handle a claim it could not take")
	assert.False(t, won)
}

func attributeString(value dynamodbtypes.AttributeValue) string {
	member, ok := value.(*dynamodbtypes.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return member.Value
}

func attributeNumber(value dynamodbtypes.AttributeValue) string {
	member, ok := value.(*dynamodbtypes.AttributeValueMemberN)
	if !ok {
		return ""
	}
	return member.Value
}
